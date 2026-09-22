package envctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
)

var ErrUnhealthy = errors.New("services did not become healthy")

type UpOptions struct {
	Headless    bool
	Full        bool
	Build       *bool
	SkipPrepare bool
	Timeout     time.Duration
}

func (c *Controller) Up(ctx context.Context, t *Target, o UpOptions) error {
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()
	return c.upLocked(ctx, t, o)
}

func (c *Controller) upLocked(ctx context.Context, t *Target, o UpOptions) error {
	wasPaused := t.Entry.State == registry.StatePaused

	if t.Entry.State != registry.StateRunning {
		if err := c.setState(ctx, t, registry.StateCreating, ""); err != nil {
			return err
		}
	}
	if err := c.upFrom(ctx, t, o, wasPaused); err != nil {
		_ = c.setState(ctx, t, registry.StateFailed, err.Error())
		_ = c.SyncRouter(ctx, t.Project)
		return err
	}
	return nil
}

func (c *Controller) upFrom(ctx context.Context, t *Target, o UpOptions, wasPaused bool) error {
	headless := t.Entry.Headless
	if o.Headless {
		headless = true
	}
	if o.Full {
		headless = false
	}

	if wasPaused && !o.Full {
		stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: headless})
		if err != nil {
			return err
		}
		c.step("resuming %s", t.Entry.Slug)
		if err := stack.Compose.Unpause(ctx); err != nil {
			return err
		}
		return c.finishUp(ctx, t, stack, o)
	}

	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: headless})
	if err != nil {
		return err
	}
	if err := c.SyncRouter(ctx, t.Project); err != nil {
		return err
	}

	if err := c.ensureExternalVolumes(ctx, t, stack); err != nil {
		return err
	}

	build := t.Config.Up.ShouldBuild()
	if o.Build != nil {
		build = *o.Build
	}
	c.step("starting %d services", len(stack.Services()))
	if err := stack.Compose.Up(ctx, build); err != nil {
		return err
	}
	if headless != t.Entry.Headless {
		if err := c.Store.Update(ctx, func(r *registry.Registry) error {
			e, err := r.Env(t.Entry.Project, t.Entry.Slug)
			if err != nil {
				return err
			}
			e.Headless = headless
			t.Entry.Headless = headless
			r.Touch()
			return nil
		}); err != nil {
			return err
		}
	}
	return c.finishUp(ctx, t, stack, o)
}

func (c *Controller) finishUp(ctx context.Context, t *Target, stack *Stack, o UpOptions) error {
	networks := stack.RouterNetworks()
	if err := c.recordStack(ctx, t, stack, networks); err != nil {
		return err
	}
	if err := c.SyncRouter(ctx, t.Project); err != nil {
		return err
	}

	timeout := t.Config.Up.Timeout.Duration()
	if o.Timeout > 0 {
		timeout = o.Timeout
	}
	if err := c.WaitHealthy(ctx, t, stack, timeout); err != nil {
		return err
	}
	if err := c.setState(ctx, t, registry.StateRunning, ""); err != nil {
		return err
	}

	if err := c.Reconcile(ctx, t.Project); err != nil {
		return err
	}
	return c.SyncRouter(ctx, t.Project)
}

func (c *Controller) ensureExternalVolumes(ctx context.Context, t *Target, stack *Stack) error {
	for _, name := range stack.Result.ExternalVolumes {
		err := c.Runtime.CreateVolume(ctx, engine.VolumeSpec{
			Name: name,
			Labels: map[string]string{
				meta.LabelManaged: "true",
				meta.LabelProject: t.Entry.Project,
				meta.LabelRole:    "shared",
			},
		})
		if err != nil {
			return fmt.Errorf("create shared volume %s: %w", name, err)
		}
	}
	return nil
}

func (c *Controller) recordStack(ctx context.Context, t *Target, stack *Stack, networks []string) error {
	return c.Store.Update(ctx, func(r *registry.Registry) error {
		e, err := r.Env(t.Entry.Project, t.Entry.Slug)
		if err != nil {
			return err
		}
		e.ComposeFile = stack.Path
		e.Networks = networks
		t.Entry.ComposeFile = stack.Path
		t.Entry.Networks = networks
		r.Touch()
		return nil
	})
}

