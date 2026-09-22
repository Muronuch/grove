package envctl

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/state"
)

const (
	SourceExact      = "exact"
	SourceGolden     = "golden"
	SourceScratch    = "scratch"
	SourceCheckpoint = "checkpoint"
	SourceKept       = "kept"
)

func (c *Controller) ProvisionState(ctx context.Context, t *Target) error {
	if c.Driver == nil || len(t.Config.Stateful) == 0 {
		return nil
	}
	refs := map[string]registry.SnapshotRef{}
	for _, st := range t.Config.Stateful {
		ref, source, err := c.provisionOne(ctx, t, st)
		if err != nil {
			return err
		}
		refs[st.Service] = registry.SnapshotRef{Key: ref.Key, Source: source}
	}
	return c.recordSnapshots(ctx, t, refs)
}

func (c *Controller) provisionOne(ctx context.Context, t *Target, st config.StatefulConfig) (state.Ref, string, error) {
	target, err := c.volumeTargetFor(ctx, t, st)
	if err != nil {
		return state.Ref{}, "", err
	}

	envKey, err := c.snapshotKey(ctx, t, st, t.Entry.Worktree)
	if err != nil {
		return state.Ref{}, "", err
	}
	if ok, err := c.Driver.Exists(ctx, envKey); err != nil {
		return state.Ref{}, "", err
	} else if ok {
		if err := c.cloneInto(ctx, envKey, target, SourceExact); err != nil {
			return state.Ref{}, "", err
		}
		return envKey, SourceExact, nil
	}

	golden, err := c.EnsureGolden(ctx, t, st)
	switch {
	case err == nil:
		if err := c.cloneInto(ctx, golden, target, SourceGolden); err != nil {
			return state.Ref{}, "", err
		}
		return golden, SourceGolden, nil
	case errors.Is(err, errNoGolden):
		c.detail("no golden snapshot for %s: %v", st.Service, err)
	default:

		c.step("could not build the golden snapshot for %s (%v); starting from an empty database", st.Service, shortError(err))
	}

	if err := c.Runtime.CreateVolume(ctx, engine.VolumeSpec{Name: target.Name, Labels: target.Labels()}); err != nil {
		return state.Ref{}, "", err
	}
	return envKey, SourceScratch, nil
}

func (c *Controller) cloneInto(ctx context.Context, ref state.Ref, target state.VolumeTarget, source string) error {
	c.step("cloning the %s snapshot of %s (%s)", source, ref.Service, ref.Short())
	stats, err := c.Driver.Clone(ctx, ref, target)
	if err != nil {
		return err
	}
	c.detail("copied %s in %s%s", humanSize(stats.Bytes), stats.Duration.Round(time.Millisecond), rateSuffix(stats))
	c.warnIfLarge(stats.Bytes, ref.Service)
	return c.Index.Update(ctx, func(idx *state.Index) error {
		idx.Used(ref)
		return nil
	})
}

var errNoGolden = errors.New("no golden can be built")

func (c *Controller) EnsureGolden(ctx context.Context, t *Target, st config.StatefulConfig) (state.Ref, error) {
	if !st.Prepare.Defined() {
		return state.Ref{}, fmt.Errorf("%w: %s has no prepare command", errNoGolden, st.Service)
	}
	ref, err := c.snapshotKey(ctx, t, st, t.Project.Root)
	if err != nil {
		return state.Ref{}, err
	}
	if ok, err := c.Driver.Exists(ctx, ref); err != nil {
		return state.Ref{}, err
	} else if ok {
		return ref, nil
	}

	lock, err := c.Store.SnapshotLock(ref.Key)
	if err != nil {
		return state.Ref{}, err
	}
	if err := lock.Acquire(ctx, 60*time.Minute); err != nil {
		return state.Ref{}, err
	}
	defer lock.Release()

	if ok, err := c.Driver.Exists(ctx, ref); err != nil {
		return state.Ref{}, err
	} else if ok {
		c.detail("golden %s was built by another process", ref.Short())
		return ref, nil
	}
	return ref, c.buildGolden(ctx, t, st, ref)
}

