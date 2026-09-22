package envctl

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/state"
)

type GCOptions struct {
	DryRun      bool
	AllProjects bool
	After       time.Duration
	KeepGoldens int
}

type GCItem struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	Env     string `json:"env,omitempty"`
	Reason  string `json:"reason"`
	Bytes   int64  `json:"bytes,omitempty"`
}

type GCReport struct {
	Schema  int      `json:"schema"`
	DryRun  bool     `json:"dry_run"`
	Removed []GCItem `json:"removed"`
	Freed   int64    `json:"freed_bytes"`
}

func (c *Controller) GC(ctx context.Context, cfg *config.Config, o GCOptions) (GCReport, error) {
	report := GCReport{Schema: Schema, DryRun: o.DryRun}

	after := o.After
	keep := o.KeepGoldens
	if cfg != nil {
		if after == 0 {
			after = cfg.State.GCAfter.Duration()
		}
		if keep == 0 {
			keep = cfg.State.KeepGoldens
		}
	}
	if after == 0 {
		after = config.DefaultGCAfter
	}
	if keep == 0 {
		keep = config.DefaultKeepGoldens
	}

	reg, err := c.Store.Read()
	if err != nil {
		return report, err
	}
	known := knownEnvs(reg)
	inUse := c.volumesInUse(reg)

	idx, err := c.Index.Read()
	if err != nil {
		return report, err
	}
	collectable := idx.Collectable(time.Now().UTC(), after, keep, inUse)
	for _, e := range collectable {
		if !o.AllProjects && cfg != nil && e.Project != cfg.Project.Name {
			continue
		}
		report.Removed = append(report.Removed, GCItem{
			Kind: "snapshot", Name: e.Volume, Project: e.Project, Bytes: e.SizeBytes,
			Reason: fmt.Sprintf("%s snapshot of %s, unused for %s",
				e.Kind, e.Service, humanDuration(time.Since(e.LastUsed))),
		})
		report.Freed += e.SizeBytes
		if o.DryRun {
			continue
		}
		if c.Driver != nil {
			if err := c.Driver.Remove(ctx, e.Ref()); err != nil {
				return report, err
			}
		}
		if err := c.Index.Update(ctx, func(i *state.Index) error {
			i.Delete(e.Ref())
			return nil
		}); err != nil {
			return report, err
		}
	}

	sel := engine.Selector{All: true, Labels: map[string]string{meta.LabelManaged: "true"}}
	containers, err := c.Runtime.Containers(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, ct := range containers {
		project, env := ct.Labels[meta.LabelProject], ct.Labels[meta.LabelEnv]
		if env == "" || known[project+"/"+env] {
			continue
		}
		if !o.AllProjects && cfg != nil && project != cfg.Project.Name {
			continue
		}
		report.Removed = append(report.Removed, GCItem{
			Kind: "container", Name: ct.Name, Project: project, Env: env,
			Reason: "no registry entry claims this env",
		})
		if !o.DryRun {
			if err := c.Runtime.RemoveContainer(ctx, ct.ID, true); err != nil {
				return report, err
			}
		}
	}

	networks, err := c.Runtime.Networks(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, n := range networks {
		project, env := n.Labels[meta.LabelProject], n.Labels[meta.LabelEnv]
		if env == "" || known[project+"/"+env] {
			continue
		}
		if !o.AllProjects && cfg != nil && project != cfg.Project.Name {
			continue
		}
		report.Removed = append(report.Removed, GCItem{
			Kind: "network", Name: n.Name, Project: project, Env: env,
			Reason: "no registry entry claims this env",
		})
		if !o.DryRun {
			_ = c.Runtime.Disconnect(ctx, n.Name, meta.RouterContainer)
			if err := c.Runtime.RemoveNetwork(ctx, n.ID); err != nil {
				return report, err
			}
		}
	}

	volumes, err := c.Runtime.Volumes(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, v := range volumes {
		project, env := v.Labels[meta.LabelProject], v.Labels[meta.LabelEnv]
		role := v.Labels[meta.LabelRole]

		if role == "snapshot" || role == "cache" || role == "shared" || role == "router" {
			continue
		}
		if env == "" || known[project+"/"+env] {
			continue
		}
		if !o.AllProjects && cfg != nil && project != cfg.Project.Name {
			continue
		}
		report.Removed = append(report.Removed, GCItem{
			Kind: "volume", Name: v.Name, Project: project, Env: env,
			Reason: "no registry entry claims this env",
		})
		if !o.DryRun {
			if err := c.Runtime.RemoveVolume(ctx, v.Name, true); err != nil {
				return report, err
			}
		}
	}

	sort.Slice(report.Removed, func(i, j int) bool {
		if report.Removed[i].Kind != report.Removed[j].Kind {
			return report.Removed[i].Kind < report.Removed[j].Kind
		}
		return report.Removed[i].Name < report.Removed[j].Name
	})
	return report, nil
}

func knownEnvs(reg *registry.Registry) map[string]bool {
	out := map[string]bool{}
	for _, e := range reg.All() {
		out[e.Project+"/"+e.Slug] = true
	}
	return out
}

func (c *Controller) volumesInUse(reg *registry.Registry) map[string]bool {
	out := map[string]bool{}
	for _, e := range reg.All() {
		for service, ref := range e.Snapshots {
			if ref.Key == "" {
				continue
			}
			out[state.Ref{Project: e.Project, Service: service, Key: ref.Key}.Volume()] = true
		}
	}
	return out
}

func (c *Controller) Uninstall(ctx context.Context, keepState bool) (GCReport, error) {
	report := GCReport{Schema: Schema}
	sel := engine.Selector{All: true, Labels: map[string]string{meta.LabelManaged: "true"}}

	containers, err := c.Runtime.Containers(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, ct := range containers {
		report.Removed = append(report.Removed, GCItem{Kind: "container", Name: ct.Name, Reason: "uninstall"})
		if err := c.Runtime.RemoveContainer(ctx, ct.ID, true); err != nil {
			return report, err
		}
	}
	networks, err := c.Runtime.Networks(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, n := range networks {
		report.Removed = append(report.Removed, GCItem{Kind: "network", Name: n.Name, Reason: "uninstall"})
		if err := c.Runtime.RemoveNetwork(ctx, n.ID); err != nil {
			return report, err
		}
	}
	volumes, err := c.Runtime.Volumes(ctx, sel)
	if err != nil {
		return report, err
	}
	for _, v := range volumes {
		report.Removed = append(report.Removed, GCItem{Kind: "volume", Name: v.Name, Reason: "uninstall"})
		if err := c.Runtime.RemoveVolume(ctx, v.Name, true); err != nil {
			return report, err
		}
	}
	if keepState {
		return report, nil
	}
	if err := c.Store.Wipe(); err != nil {
		return report, err
	}
	report.Removed = append(report.Removed, GCItem{Kind: "state", Name: c.Store.Home(), Reason: "uninstall"})
	return report, nil
}

func (c *Controller) WorktreesOf(reg *registry.Registry) []string {
	var out []string
	for _, e := range reg.All() {
		if e.Worktree != "" {
			out = append(out, e.Worktree)
		}
	}
	sort.Strings(out)
	return out
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
