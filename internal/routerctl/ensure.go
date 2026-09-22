package routerctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
)

type Manager struct {
	Runtime engine.Runtime
	Store   *registry.Store
	Out     io.Writer
}

type Status struct {
	ContainerID string
	Image       string
	Port        int
	AdminPort   int
	Version     string
	Recreated   bool
	Fallback    bool
}

func (m *Manager) Ensure(ctx context.Context, cfg *config.Config) (Status, error) {
	if st, ok, err := m.check(ctx, cfg); err != nil {
		return Status{}, err
	} else if ok {
		return st, nil
	}

	lock, err := registry.NewLock(m.Store.Path("locks", "router.lock"), "the router")
	if err != nil {
		return Status{}, err
	}
	if err := lock.Acquire(ctx, 5*time.Minute); err != nil {
		return Status{}, err
	}
	defer lock.Release()

	if st, ok, err := m.check(ctx, cfg); err != nil {
		return Status{}, err
	} else if ok {
		return st, nil
	}
	return m.recreate(ctx, cfg)
}

func (m *Manager) check(ctx context.Context, cfg *config.Config) (Status, bool, error) {
	token, err := m.Store.RouterToken()
	if err != nil {
		return Status{}, false, err
	}
	reg, err := m.Store.Read()
	if err != nil {
		return Status{}, false, err
	}
	wantImage := ImageRef(cfg.Router.Image)
	adminPort := cfg.Router.AdminPort
	if reg.Router.AdminPort != 0 {
		adminPort = reg.Router.AdminPort
	}

	c, err := m.Runtime.Container(ctx, meta.RouterContainer)
	if err != nil || c.State != engine.StateRunning {
		return Status{}, false, nil
	}
	client := NewClient(adminPort, token)

	h, herr := healthWithGrace(ctx, client, 5*time.Second)
	switch {
	case herr != nil:
		m.step("replacing the router: it is not answering (%v)", shortErr(herr))
		return Status{}, false, nil
	case h.Version != meta.VersionString():
		m.step("replacing the router: it runs %s, this is %s", h.Version, meta.VersionString())
		return Status{}, false, nil
	case c.Image != wantImage:
		m.step("replacing the router: it runs image %s, want %s", c.Image, wantImage)
		return Status{}, false, nil
	}

	if _, aerr := client.Routes(ctx); aerr != nil {
		m.step("replacing the router: its admin token no longer matches this state directory")
		return Status{}, false, nil
	}

	st := Status{
		ContainerID: c.ID, Image: wantImage, Version: h.Version,
		Port: EffectivePort(reg, cfg), AdminPort: adminPort,
	}
	done, err := m.finish(ctx, st, adminPort, token, reg)
	return done, true, err
}

func (m *Manager) recreate(ctx context.Context, cfg *config.Config) (Status, error) {
	token, err := m.Store.RouterToken()
	if err != nil {
		return Status{}, err
	}
	reg, err := m.Store.Read()
	if err != nil {
		return Status{}, err
	}

	wantImage := ImageRef(cfg.Router.Image)
	wantPort := cfg.Router.Port
	if reg.Router.Port != 0 && reg.Router.Port != cfg.Router.Port && reg.Router.Port == meta.FallbackRouterPort {
		wantPort = reg.Router.Port
	}
	adminPort := cfg.Router.AdminPort
	if reg.Router.AdminPort != 0 {
		adminPort = reg.Router.AdminPort
	}

	st := Status{Image: wantImage, Port: wantPort, AdminPort: adminPort}

	if c, err := m.Runtime.Container(ctx, meta.RouterContainer); err == nil && c.State != engine.StateRunning {
		m.step("restarting the router (it is %s)", c.State)
	} else if err != nil && !errors.Is(err, engine.ErrNotFound) {
		return Status{}, err
	}

	created, fellBack, err := m.create(ctx, cfg, wantImage, wantPort, adminPort, token)
	if err != nil {
		return Status{}, err
	}
	st.ContainerID = created
	st.Recreated = true
	st.Fallback = fellBack
	if fellBack {
		st.Port = meta.FallbackRouterPort
	}
	st.Version = meta.VersionString()

	if err := m.waitHealthy(ctx, st.AdminPort, token); err != nil {
		return Status{}, err
	}
	return m.finish(ctx, st, adminPort, token, reg)
}