func (c *Controller) buildGolden(ctx context.Context, t *Target, st config.StatefulConfig, ref state.Ref) error {
	start := time.Now()
	slug := "golden-" + ref.Short()[:8]
	c.step("building the golden snapshot for %s (%s)", st.Service, ref.Short())

	tmp := &Target{
		Project: t.Project,
		Config:  t.Config,
		Entry: &registry.Env{
			Slug: slug, Project: t.Entry.Project, Slot: 0,
			Worktree: t.Project.Root, State: registry.StateCreating, Headless: true,
		},
	}

	only := []string{st.Service}
	if st.Prepare.Service != "" && st.Prepare.Mode != config.PrepareHost {
		only = append(only, st.Prepare.Service)
	}
	stack, err := c.Prepare(ctx, tmp, PrepareOptions{
		Headless:   true,
		Only:       only,
		WorkingDir: t.Project.Root,
		Unrouted:   true,
	})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := stack.Compose.Down(cleanup, true); err != nil {
			c.detail("removing the golden build env: %v", err)
		}
	}()

	if err := c.ensureExternalVolumes(ctx, tmp, stack); err != nil {
		return err
	}
	if err := stack.Compose.Up(ctx, t.Config.Up.ShouldBuild()); err != nil {
		return err
	}
	if err := c.WaitHealthy(ctx, tmp, stack, t.Config.Up.Timeout.Duration()); err != nil {
		return err
	}

	results, err := c.runPrepareOn(ctx, tmp, stack)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.ExitCode != 0 {
			return fmt.Errorf("prepare for %s exited with code %d while building the golden snapshot:\n%s",
				r.Service, r.ExitCode, indent(engine.LastLines(r.Output, 30), "  "))
		}
	}

	c.detail("stopping %s cleanly (grace %s)", st.Service, st.StopGrace)
	if err := stack.Compose.Stop(ctx, st.StopGrace.Duration(), st.Service); err != nil {
		return err
	}
	if err := c.assertCleanExit(ctx, tmp, st.Service); err != nil {
		return err
	}

	volume := meta.ComposeVolume(stack.Model.Name, st.Volume)
	stats, err := c.Driver.Save(ctx, volume, ref)
	if err != nil {
		return err
	}
	c.step("golden %s ready: %s in %s", ref.Short(), humanSize(stats.Bytes), time.Since(start).Round(time.Second))
	c.warnIfLarge(stats.Bytes, st.Service)

	commit, _ := t.Project.Git().Resolve(ctx, "HEAD")
	keyRes, _ := c.keyResult(ctx, t, st, t.Project.Root)
	return c.Index.Update(ctx, func(idx *state.Index) error {
		idx.Put(state.Entry{
			Key: ref.Key, Project: ref.Project, Service: ref.Service, Volume: ref.Volume(),
			Kind: state.KindGolden, SizeBytes: stats.Bytes, BuiltFrom: commit,
			BuildDuration: time.Since(start), Inputs: len(keyRes.Files),
		})
		return nil
	})
}

func (c *Controller) assertCleanExit(ctx context.Context, t *Target, service string) error {
	cs, err := c.Runtime.Containers(ctx, engine.Selector{
		All: true,
		Labels: map[string]string{
			meta.LabelManaged:            "true",
			meta.LabelProject:            t.Entry.Project,
			meta.LabelEnv:                t.Entry.Slug,
			"com.docker.compose.service": service,
		},
	})
	if err != nil {
		return err
	}
	for _, ct := range cs {
		full, err := c.Runtime.Container(ctx, ct.ID)
		if err != nil {
			return err
		}
		if full.State == engine.StateExited && full.ExitCode != 0 {
			logs := c.tailLogs(ctx, ct.ID, 20)
			return fmt.Errorf("%s did not shut down cleanly (exit %d); refusing to snapshot a possibly corrupt data directory\n%s",
				service, full.ExitCode, indent(logs, "  "))
		}
	}
	return nil
}

func (c *Controller) snapshotKey(ctx context.Context, t *Target, st config.StatefulConfig, worktree string) (state.Ref, error) {
	res, err := c.keyResult(ctx, t, st, worktree)
	if err != nil {
		return state.Ref{}, err
	}
	return state.Ref{Project: t.Entry.Project, Service: st.Service, Key: res.Key}, nil
}

func (c *Controller) keyResult(ctx context.Context, t *Target, st config.StatefulConfig, worktree string) (state.KeyResult, error) {
	image, err := c.statefulImageID(ctx, t, st, worktree)
	if err != nil {
		return state.KeyResult{}, err
	}
	return state.ComputeKey(state.KeyInput{
		Driver:   c.Driver.Name(),
		Image:    image,
		Prepare:  st.Prepare,
		Worktree: worktree,
		Inputs:   st.Inputs,
	})
}

