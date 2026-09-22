package router

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Table struct {
	Version   int64      `json:"version"`
	UpdatedAt time.Time  `json:"updated_at"`
	Envs      []EnvRoute `json:"envs"`
}

type EnvState string

const (
	StateRunning  EnvState = "running"
	StatePaused   EnvState = "paused"
	StateStopped  EnvState = "stopped"
	StateCreating EnvState = "creating"
	StateFailed   EnvState = "failed"
)

func (s EnvState) Asleep() bool { return s == StatePaused || s == StateStopped }

type EnvRoute struct {
	Project   string            `json:"project"`
	Env       string            `json:"env"`
	Slot      int               `json:"slot"`
	State     EnvState          `json:"state"`
	Hosts     []string          `json:"hosts"`
	Routes    []Route           `json:"routes"`
	URLs      map[string]string `json:"urls,omitempty"`
	LastError string            `json:"last_error,omitempty"`
}

type Route struct {
	HostPrefix    string `json:"host_prefix"`
	Path          string `json:"path"`
	Upstream      string `json:"upstream"`
	Service       string `json:"service,omitempty"`
	RewriteHost   string `json:"rewrite_host,omitempty"`
	WebsocketPath string `json:"websocket_path,omitempty"`
}

type Match struct {
	Env   *EnvRoute
	Route *Route
}

type index struct {
	table    Table
	hosts    map[string]*hostEntry
	projects map[string][]*EnvRoute
}

type hostEntry struct {
	env    *EnvRoute
	routes []*Route
}

type Store struct {
	mu  sync.RWMutex
	idx *index
}

func NewStore() *Store { return &Store{idx: buildIndex(Table{})} }

func (s *Store) Replace(t Table) (accepted bool, current int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Version < s.idx.table.Version {
		return false, s.idx.table.Version
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now().UTC()
	}
	s.idx = buildIndex(t)
	return true, t.Version
}

func (s *Store) Table() Table {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.table
}

func (s *Store) SetState(project, env string, state EnvState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.idx.table
	for i := range t.Envs {
		if t.Envs[i].Project == project && t.Envs[i].Env == env {
			t.Envs[i].State = state
			s.idx = buildIndex(t)
			return true
		}
	}
	return false
}

func (s *Store) Lookup(host, path string) (Match, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.idx.hosts[normaliseHost(host)]
	if !ok {
		return Match{}, false
	}
	var best *Route
	bestLen := -1
	for _, r := range entry.routes {
		if !pathMatches(path, r.Path) {
			continue
		}
		if len(r.Path) > bestLen {
			best, bestLen = r, len(r.Path)
		}
	}
	if best == nil {
		return Match{Env: entry.env}, false
	}
	return Match{Env: entry.env, Route: best}, true
}

func (s *Store) EnvForHost(host string) (*EnvRoute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.idx.hosts[normaliseHost(host)]
	if !ok {
		return nil, false
	}
	return entry.env, true
}

func (s *Store) ProjectEnvs(project string) []*EnvRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.projects[project]
}

func (s *Store) AllEnvs() []EnvRoute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]EnvRoute, len(s.idx.table.Envs))
	copy(out, s.idx.table.Envs)
	return out
}

func pathMatches(path, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/' || rest[0] == '?'
}

func buildIndex(t Table) *index {
	idx := &index{
		table:    t,
		hosts:    map[string]*hostEntry{},
		projects: map[string][]*EnvRoute{},
	}
	for i := range t.Envs {
		env := &t.Envs[i]
		idx.projects[env.Project] = append(idx.projects[env.Project], env)

		hosts := make([]string, 0, len(env.Hosts))
		for _, h := range env.Hosts {
			hosts = append(hosts, normaliseHost(h))
		}
		bases := baseHosts(hosts)

		for j := range env.Routes {
			r := &env.Routes[j]
			for _, base := range bases {
				host := r.HostPrefix + base
				e, ok := idx.hosts[host]
				if !ok {
					e = &hostEntry{env: env}
					idx.hosts[host] = e
				}
				e.routes = append(e.routes, r)
			}
		}

		for _, h := range hosts {
			if _, ok := idx.hosts[h]; !ok {
				idx.hosts[h] = &hostEntry{env: env}
			}
		}
	}
	for p := range idx.projects {
		sort.Slice(idx.projects[p], func(i, j int) bool {
			return idx.projects[p][i].Env < idx.projects[p][j].Env
		})
	}
	return idx
}

func baseHosts(hosts []string) []string {
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		set[h] = true
	}
	var bases []string
	for _, h := range hosts {
		if _, rest, ok := strings.Cut(h, "."); ok && set[rest] {
			continue
		}
		bases = append(bases, h)
	}
	if len(bases) == 0 {
		bases = hosts
	}
	sort.Strings(bases)
	return bases
}

func normaliseHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
}
