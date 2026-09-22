package envctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/routerctl"
	"github.com/Muronuch/grove/internal/state"
)

var ErrNotInEnv = errors.New("not inside a " + meta.Name + "-managed worktree")

type Controller struct {
	Store    *registry.Store
	Runtime  engine.Runtime
	Router   *routerctl.Manager
	Progress io.Writer
	Verbose  bool
	Trust    bool
	Driver   state.Driver
	Index    *state.Store
	WarnSize int64
	Confirm  func(prompt string, commands []string) (bool, error)
}

func NewController(ctx context.Context, home string, progress io.Writer) (*Controller, error) {
	store, err := registry.Open(home)
	if err != nil {
		return nil, err
	}
	rt, err := engine.NewDocker(ctx)
	if err != nil {
		return nil, err
	}
	c := &Controller{Store: store, Runtime: rt, Progress: progress}
	c.Router = &routerctl.Manager{Runtime: rt, Store: store, Out: progress}
	return c, nil
}

func (c *Controller) Close() error {
	if c.Runtime != nil {
		return c.Runtime.Close()
	}
	return nil
}

func (c *Controller) step(format string, args ...any) {
	if c.Progress != nil {
		fmt.Fprintf(c.Progress, ":: "+format+"\n", args...)
	}
}

func (c *Controller) detail(format string, args ...any) {
	if c.Progress != nil && c.Verbose {
		fmt.Fprintf(c.Progress, "   "+format+"\n", args...)
	}
}

type Target struct {
	Project *project.Project
	Entry   *registry.Env
	Config  *config.Config
}

func (t *Target) Identity() config.EnvIdentity {
	return config.EnvIdentity{
		Project: t.Entry.Project,
		Slug:    t.Entry.Slug,
		Slot:    t.Entry.Slot,
		Repo:    t.Project.RepoName(),
	}
}

func (t *Target) Hosts() config.HostSet { return t.Config.Hosts(t.Identity()) }

func (t *Target) ComposeProject() string { return t.Entry.ComposeProject() }

func (c *Controller) FindProject(ctx context.Context, dir string) (*project.Project, error) {
	p, err := project.Find(ctx, dir)
	if err != nil {
		return nil, err
	}
	reg, err := c.Store.Read()
	if err != nil {
		return nil, err
	}
	p.Config.Router.Port = routerctl.EffectivePort(reg, p.Config)
	if reg.Router.AdminPort != 0 {
		p.Config.Router.AdminPort = reg.Router.AdminPort
	}
	if err := c.Configure(p.Config); err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Controller) Configure(cfg *config.Config) error {
	if c.Index == nil {
		c.Index = state.NewStore(c.Store)
	}
	driver, err := state.New(cfg.State.Driver, c.Runtime, cfg.State.HelperImage)
	if err != nil {
		return err
	}
	c.Driver = driver
	c.WarnSize = cfg.State.WarnSize.Bytes
	return nil
}

