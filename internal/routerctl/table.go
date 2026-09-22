package routerctl

import (
	"fmt"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
)

type Source struct {
	Cfg *config.Config
	Env *registry.Env
}

func BuildTable(version int64, sources []Source) router.Table {
	t := router.Table{Version: version, UpdatedAt: time.Now().UTC()}
	for _, s := range sources {
		if s.Cfg == nil || s.Env == nil {
			continue
		}
		t.Envs = append(t.Envs, EnvRoute(s.Cfg, s.Env))
	}
	sort.Slice(t.Envs, func(i, j int) bool {
		if t.Envs[i].Project != t.Envs[j].Project {
			return t.Envs[i].Project < t.Envs[j].Project
		}
		return t.Envs[i].Env < t.Envs[j].Env
	})
	return t
}

func EnvRoute(cfg *config.Config, e *registry.Env) router.EnvRoute {
	id := config.EnvIdentity{Project: e.Project, Slug: e.Slug, Slot: e.Slot}
	hosts := cfg.Hosts(id)
	composeProject := meta.ComposeProject(e.Project, e.Slug)

	entry := router.EnvRoute{
		Project:   e.Project,
		Env:       e.Slug,
		Slot:      e.Slot,
		State:     routerState(e.State),
		Hosts:     hosts.AllHosts(),
		URLs:      hosts.URLs(),
		LastError: e.Error,
	}

	upstream := func(name string, port int) string {
		return fmt.Sprintf("%s:%d", meta.ContainerName(composeProject, name, 1), port)
	}

	def, hasDefault := cfg.DefaultService()
	if hasDefault {
		entry.Routes = append(entry.Routes, router.Route{
			HostPrefix:    "",
			Path:          "/",
			Upstream:      upstream(def.Name, def.Port),
			Service:       def.Name,
			RewriteHost:   def.RewriteHost,
			WebsocketPath: def.WebsocketPath,
		})
	}

	for _, s := range cfg.RoutableServices() {
		for _, p := range s.Paths {
			entry.Routes = append(entry.Routes, router.Route{
				HostPrefix:    "",
				Path:          p,
				Upstream:      upstream(s.Name, s.Port),
				Service:       s.Name,
				RewriteHost:   s.RewriteHost,
				WebsocketPath: s.WebsocketPath,
			})
		}

		if hasDefault && s.Name == def.Name {
			continue
		}
		entry.Routes = append(entry.Routes, router.Route{
			HostPrefix:    s.Name + ".",
			Path:          "/",
			Upstream:      upstream(s.Name, s.Port),
			Service:       s.Name,
			RewriteHost:   s.RewriteHost,
			WebsocketPath: s.WebsocketPath,
		})
	}

	sort.SliceStable(entry.Routes, func(i, j int) bool {
		if entry.Routes[i].HostPrefix != entry.Routes[j].HostPrefix {
			return entry.Routes[i].HostPrefix < entry.Routes[j].HostPrefix
		}
		return entry.Routes[i].Path < entry.Routes[j].Path
	})
	return entry
}

func routerState(s registry.State) router.EnvState {
	switch s {
	case registry.StateRunning:
		return router.StateRunning
	case registry.StatePaused:
		return router.StatePaused
	case registry.StateStopped:
		return router.StateStopped
	case registry.StateFailed, registry.StateMissing:
		return router.StateFailed
	default:
		return router.StateCreating
	}
}