func (m *Manager) finish(ctx context.Context, st Status, adminPort int, token string, reg *registry.Registry) (Status, error) {
	if reg.Router.Port != st.Port || reg.Router.AdminPort != adminPort || reg.Router.Version != st.Version || reg.Router.Image != st.Image {
		err := m.Store.Update(ctx, func(r *registry.Registry) error {
			r.Router.Port = st.Port
			r.Router.AdminPort = adminPort
			r.Router.Version = st.Version
			r.Router.Image = st.Image
			r.Touch()
			return nil
		})
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

func (m *Manager) create(ctx context.Context, cfg *config.Config, image string, port, adminPort int, token string) (string, bool, error) {
	if err := EnsureImage(ctx, m.Runtime, image, m.Out); err != nil {
		return "", false, err
	}
	if err := m.Runtime.CreateVolume(ctx, engine.VolumeSpec{
		Name:   meta.RouterDataVolume,
		Labels: map[string]string{meta.LabelManaged: "true", meta.LabelRole: "router"},
	}); err != nil {
		return "", false, err
	}

	bind := cfg.Router.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	spec := func(p int) engine.ContainerSpec {
		return engine.ContainerSpec{
			Name:  meta.RouterContainer,
			Image: image,
			Labels: map[string]string{
				meta.LabelManaged: "true",
				meta.LabelRole:    "router",
				meta.LabelCreated: time.Now().UTC().Format(time.RFC3339),
			},
			Env: []string{
				router.TokenEnv + "=" + token,
				router.StatePathEnv + "=" + router.DefaultStatePath,
			},
			Mounts: []engine.Mount{{Volume: meta.RouterDataVolume, Target: "/data"}},
			Ports: []engine.PortSpec{
				{HostIP: bind, HostPort: p, ContainerPort: meta.RouterProxyPort},

				{HostIP: "127.0.0.1", HostPort: adminPort, ContainerPort: meta.RouterAdminPort},
			},

			Restart: "unless-stopped",
		}
	}

	id, err := m.Runtime.CreateAndStart(ctx, spec(port))
	if err == nil {
		return id, false, nil
	}
	if !isPortUnavailable(err) || port == meta.FallbackRouterPort {
		return "", false, err
	}
	m.step("port %d is not available (%v); falling back to %d", port, shortErr(err), meta.FallbackRouterPort)
	id, err = m.Runtime.CreateAndStart(ctx, spec(meta.FallbackRouterPort))
	if err != nil {
		return "", false, fmt.Errorf("could not publish the router on %d or %d: %w", port, meta.FallbackRouterPort, err)
	}
	return id, true, nil
}

func healthWithGrace(ctx context.Context, client *Client, grace time.Duration) (router.HealthResponse, error) {
	deadline := time.Now().Add(grace)
	for {
		h, err := client.Health(ctx)
		if err == nil || time.Now().After(deadline) {
			return h, err
		}
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (m *Manager) waitHealthy(ctx context.Context, adminPort int, token string) error {
	client := NewClient(adminPort, token)
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if _, err := client.Health(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	logs := m.routerLogs(ctx)
	return fmt.Errorf("the router did not become healthy: %w%s", last, logs)
}

func (m *Manager) routerLogs(ctx context.Context) string {
	rc, err := m.Runtime.Logs(ctx, meta.RouterContainer, engine.LogOptions{Tail: "20"})
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	if err != nil || len(b) == 0 {
		return ""
	}
	return "\n\nrouter logs:\n" + engine.DemuxLogs(b)
}

func (m *Manager) Sync(ctx context.Context, sources []Source, selfAliases bool) error {
	token, err := m.Store.RouterToken()
	if err != nil {
		return err
	}
	reg, err := m.Store.Read()
	if err != nil {
		return err
	}

	for _, s := range sources {
		if s.Env == nil || len(s.Env.Networks) == 0 || s.Env.State == registry.StateRemoving {
			continue
		}
		for _, n := range s.Env.Networks {
			var aliases []string
			if selfAliases && s.Cfg != nil {
				aliases = s.Cfg.Hosts(config.EnvIdentity{
					Project: s.Env.Project, Slug: s.Env.Slug, Slot: s.Env.Slot,
				}).AllHosts()
			}
			if err := m.Runtime.Connect(ctx, n, meta.RouterContainer, aliases); err != nil {
				if errors.Is(err, engine.ErrNotFound) {
					continue
				}
				return fmt.Errorf("attach the router to env %s: %w", s.Env.Slug, err)
			}
		}
	}

	version := reg.Router.RoutesVersion + 1
	client := NewClient(routerAdminPort(reg), token)

	if err := client.PutRoutes(ctx, BuildTable(version, sources)); err != nil {
		if !errors.Is(err, ErrStaleTable) {
			return err
		}

		current, rerr := client.Routes(ctx)
		if rerr != nil {
			return err
		}
		version = current.Version + 1
		m.step("the router held a newer route table (%d); republishing as %d", current.Version, version)
		if err := client.PutRoutes(ctx, BuildTable(version, sources)); err != nil {
			return err
		}
	}
	return m.Store.Update(ctx, func(r *registry.Registry) error {
		if r.Router.RoutesVersion < version {
			r.Router.RoutesVersion = version
			r.Touch()
		}
		return nil
	})
}

func (m *Manager) Detach(ctx context.Context, networks []string) error {
	for _, n := range networks {
		if err := m.Runtime.Disconnect(ctx, n, meta.RouterContainer); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Client() (*Client, error) {
	token, err := m.Store.RouterToken()
	if err != nil {
		return nil, err
	}
	reg, err := m.Store.Read()
	if err != nil {
		return nil, err
	}
	return NewClient(routerAdminPort(reg), token), nil
}

func (m *Manager) Remove(ctx context.Context, withData bool) error {
	if err := m.Runtime.RemoveContainer(ctx, meta.RouterContainer, true); err != nil {
		return err
	}
	if withData {
		return m.Runtime.RemoveVolume(ctx, meta.RouterDataVolume, true)
	}
	return nil
}

func EffectivePort(reg *registry.Registry, cfg *config.Config) int {
	if reg.Router.Port != 0 {
		return reg.Router.Port
	}
	return cfg.Router.Port
}

func routerAdminPort(reg *registry.Registry) int {
	if reg.Router.AdminPort != 0 {
		return reg.Router.AdminPort
	}
	return meta.DefaultRouterAdminPort
}

func (m *Manager) ConnectedNetworks(ctx context.Context) ([]string, error) {
	c, err := m.Runtime.Container(ctx, meta.RouterContainer)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(c.Networks))
	for n := range c.Networks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Manager) step(format string, args ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, ":: "+format+"\n", args...)
	}
}

func isPortUnavailable(err error) bool {
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"port is already allocated", "address already in use", "bind: permission denied",
		"ports are not available", "access permissions", "eacces", "eaddrinuse",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
