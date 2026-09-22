package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/registry"
)

const IndexSchema = 1

type Entry struct {
	Key           string        `json:"key"`
	Project       string        `json:"project"`
	Service       string        `json:"service"`
	Volume        string        `json:"volume"`
	Kind          Kind          `json:"kind"`
	Name          string        `json:"name,omitempty"`
	Env           string        `json:"env,omitempty"`
	SizeBytes     int64         `json:"size_bytes"`
	CreatedAt     time.Time     `json:"created_at"`
	LastUsed      time.Time     `json:"last_used"`
	UseCount      int           `json:"use_count"`
	BuiltFrom     string        `json:"built_from,omitempty"`
	BuildDuration time.Duration `json:"build_duration_ns,omitempty"`
	Inputs        int           `json:"inputs,omitempty"`
}

func (e Entry) Ref() Ref { return Ref{Project: e.Project, Service: e.Service, Key: e.Key} }

type Index struct {
	Schema    int     `json:"schema"`
	Snapshots []Entry `json:"snapshots"`
	dirty     bool
}

type Store struct {
	reg *registry.Store
}

func NewStore(reg *registry.Store) *Store { return &Store{reg: reg} }

func (s *Store) path() string { return s.reg.Path("snapshots.json") }

func (s *Store) lock() (*registry.Lock, error) {
	return registry.NewLock(s.reg.Path("locks", "snapshots.lock"), "the snapshot index")
}

func (s *Store) Read() (*Index, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return &Index{Schema: IndexSchema}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path(), err)
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("parse %s: %w (delete the file to rebuild it)", s.path(), err)
	}
	if idx.Schema == 0 {
		idx.Schema = IndexSchema
	}
	return &idx, nil
}

func (s *Store) Update(ctx context.Context, fn func(*Index) error) error {
	lock, err := s.lock()
	if err != nil {
		return err
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	idx, err := s.Read()
	if err != nil {
		return err
	}
	if err := fn(idx); err != nil {
		return err
	}
	if !idx.dirty {
		return nil
	}
	return s.write(idx)
}

func (s *Store) write(idx *Index) error {
	idx.Schema = IndexSchema
	sort.Slice(idx.Snapshots, func(i, j int) bool {
		a, b := idx.Snapshots[i], idx.Snapshots[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path()), "snapshots-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := registry.ReplaceFile(name, s.path()); err != nil {
		return fmt.Errorf("write %s: %w", s.path(), err)
	}
	idx.dirty = false
	return nil
}

func (i *Index) Touch() { i.dirty = true }

func (i *Index) Find(ref Ref) (*Entry, bool) {
	for n := range i.Snapshots {
		e := &i.Snapshots[n]
		if e.Key == ref.Key && e.Project == ref.Project && e.Service == ref.Service {
			return e, true
		}
	}
	return nil, false
}

func (i *Index) Put(e Entry) {
	if old, ok := i.Find(e.Ref()); ok {
		if e.UseCount == 0 {
			e.UseCount = old.UseCount
		}
		if e.LastUsed.IsZero() {
			e.LastUsed = old.LastUsed
		}
		*old = e
		i.dirty = true
		return
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.LastUsed.IsZero() {
		e.LastUsed = e.CreatedAt
	}
	i.Snapshots = append(i.Snapshots, e)
	i.dirty = true
}

func (i *Index) Used(ref Ref) {
	if e, ok := i.Find(ref); ok {
		e.LastUsed = time.Now().UTC()
		e.UseCount++
		i.dirty = true
	}
}

func (i *Index) Delete(ref Ref) {
	for n := range i.Snapshots {
		if i.Snapshots[n].Key == ref.Key && i.Snapshots[n].Project == ref.Project && i.Snapshots[n].Service == ref.Service {
			i.Snapshots = append(i.Snapshots[:n], i.Snapshots[n+1:]...)
			i.dirty = true
			return
		}
	}
}

func (i *Index) ForProject(project string) []Entry {
	var out []Entry
	for _, e := range i.Snapshots {
		if e.Project == project {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

func (i *Index) Checkpoints(project, env string) []Entry {
	var out []Entry
	for _, e := range i.Snapshots {
		if e.Project == project && e.Env == env && e.Kind == KindCheckpoint {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

func (i *Index) Collectable(now time.Time, after time.Duration, keepGoldens int, inUse map[string]bool) []Entry {
	type group struct{ project, service string }
	goldens := map[group][]Entry{}
	for _, e := range i.Snapshots {
		if e.Kind == KindGolden {
			g := group{e.Project, e.Service}
			goldens[g] = append(goldens[g], e)
		}
	}
	keep := map[string]bool{}
	for g, list := range goldens {
		sort.Slice(list, func(a, b int) bool { return list[a].CreatedAt.After(list[b].CreatedAt) })
		for n := 0; n < len(list) && n < keepGoldens; n++ {
			keep[list[n].Volume] = true
		}
		_ = g
	}

	var out []Entry
	for _, e := range i.Snapshots {
		switch {
		case e.Kind == KindCheckpoint:

			continue
		case keep[e.Volume], inUse[e.Volume]:
			continue
		case now.Sub(e.LastUsed) < after:
			continue
		}
		out = append(out, e)
	}
	return out
}