func (c *Controller) statefulImageID(ctx context.Context, t *Target, st config.StatefulConfig, worktree string) (string, error) {
	stack, err := c.Prepare(ctx, t, PrepareOptions{
		WorkingDir: worktree,
		Unrouted:   true,
		Slug:       t.Entry.Slug,
		Slot:       t.Entry.Slot,
	})
	if err != nil {
		return "", err
	}
	svc, ok := stack.Model.Services[st.Service]
	if !ok {
		return "", fmt.Errorf("compose service %q is not part of this project", st.Service)
	}
	if svc.Image == "" {
		if svc.Build != nil {
			res, err := state.ComputeKey(state.KeyInput{
				Driver:   "build-context",
				Worktree: svc.Build.Context,
				Inputs:   []string{"**"},
			})
			if err != nil {
				return "", err
			}
			return "build:" + res.Key, nil
		}
		return "unknown", nil
	}
	id, err := c.Runtime.ImageID(ctx, svc.Image)
	if err != nil {
		c.detail("could not resolve image %s (%v); keying on its reference", svc.Image, shortError(err))
		return "ref:" + svc.Image, nil
	}
	return id, nil
}

func (c *Controller) volumeTargetFor(ctx context.Context, t *Target, st config.StatefulConfig) (state.VolumeTarget, error) {
	composeProject := t.ComposeProject()
	return state.VolumeTarget{
		Name:           meta.ComposeVolume(composeProject, st.Volume),
		ComposeProject: composeProject,
		ComposeVolume:  st.Volume,
		Project:        t.Entry.Project,
		Env:            t.Entry.Slug,
	}, nil
}

func (c *Controller) recordSnapshots(ctx context.Context, t *Target, refs map[string]registry.SnapshotRef) error {
	return c.Store.Update(ctx, func(r *registry.Registry) error {
		e, err := r.Env(t.Entry.Project, t.Entry.Slug)
		if err != nil {
			return err
		}
		if e.Snapshots == nil {
			e.Snapshots = map[string]registry.SnapshotRef{}
		}
		for svc, ref := range refs {
			e.Snapshots[svc] = ref
		}
		t.Entry.Snapshots = e.Snapshots
		r.Touch()
		return nil
	})
}