func (c *Controller) Resolve(ctx context.Context, dir, name string) (*Target, error) {
	p, err := c.FindProject(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := c.Reconcile(ctx, p); err != nil {
		return nil, err
	}
	reg, err := c.Store.Read()
	if err != nil {
		return nil, err
	}
	ps, ok := reg.LookupProject(p.Name())
	if !ok {
		if name == "" {
			return nil, fmt.Errorf("%w, and project %q has no envs yet (create one with `%s new <branch>`)",
				ErrNotInEnv, p.Name(), meta.Name)
		}
		return nil, fmt.Errorf("%w: %q (project %q has no envs)", registry.ErrEnvNotFound, name, p.Name())
	}

	if name == "" {
		abs, _ := absPath(dir)
		e, found := reg.FindByWorktree(abs)
		if !found || e.Project != p.Name() {
			return nil, fmt.Errorf("%w; name the env, e.g. `%s status %s`",
				ErrNotInEnv, meta.Name, firstSlug(ps))
		}
		return &Target{Project: p, Entry: e, Config: p.Config}, nil
	}

	e, err := lookupEnv(ps, name)
	if err != nil {
		return nil, err
	}
	return &Target{Project: p, Entry: e, Config: p.Config}, nil
}

func lookupEnv(ps *registry.ProjectState, name string) (*registry.Env, error) {
	if e, ok := ps.Envs[name]; ok {
		return e, nil
	}
	if slot, ok := strings.CutPrefix(name, "s"); ok {
		if n, err := strconv.Atoi(slot); err == nil {
			for _, e := range ps.List() {
				if e.Slot == n {
					return e, nil
				}
			}
		}
	}
	var byBranch []*registry.Env
	for _, e := range ps.List() {
		if e.Branch == name {
			byBranch = append(byBranch, e)
		}
	}
	if len(byBranch) == 1 {
		return byBranch[0], nil
	}
	known := make([]string, 0, len(ps.Envs))
	for _, e := range ps.List() {
		known = append(known, e.Slug)
	}
	if len(known) == 0 {
		return nil, fmt.Errorf("%w: %q (project %q has no envs)", registry.ErrEnvNotFound, name, ps.Name)
	}
	return nil, fmt.Errorf("%w: %q (known: %s)", registry.ErrEnvNotFound, name, strings.Join(known, ", "))
}

func firstSlug(ps *registry.ProjectState) string {
	if list := ps.List(); len(list) > 0 {
		return list[0].Slug
	}
	return "<env>"
}

func (c *Controller) Reconcile(ctx context.Context, p *project.Project) error {
	containers, err := c.Runtime.Containers(ctx, engine.Selector{
		All:    true,
		Labels: map[string]string{meta.LabelManaged: "true", meta.LabelProject: p.Name()},
	})
	if err != nil {
		return err
	}
	byEnv := map[string][]engine.Container{}
	for _, ct := range containers {
		if slug := ct.Labels[meta.LabelEnv]; slug != "" {
			byEnv[slug] = append(byEnv[slug], ct)
		}
	}

	return c.Store.Update(ctx, func(r *registry.Registry) error {
		ps := r.Project(p.Name(), p.Root)
		for _, e := range ps.List() {
			found := byEnv[e.Slug]
			switch {
			case len(found) == 0:

				if e.State.Live() {
					e.State = registry.StateMissing
					r.Touch()
				}
			default:
				state := deriveState(found)
				if e.State != state && e.State != registry.StateCreating && e.State != registry.StateRemoving {
					e.State = state
					if state == registry.StateRunning {
						e.Error = ""
					}
					r.Touch()
				}
				if updatePublishedPorts(e, found) {
					r.Touch()
				}
			}
		}
		return nil
	})
}

func deriveState(cs []engine.Container) registry.State {
	running, paused, other := 0, 0, 0
	for _, c := range cs {
		switch c.State {
		case engine.StateRunning:
			running++
		case engine.StatePaused:
			paused++
		default:
			other++
		}
	}
	switch {
	case paused > 0 && running == 0:
		return registry.StatePaused
	case running > 0:
		return registry.StateRunning
	case other > 0:
		return registry.StateStopped
	default:
		return registry.StateMissing
	}
}

func updatePublishedPorts(e *registry.Env, cs []engine.Container) bool {
	found := map[string]string{}
	for _, ct := range cs {
		svc := ct.ComposeService()
		if svc == "" {
			continue
		}
		for _, bindings := range ct.Ports {
			for _, b := range bindings {
				if b.Port == 0 {
					continue
				}
				ip := b.IP
				if ip == "" || ip == "0.0.0.0" {
					ip = "127.0.0.1"
				}
				found[svc] = fmt.Sprintf("%s:%d", ip, b.Port)
			}
		}
	}
	if len(found) == 0 && len(e.TCP) == 0 {
		return false
	}
	if mapsEqual(e.TCP, found) {
		return false
	}
	if len(found) == 0 {
		e.TCP = nil
	} else {
		e.TCP = found
	}
	return true
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (c *Controller) Sources(ctx context.Context, current *project.Project) ([]routerctl.Source, error) {
	reg, err := c.Store.Read()
	if err != nil {
		return nil, err
	}
	configs := map[string]*config.Config{}
	if current != nil {
		configs[current.Name()] = current.Config
	}

	var out []routerctl.Source
	names := make([]string, 0, len(reg.Projects))
	for n := range reg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		ps := reg.Projects[name]
		cfg := configs[name]
		if cfg == nil {
			loaded, err := loadProjectConfig(ps, reg)
			if err != nil {
				c.detail("skipping project %s in the route table: %v", name, err)
				continue
			}
			cfg = loaded
			configs[name] = cfg
		}
		for _, e := range ps.List() {
			if e.State == registry.StateRemoving {
				continue
			}
			out = append(out, routerctl.Source{Cfg: cfg, Env: e})
		}
	}
	return out, nil
}

func loadProjectConfig(ps *registry.ProjectState, reg *registry.Registry) (*config.Config, error) {
	if ps.Root == "" {
		return nil, fmt.Errorf("project %s has no recorded root", ps.Name)
	}
	cfg, err := config.Load(filepath.Join(ps.Root, config.FileName))
	if err != nil {
		return nil, err
	}
	cfg.Router.Port = routerctl.EffectivePort(reg, cfg)
	if reg.Router.AdminPort != 0 {
		cfg.Router.AdminPort = reg.Router.AdminPort
	}
	return cfg, nil
}

func (c *Controller) SyncRouter(ctx context.Context, p *project.Project) error {
	if _, err := c.Router.Ensure(ctx, p.Config); err != nil {
		return err
	}

	reg, err := c.Store.Read()
	if err != nil {
		return err
	}
	p.Config.Router.Port = routerctl.EffectivePort(reg, p.Config)

	sources, err := c.Sources(ctx, p)
	if err != nil {
		return err
	}
	return c.Router.Sync(ctx, sources, p.Config.Router.SelfAliases)
}

func (c *Controller) setState(ctx context.Context, t *Target, state registry.State, errMsg string) error {
	return c.Store.Update(ctx, func(r *registry.Registry) error {
		e, err := r.Env(t.Entry.Project, t.Entry.Slug)
		if err != nil {
			return err
		}
		e.State = state
		e.Error = errMsg
		if state == registry.StateRunning {
			e.LastActivity = time.Now().UTC()
		}
		t.Entry.State = state
		t.Entry.Error = errMsg
		r.Touch()
		return nil
	})
}

func (c *Controller) TouchActivity(ctx context.Context, t *Target, hold time.Duration) error {
	now := time.Now().UTC()
	return c.Store.Update(ctx, func(r *registry.Registry) error {
		e, err := r.Env(t.Entry.Project, t.Entry.Slug)
		if err != nil {
			return err
		}
		e.LastActivity = now
		if hold > 0 {
			until := now.Add(hold)
			if until.After(e.HoldUntil) {
				e.HoldUntil = until
			}
		}
		r.Touch()
		return nil
	})
}

func absPath(p string) (string, error) {
	if p == "" {
		return os.Getwd()
	}
	return filepath.Abs(p)
}
