package envctl

import (
	"context"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/routerctl"
)

const Schema = 1

type Report struct {
	Schema int         `json:"schema"`
	Envs   []EnvStatus `json:"envs"`
	Router *RouterInfo `json:"router,omitempty"`
}

type RouterInfo struct {
	State     string `json:"state"`
	Port      int    `json:"port"`
	AdminPort int    `json:"admin_port"`
	Version   string `json:"version"`
	Image     string `json:"image"`
}

type EnvStatus struct {
	Project     string                          `json:"project"`
	Env         string                          `json:"env"`
	Slot        int                             `json:"slot"`
	Branch      string                          `json:"branch"`
	Worktree    string                          `json:"worktree"`
	State       string                          `json:"state"`
	Pinned      bool                            `json:"pinned"`
	Headless    bool                            `json:"headless"`
	IdleSeconds int64                           `json:"idle_seconds"`
	MemoryBytes int64                           `json:"memory_bytes"`
	HeldSeconds int64                           `json:"held_seconds,omitempty"`
	URLs        map[string]string               `json:"urls"`
	TCP         map[string]string               `json:"tcp,omitempty"`
	Services    []ServiceStatus                 `json:"services"`
	Snapshots   map[string]registry.SnapshotRef `json:"snapshots,omitempty"`
	Error       string                          `json:"error,omitempty"`
}

type ServiceStatus struct {
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Health      string  `json:"health,omitempty"`
	Container   string  `json:"container,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	CPUPercent  float64 `json:"cpu_percent,omitempty"`
}

type StatusOptions struct {
	Stats       bool
	AllProjects bool
}

func (c *Controller) Status(ctx context.Context, t *Target, o StatusOptions) (EnvStatus, error) {
	containers, err := c.envContainers(ctx, t.Entry.Project, t.Entry.Slug)
	if err != nil {
		return EnvStatus{}, err
	}
	stats := map[string]engine.Stats{}
	if o.Stats {
		stats = c.sampleStats(ctx, containers)
	}
	return c.buildStatus(t.Config, t.Entry, containers, stats), nil
}

func (c *Controller) List(ctx context.Context, dir string, o StatusOptions) (Report, error) {
	report := Report{Schema: Schema}

	reg, err := c.Store.Read()
	if err != nil {
		return report, err
	}
	report.Router = c.routerInfo(ctx, reg)

	var only string
	configs := map[string]*config.Config{}
	if p, err := c.FindProject(ctx, dir); err == nil {
		if err := c.Reconcile(ctx, p); err != nil {
			return report, err
		}
		configs[p.Name()] = p.Config
		if !o.AllProjects {
			only = p.Name()
		}
		reg, err = c.Store.Read()
		if err != nil {
			return report, err
		}
	} else if only == "" && !o.AllProjects {
		o.AllProjects = true
	}

	names := make([]string, 0, len(reg.Projects))
	for n := range reg.Projects {
		if only != "" && n != only {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	reconciled := false
	for _, name := range names {
		ps := reg.Projects[name]
		cfg := configs[name]
		if cfg == nil {
			loaded, err := loadProjectConfig(ps, reg)
			if err != nil {
				c.detail("skipping project %s: %v", name, err)
				continue
			}
			cfg = loaded
			configs[name] = cfg
			if p, err := project.FromConfig(ctx, cfg, ps.Root); err == nil {
				if err := c.Reconcile(ctx, p); err == nil {
					reconciled = true
				}
			}
		}
	}
	if reconciled {
		if fresh, err := c.Store.Read(); err == nil {
			reg = fresh
		}
	}

	for _, name := range names {
		ps, ok := reg.Projects[name]
		if !ok {
			continue
		}
		cfg := configs[name]
		if cfg == nil {
			continue
		}
		containers, err := c.projectContainers(ctx, name)
		if err != nil {
			return report, err
		}
		var stats map[string]engine.Stats
		if o.Stats {
			var all []engine.Container
			for _, cs := range containers {
				all = append(all, cs...)
			}
			stats = c.sampleStats(ctx, all)
		}
		for _, e := range ps.List() {
			report.Envs = append(report.Envs, c.buildStatus(cfg, e, containers[e.Slug], stats))
		}
	}
	return report, nil
}

func (c *Controller) routerInfo(ctx context.Context, reg *registry.Registry) *RouterInfo {
	info := &RouterInfo{
		Port:      reg.Router.Port,
		AdminPort: reg.Router.AdminPort,
		Version:   reg.Router.Version,
		Image:     reg.Router.Image,
		State:     "missing",
	}
	if ct, err := c.Runtime.Container(ctx, meta.RouterContainer); err == nil {
		info.State = string(ct.State)
	}
	return info
}

func (c *Controller) buildStatus(cfg *config.Config, e *registry.Env, containers []engine.Container, stats map[string]engine.Stats) EnvStatus {
	now := time.Now().UTC()
	id := config.EnvIdentity{Project: e.Project, Slug: e.Slug, Slot: e.Slot}
	hosts := cfg.Hosts(id)

	st := EnvStatus{
		Project:     e.Project,
		Env:         e.Slug,
		Slot:        e.Slot,
		Branch:      e.Branch,
		Worktree:    e.Worktree,
		State:       string(e.State),
		Pinned:      e.Pinned,
		Headless:    e.Headless,
		IdleSeconds: int64(e.IdleFor(now).Seconds()),
		URLs:        hosts.URLs(),
		TCP:         e.TCP,
		Snapshots:   e.Snapshots,
		Error:       e.Error,
	}
	if e.Held(now) {
		st.HeldSeconds = int64(e.HoldUntil.Sub(now).Seconds())
	}
	for _, ct := range containers {
		svc := ct.ComposeService()
		if svc == "" {
			continue
		}
		s := ServiceStatus{
			Name:      svc,
			State:     string(ct.State),
			Health:    string(ct.Health),
			Container: ct.Name,
		}
		if sample, ok := stats[ct.ID]; ok {
			s.MemoryBytes = sample.MemoryBytes
			s.CPUPercent = sample.CPUPercent
			st.MemoryBytes += sample.MemoryBytes
		}
		st.Services = append(st.Services, s)
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
	return st
}

func (c *Controller) envContainers(ctx context.Context, projectName, slug string) ([]engine.Container, error) {
	return c.Runtime.Containers(ctx, engine.Selector{
		All: true,
		Labels: map[string]string{
			meta.LabelManaged: "true",
			meta.LabelProject: projectName,
			meta.LabelEnv:     slug,
		},
	})
}

func (c *Controller) projectContainers(ctx context.Context, projectName string) (map[string][]engine.Container, error) {
	cs, err := c.Runtime.Containers(ctx, engine.Selector{
		All:    true,
		Labels: map[string]string{meta.LabelManaged: "true", meta.LabelProject: projectName},
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]engine.Container{}
	for _, ct := range cs {
		if slug := ct.Labels[meta.LabelEnv]; slug != "" {
			out[slug] = append(out[slug], ct)
		}
	}
	return out, nil
}

func (c *Controller) sampleStats(ctx context.Context, containers []engine.Container) map[string]engine.Stats {
	var ids []string
	for _, ct := range containers {
		if ct.State == engine.StateRunning {
			ids = append(ids, ct.ID)
		}
	}
	if len(ids) == 0 {
		return map[string]engine.Stats{}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stats, err := c.Runtime.Stats(ctx, ids)
	if err != nil {
		return map[string]engine.Stats{}
	}
	return stats
}

func (c *Controller) RouterSources(ctx context.Context) ([]routerctl.Source, error) {
	return c.Sources(ctx, nil)
}