func (c *Controller) cacheBranchSnapshot(ctx context.Context, t *Target) error {
	if c.Driver == nil || !t.Config.State.CacheBranch() {
		return nil
	}
	for _, st := range t.Config.Stateful {
		if !st.Prepare.Defined() {
			continue
		}
		ref, err := c.snapshotKey(ctx, t, st, t.Entry.Worktree)
		if err != nil {
			return err
		}
		if ok, err := c.Driver.Exists(ctx, ref); err != nil {
			return err
		} else if ok {
			continue
		}
		if err := c.saveSnapshot(ctx, t, st, ref, state.KindBranch, ""); err != nil {
			c.step("could not cache the snapshot for %s: %v", st.Service, shortError(err))
			return nil
		}
		if err := c.recordSnapshots(ctx, t, map[string]registry.SnapshotRef{
			st.Service: {Key: ref.Key, Source: SourceExact},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) saveSnapshot(ctx context.Context, t *Target, st config.StatefulConfig, ref state.Ref, kind state.Kind, name string) error {
	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return err
	}
	c.detail("stopping %s to snapshot it", st.Service)
	if err := stack.Compose.Stop(ctx, st.StopGrace.Duration(), st.Service); err != nil {
		return err
	}
	if err := c.assertCleanExit(ctx, t, st.Service); err != nil {
		_ = stack.Compose.Start(ctx, st.Service)
		return err
	}

	volume := meta.ComposeVolume(t.ComposeProject(), st.Volume)
	stats, saveErr := c.Driver.Save(ctx, volume, ref)

	if err := stack.Compose.Start(ctx, st.Service); err != nil {
		c.step("could not restart %s after the snapshot: %v", st.Service, err)
	} else {
		if deps := dependentsOf(stack, st.Service); len(deps) > 0 {
			c.detail("restarting %s, which lost their connections", strings.Join(deps, ", "))
			if err := stack.Compose.Restart(ctx, deps...); err != nil {
				c.step("could not restart %s: %v", strings.Join(deps, ", "), shortError(err))
			}
		}
		if err := c.WaitHealthy(ctx, t, stack, t.Config.Up.Timeout.Duration()); err != nil {
			c.step("%s did not come back cleanly after the snapshot: %v", st.Service, shortError(err))
		}
	}
	if saveErr != nil {
		return saveErr
	}
	c.detail("snapshot %s: %s in %s%s", ref.Short(), humanSize(stats.Bytes),
		stats.Duration.Round(time.Millisecond), rateSuffix(stats))
	c.warnIfLarge(stats.Bytes, st.Service)

	commit, _ := t.Project.GitIn(t.Entry.Worktree).Resolve(ctx, "HEAD")
	return c.Index.Update(ctx, func(idx *state.Index) error {
		idx.Put(state.Entry{
			Key: ref.Key, Project: ref.Project, Service: ref.Service, Volume: ref.Volume(),
			Kind: kind, Name: name, Env: t.Entry.Slug,
			SizeBytes: stats.Bytes, BuiltFrom: commit,
		})
		return nil
	})
}

func dependentsOf(stack *Stack, service string) []string {
	svc, ok := stack.Model.Services[service]
	if !ok {
		return nil
	}
	deps := stack.Model.GetDependentsForService(svc)
	sort.Strings(deps)
	return deps
}

type ResetOptions struct {
	Scratch     bool
	From        string
	SkipPrepare bool
}

func (c *Controller) ResetState(ctx context.Context, t *Target, o ResetOptions) error {
	if c.Driver == nil || len(t.Config.Stateful) == 0 {
		return errors.New("this project declares no [[stateful]] service")
	}
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	if err := c.wipeStatefulVolumes(ctx, t); err != nil {
		return err
	}

	refs := map[string]registry.SnapshotRef{}
	for _, st := range t.Config.Stateful {
		target, err := c.volumeTargetFor(ctx, t, st)
		if err != nil {
			return err
		}
		switch {
		case o.From != "":
			ref := state.CheckpointRef(t.Entry.Project, st.Service, t.Entry.Slug, o.From)
			if err := c.cloneInto(ctx, ref, target, SourceCheckpoint); err != nil {
				return fmt.Errorf("restore checkpoint %q for %s: %w", o.From, st.Service, err)
			}
			refs[st.Service] = registry.SnapshotRef{Key: ref.Key, Source: SourceCheckpoint}
		case o.Scratch:
			if err := c.Runtime.CreateVolume(ctx, engine.VolumeSpec{Name: target.Name, Labels: target.Labels()}); err != nil {
				return err
			}
			key, _ := c.snapshotKey(ctx, t, st, t.Entry.Worktree)
			refs[st.Service] = registry.SnapshotRef{Key: key.Key, Source: SourceScratch}
		default:
			ref, source, err := c.provisionOne(ctx, t, st)
			if err != nil {
				return err
			}
			refs[st.Service] = registry.SnapshotRef{Key: ref.Key, Source: source}
		}
	}
	if err := c.recordSnapshots(ctx, t, refs); err != nil {
		return err
	}

	if err := c.upLocked(ctx, t, UpOptions{}); err != nil {
		return err
	}
	if o.SkipPrepare || o.From != "" {
		return nil
	}
	return c.PrepareState(ctx, t)
}

func (c *Controller) wipeStatefulVolumes(ctx context.Context, t *Target) error {
	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(t.Config.Stateful))
	for _, st := range t.Config.Stateful {
		names = append(names, st.Service)
	}
	sort.Strings(names)
	c.step("resetting %s", strings.Join(names, ", "))

	if len(t.Entry.Networks) > 0 {
		if err := c.Router.Detach(ctx, t.Entry.Networks); err != nil {
			c.detail("detaching the router: %v", err)
		}
	}
	if err := stack.Compose.Down(ctx, false); err != nil {
		return err
	}
	for _, st := range t.Config.Stateful {
		volume := meta.ComposeVolume(t.ComposeProject(), st.Volume)
		c.detail("removing volume %s", volume)
		if err := c.Runtime.RemoveVolume(ctx, volume, true); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) SaveCheckpoint(ctx context.Context, t *Target, name string) error {
	if c.Driver == nil || len(t.Config.Stateful) == 0 {
		return errors.New("this project declares no [[stateful]] service")
	}
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	for _, st := range t.Config.Stateful {
		ref := state.CheckpointRef(t.Entry.Project, st.Service, t.Entry.Slug, name)
		c.step("saving checkpoint %q of %s", name, st.Service)
		if err := c.saveSnapshot(ctx, t, st, ref, state.KindCheckpoint, name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) Checkpoints(ctx context.Context, t *Target) ([]state.Entry, error) {
	idx, err := c.Index.Read()
	if err != nil {
		return nil, err
	}
	return idx.Checkpoints(t.Entry.Project, t.Entry.Slug), nil
}

func (c *Controller) warnIfLarge(bytes int64, service string) {
	if c.WarnSize <= 0 || bytes < c.WarnSize {
		return
	}
	c.step("%s's snapshot is %s; the volume-copy driver costs that much time on every env. "+
		"Trim the seed data, or wait for the reflink and pg-template drivers.",
		service, humanSize(bytes))
}

func rateSuffix(s state.Stats) string {
	if r := s.Rate(); r != "" {
		return " (" + r + ")"
	}
	return ""
}

func humanSize(n int64) string {
	if n <= 0 {
		return "unknown size"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func shortError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