func (c *Controller) WaitHealthy(ctx context.Context, t *Target, stack *Stack, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	services := stack.Services()

	probes := map[string]string{}
	if stack.Routed {
		for _, s := range t.Config.RoutableServices() {
			if _, ok := stack.Model.Services[s.Name]; ok {
				probes[s.Name] = t.Hosts().Host(s.Name)
			}
		}
	}

	c.step("waiting for %d services", len(services))
	var lastPending []string
	for {
		containers, err := c.Runtime.Containers(ctx, engine.Selector{
			All:    true,
			Labels: map[string]string{meta.LabelManaged: "true", meta.LabelProject: t.Entry.Project, meta.LabelEnv: t.Entry.Slug},
		})
		if err != nil {
			return err
		}
		byService := map[string]engine.Container{}
		for _, ct := range containers {
			if svc := ct.ComposeService(); svc != "" {
				byService[svc] = ct
			}
		}

		var pending []string
		for _, svc := range services {
			ct, ok := byService[svc]
			if !ok {
				pending = append(pending, svc+" (no container)")
				continue
			}
			switch {
			case ct.State == engine.StateExited || ct.State == engine.StateDead:

				if ct.ExitCode == 0 {
					continue
				}
				return c.unhealthy(ctx, t, svc, ct, fmt.Sprintf("exited with code %d", ct.ExitCode))
			case ct.State == engine.StateRestarting:
				return c.unhealthy(ctx, t, svc, ct, "is restarting in a loop")
			case ct.State != engine.StateRunning:
				pending = append(pending, fmt.Sprintf("%s (%s)", svc, ct.State))
				continue
			}
			switch ct.Health {
			case engine.HealthUnhealthy:
				pending = append(pending, svc+" (unhealthy)")
			case engine.HealthStarting:
				pending = append(pending, svc+" (starting)")
			case engine.HealthHealthy:
			default:

				if host, routable := probes[svc]; routable {
					if !c.probeThroughRouter(ctx, t, host) {
						pending = append(pending, svc+" (not listening)")
					}
				}
			}
		}

		if len(pending) == 0 {
			return nil
		}
		if !equalStrings(pending, lastPending) {
			c.detail("waiting on: %s", strings.Join(pending, ", "))
			lastPending = pending
		}
		if time.Now().After(deadline) {
			svc := strings.SplitN(pending[0], " ", 2)[0]
			ct := byService[svc]
			return c.unhealthy(ctx, t, svc, ct,
				fmt.Sprintf("did not become healthy within %s (waiting on: %s)", timeout, strings.Join(pending, ", ")))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (c *Controller) unhealthy(ctx context.Context, t *Target, service string, ct engine.Container, why string) error {
	logs := ""
	if ct.ID != "" {
		logs = c.tailLogs(ctx, ct.ID, 25)
	}
	msg := fmt.Sprintf("service %q of env %s %s", service, t.Entry.Slug, why)
	if logs != "" {
		msg += "\n\nlast log lines:\n" + indent(logs, "  ")
	}
	msg += fmt.Sprintf("\n\nreproduce with: %s logs %s %s", meta.Name, t.Entry.Slug, service)
	return fmt.Errorf("%w: %s", ErrUnhealthy, msg)
}

func (c *Controller) tailLogs(ctx context.Context, id string, n int) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rc, err := c.Runtime.Logs(ctx, id, engine.LogOptions{Tail: fmt.Sprint(n)})
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 128<<10))
	if err != nil && len(b) == 0 {
		return ""
	}
	return engine.LastLines(engine.DemuxLogs(b), n)
}

func (c *Controller) probeThroughRouter(ctx context.Context, t *Target, host string) bool {
	port := t.Config.Router.Port
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Host = host

	req.Header.Set("Accept", "*/*")
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	return res.Header.Get(router.ErrorHeader) == ""
}

type StopOptions struct {
	Grace time.Duration
}

func (c *Controller) Stop(ctx context.Context, t *Target, o StopOptions) error {
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return err
	}
	if err := stack.Compose.Stop(ctx, o.Grace); err != nil {
		return err
	}
	if err := c.setState(ctx, t, registry.StateStopped, ""); err != nil {
		return err
	}
	return c.SyncRouter(ctx, t.Project)
}

func (c *Controller) Pause(ctx context.Context, t *Target) error {
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return err
	}
	if err := stack.Compose.Pause(ctx); err != nil {
		return err
	}
	if err := c.setState(ctx, t, registry.StatePaused, ""); err != nil {
		return err
	}
	return c.SyncRouter(ctx, t.Project)
}

func (c *Controller) Restart(ctx context.Context, t *Target, services []string) error {
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return err
	}
	if err := stack.Compose.Restart(ctx, services...); err != nil {
		return err
	}
	return c.finishUp(ctx, t, stack, UpOptions{})
}

func (c *Controller) Logs(ctx context.Context, t *Target, service string, o engine.LogOptions, w io.Writer) error {
	ids, err := c.containerIDs(ctx, t, service)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("env %s has no container for service %q", t.Entry.Slug, service)
	}
	rc, err := c.Runtime.Logs(ctx, ids[0], o)
	if err != nil {
		return err
	}
	defer rc.Close()
	return engine.LogWriter(w, rc)
}

