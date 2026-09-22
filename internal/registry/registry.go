package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/meta"
)

const Schema = 1

var ErrEnvNotFound = errors.New("env not found")

var ErrNoSlots = errors.New("no free slot")

type State string

const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateStopped  State = "stopped"
	StateMissing  State = "missing"
	StateFailed   State = "failed"
	StateRemoving State = "removing"
)

func (s State) Live() bool {
	switch s {
	case StateRunning, StatePaused, StateStopped:
		return true
	}
	return false
}

type Registry struct {
	Schema   int                      `json:"schema"`
	Router   Router                   `json:"router"`
	Projects map[string]*ProjectState `json:"projects"`
	dirty    bool
}

type Router struct {
	Port          int    `json:"port"`
	AdminPort     int    `json:"admin_port"`
	Version       string `json:"version"`
	Image         string `json:"image"`
	RoutesVersion int64  `json:"routes_version"`
}

type ProjectState struct {
	Name         string          `json:"name"`
	Root         string          `json:"root"`
	Envs         map[string]*Env `json:"envs"`
	TrustedHooks []string        `json:"trusted_hooks,omitempty"`
}

type Env struct {
	Slug         string                 `json:"env"`
	Project      string                 `json:"project"`
	Branch       string                 `json:"branch"`
	Slot         int                    `json:"slot"`
	Worktree     string                 `json:"worktree"`
	State        State                  `json:"state"`
	Pinned       bool                   `json:"pinned"`
	Headless     bool                   `json:"headless"`
	OwnsWorktree bool                   `json:"owns_worktree"`
	CreatedAt    time.Time              `json:"created_at"`
	LastActivity time.Time              `json:"last_activity"`
	HoldUntil    time.Time              `json:"hold_until,omitzero"`
	ComposeFile  string                 `json:"compose_file,omitempty"`
	Networks     []string               `json:"networks,omitempty"`
	TCP          map[string]string      `json:"tcp,omitempty"`
	Snapshots    map[string]SnapshotRef `json:"snapshots,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

type SnapshotRef struct {
	Key    string `json:"key"`
	Source string `json:"source"`
}

func (e *Env) ComposeProject() string { return meta.ComposeProject(e.Project, e.Slug) }

func (e *Env) Held(now time.Time) bool {
	return !e.HoldUntil.IsZero() && e.HoldUntil.After(now)
}

func (e *Env) IdleFor(now time.Time) time.Duration {
	if e.LastActivity.IsZero() {
		return now.Sub(e.CreatedAt)
	}
	return now.Sub(e.LastActivity)
}

type Store struct {
	home string
}

func DefaultHome() (string, error) {
	if v := os.Getenv(envHomeVar()); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", envHomeVar(), v)
		}
		return filepath.Clean(v), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory (set %s): %w", envHomeVar(), err)
	}
	return filepath.Join(h, "."+meta.Name), nil
}

func envHomeVar() string { return meta.EnvVarName("HOME") }

func Open(home string) (*Store, error) {
	if home == "" {
		h, err := DefaultHome()
		if err != nil {
			return nil, err
		}
		home = h
	}
	for _, d := range []string{home, filepath.Join(home, "locks"), filepath.Join(home, "envs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create state directory %s: %w", d, err)
		}
	}
	return &Store{home: home}, nil
}

func (s *Store) Home() string { return s.home }

func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.home}, parts...)...)
}

func (s *Store) EnvDir(project, slug string) string {
	return s.Path("envs", project, slug)
}

func (s *Store) file() string { return s.Path("registry.json") }

func (s *Store) registryLock() (*Lock, error) {
	return NewLock(s.Path("locks", "registry.lock"), "the registry")
}

func (s *Store) EnvLock(project, slug string) (*Lock, error) {
	return NewLock(s.Path("locks", fmt.Sprintf("env-%s-%s.lock", project, slug)), "env "+slug)
}

func (s *Store) SnapshotLock(key string) (*Lock, error) {
	return NewLock(s.Path("locks", "snap-"+meta.SnapshotKeyShort(key)+".lock"), "snapshot "+meta.SnapshotKeyShort(key))
}

func (s *Store) Read() (*Registry, error) {
	b, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return newRegistry(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.file(), err)
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete the file to start over)", s.file(), err)
	}
	if r.Projects == nil {
		r.Projects = map[string]*ProjectState{}
	}
	for name, p := range r.Projects {
		if p.Envs == nil {
			p.Envs = map[string]*Env{}
		}
		for slug, e := range p.Envs {
			e.Slug = slug
			e.Project = name
		}
	}
	if r.Schema == 0 {
		r.Schema = Schema
	}
	return &r, nil
}

func newRegistry() *Registry {
	return &Registry{
		Schema:   Schema,
		Projects: map[string]*ProjectState{},
		Router: Router{
			Port:      meta.DefaultRouterPort,
			AdminPort: meta.DefaultRouterAdminPort,
		},
	}
}

func (s *Store) Update(ctx context.Context, fn func(*Registry) error) error {
	lock, err := s.registryLock()
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, LockTimeout); err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	r, err := s.Read()
	if err != nil {
		return err
	}
	if err := fn(r); err != nil {
		return err
	}
	if !r.dirty {
		return nil
	}
	return s.write(r)
}

func (s *Store) write(r *Registry) error {
	r.Schema = Schema
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(s.home, "registry-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFileRetry(name, s.file()); err != nil {
		return fmt.Errorf("write %s: %w", s.file(), err)
	}
	r.dirty = false
	return nil
}

func replaceFileRetry(tmp, dst string) error {
	const attempts = 20
	var err error
	for i := range attempts {
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(time.Duration(10+5*i) * time.Millisecond)
		}
	}
	return err
}

func (r *Registry) Touch() { r.dirty = true }

func (r *Registry) Dirty() bool { return r.dirty }

func (r *Registry) Project(name, root string) *ProjectState {
	if r.Projects == nil {
		r.Projects = map[string]*ProjectState{}
	}
	p, ok := r.Projects[name]
	if !ok {
		p = &ProjectState{Name: name, Root: root, Envs: map[string]*Env{}}
		r.Projects[name] = p
		r.dirty = true
		return p
	}
	if p.Envs == nil {
		p.Envs = map[string]*Env{}
	}
	if root != "" && p.Root != root {
		p.Root = root
		r.dirty = true
	}
	return p
}

func (r *Registry) LookupProject(name string) (*ProjectState, bool) {
	p, ok := r.Projects[name]
	return p, ok
}

func (r *Registry) Env(project, slug string) (*Env, error) {
	p, ok := r.Projects[project]
	if !ok {
		return nil, fmt.Errorf("%w: project %q has no envs", ErrEnvNotFound, project)
	}
	e, ok := p.Envs[slug]
	if !ok {
		return nil, fmt.Errorf("%w: %q in project %q (see `%s ls`)", ErrEnvNotFound, slug, project, meta.Name)
	}
	return e, nil
}

func (p *ProjectState) List() []*Env {
	out := make([]*Env, 0, len(p.Envs))
	for _, e := range p.Envs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

func (r *Registry) All() []*Env {
	names := make([]string, 0, len(r.Projects))
	for n := range r.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*Env
	for _, n := range names {
		out = append(out, r.Projects[n].List()...)
	}
	return out
}

func (p *ProjectState) UsedSlots() map[int]bool {
	used := map[int]bool{}
	for _, e := range p.Envs {
		if e.Slot > 0 {
			used[e.Slot] = true
		}
	}
	return used
}

func (p *ProjectState) SlugTaken(slug, branch string) bool {
	e, ok := p.Envs[slug]
	return ok && e.Branch != branch
}

func (r *Registry) FindByWorktree(dir string) (*Env, bool) {
	dir = filepath.Clean(dir)
	var best *Env
	bestLen := -1
	for _, e := range r.All() {
		if e.Worktree == "" {
			continue
		}
		wt := filepath.Clean(e.Worktree)
		if !underOrEqual(dir, wt) {
			continue
		}
		if len(wt) > bestLen {
			best, bestLen = e, len(wt)
		}
	}
	return best, best != nil
}

func underOrEqual(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !hasDotDotPrefix(rel))
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && (rel[2] == filepath.Separator || rel[2] == '/')
}

func (r *Registry) NextRoutesVersion() int64 {
	r.Router.RoutesVersion++
	r.dirty = true
	return r.Router.RoutesVersion
}

func (s *Store) Wipe() error {
	entries, err := os.ReadDir(s.home)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.home, e.Name())); err != nil {
			return fmt.Errorf("remove %s: %w", filepath.Join(s.home, e.Name()), err)
		}
	}
	return nil
}