func (c *Controller) Exec(ctx context.Context, t *Target, service string, cmd []string, stdin io.Reader, stdout, stderr io.Writer, tty bool) (int, error) {
	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return 1, err
	}
	if _, ok := stack.Model.Services[service]; !ok {
		return 1, fmt.Errorf("env %s has no service %q (services: %s)",
			t.Entry.Slug, service, strings.Join(stack.Services(), ", "))
	}
	args := []string{"exec"}
	if !tty {
		args = append(args, "-T")
	}
	args = append(args, service)
	args = append(args, cmd...)
	return stack.Compose.RunInteractive(ctx, stdin, stdout, stderr, args...)
}

func (c *Controller) containerIDs(ctx context.Context, t *Target, service string) ([]string, error) {
	labels := map[string]string{
		meta.LabelManaged: "true",
		meta.LabelProject: t.Entry.Project,
		meta.LabelEnv:     t.Entry.Slug,
	}
	if service != "" {
		labels["com.docker.compose.service"] = service
	}
	cs, err := c.Runtime.Containers(ctx, engine.Selector{All: true, Labels: labels})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cs))
	for _, ct := range cs {
		out = append(out, ct.ID)
	}
	return out, nil
}

type DownOptions struct {
	KeepWorktree bool
	KeepVolumes  bool
	DeleteBranch bool
	Force        bool
}

func (c *Controller) Down(ctx context.Context, t *Target, o DownOptions) error {
	lock, err := c.Store.EnvLock(t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer lock.Release()

	previous := t.Entry.State
	if err := c.setState(ctx, t, registry.StateRemoving, ""); err != nil {
		return err
	}
	restore := func(cause error) error {
		_ = c.setState(ctx, t, previous, "")
		_ = c.SyncRouter(ctx, t.Project)
		return cause
	}

	if len(t.Config.Hooks.PreDown) > 0 && t.Entry.Worktree != "" {
		if err := c.runHooks(ctx, t, t.Config.Hooks.PreDown, "pre_down"); err != nil {
			c.step("pre_down hook failed, continuing with teardown: %v", err)
		}
	}

	if len(t.Entry.Networks) > 0 {
		if err := c.Router.Detach(ctx, t.Entry.Networks); err != nil {
			c.detail("detaching the router: %v", err)
		}
	}

	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		c.detail("could not rebuild the compose model (%v); removing by label", err)
		if err := c.removeByLabel(ctx, t, !o.KeepVolumes); err != nil {
			return err
		}
	} else {
		c.step("removing the stack")
		if err := stack.Compose.Down(ctx, !o.KeepVolumes); err != nil {
			return restore(err)
		}
	}

	switch {
	case o.KeepWorktree || t.Entry.Worktree == "":
	case !t.Entry.OwnsWorktree:
		c.detail("leaving %s in place: %s did not create it", t.Entry.Worktree, meta.Name)
	default:
		if err := c.removeWorktree(ctx, t, o.Force); err != nil {
			return restore(err)
		}
	}
	if o.DeleteBranch && t.Entry.Branch != "" {
		if err := t.Project.Git().DeleteBranch(ctx, t.Entry.Branch); err != nil {
			c.step("could not delete branch %s: %v", t.Entry.Branch, err)
		}
	}

	if err := c.Store.Update(ctx, func(r *registry.Registry) error {
		ps, ok := r.LookupProject(t.Entry.Project)
		if !ok {
			return nil
		}
		delete(ps.Envs, t.Entry.Slug)
		r.Touch()
		return nil
	}); err != nil {
		return err
	}
	return c.SyncRouter(ctx, t.Project)
}

func (c *Controller) teardownStack(ctx context.Context, t *Target) error {
	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		c.detail("could not rebuild the compose model (%v); removing by label", err)
		return c.removeByLabel(ctx, t, true)
	}
	if err := stack.Compose.Down(ctx, true); err != nil {
		c.detail("compose down failed (%v); removing by label", err)
		return c.removeByLabel(ctx, t, true)
	}
	return nil
}

func (c *Controller) removeByLabel(ctx context.Context, t *Target, volumes bool) error {
	sel := engine.Selector{All: true, Labels: map[string]string{
		meta.LabelManaged: "true",
		meta.LabelProject: t.Entry.Project,
		meta.LabelEnv:     t.Entry.Slug,
	}}
	cs, err := c.Runtime.Containers(ctx, sel)
	if err != nil {
		return err
	}
	for _, ct := range cs {
		if err := c.Runtime.RemoveContainer(ctx, ct.ID, true); err != nil {
			return err
		}
	}
	ns, err := c.Runtime.Networks(ctx, sel)
	if err != nil {
		return err
	}
	for _, n := range ns {
		if err := c.Runtime.RemoveNetwork(ctx, n.ID); err != nil {
			return err
		}
	}
	if !volumes {
		return nil
	}
	vs, err := c.Runtime.Volumes(ctx, sel)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := c.Runtime.RemoveVolume(ctx, v.Name, true); err != nil {
			return err
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
