package state

import (
	"context"
	"testing"
	"time"

	"github.com/Muronuch/grove/internal/registry"
)

func newIndexStore(t *testing.T) *Store {
	t.Helper()
	reg, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(reg)
}

func TestIndexRoundTrip(t *testing.T) {
	s := newIndexStore(t)
	ctx := context.Background()
	ref := Ref{Project: "acme", Service: "postgres", Key: "abc123"}

	if err := s.Update(ctx, func(i *Index) error {
		i.Put(Entry{
			Key: ref.Key, Project: ref.Project, Service: ref.Service, Volume: ref.Volume(),
			Kind: KindGolden, SizeBytes: 1024,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	idx, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := idx.Find(ref)
	if !ok {
		t.Fatalf("snapshot not found: %+v", idx.Snapshots)
	}
	if e.SizeBytes != 1024 || e.Kind != KindGolden {
		t.Errorf("entry = %+v", e)
	}
	if e.CreatedAt.IsZero() || e.LastUsed.IsZero() {
		t.Error("timestamps were not filled in")
	}
}

func TestIndexUsedTracksClones(t *testing.T) {
	s := newIndexStore(t)
	ctx := context.Background()
	ref := Ref{Project: "p", Service: "db", Key: "k"}
	old := time.Now().UTC().Add(-48 * time.Hour)

	s.Update(ctx, func(i *Index) error {
		i.Put(Entry{Key: ref.Key, Project: "p", Service: "db", Volume: ref.Volume(), LastUsed: old, CreatedAt: old})
		return nil
	})
	s.Update(ctx, func(i *Index) error {
		i.Used(ref)
		return nil
	})
	idx, _ := s.Read()
	e, _ := idx.Find(ref)
	if e.UseCount != 1 {
		t.Errorf("use count = %d", e.UseCount)
	}
	if time.Since(e.LastUsed) > time.Minute {
		t.Errorf("last used was not refreshed: %v", e.LastUsed)
	}
}

func TestCollectableKeepsWhatMatters(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	idx := &Index{}

	for i, age := range []time.Duration{1, 2, 3, 4} {
		idx.Put(Entry{
			Key: string(rune('a' + i)), Project: "p", Service: "db",
			Volume: "vol-golden-" + string(rune('a'+i)), Kind: KindGolden,
			CreatedAt: now.Add(-age * 24 * time.Hour), LastUsed: old,
		})
	}
	idx.Put(Entry{Key: "branch", Project: "p", Service: "db", Volume: "vol-branch", Kind: KindBranch, LastUsed: old, CreatedAt: old})
	idx.Put(Entry{Key: "cp-x", Project: "p", Service: "db", Volume: "vol-cp", Kind: KindCheckpoint, Name: "before-demo", LastUsed: old, CreatedAt: old})
	idx.Put(Entry{Key: "fresh", Project: "p", Service: "db", Volume: "vol-fresh", Kind: KindBranch, LastUsed: now, CreatedAt: now})
	idx.Put(Entry{Key: "used", Project: "p", Service: "db", Volume: "vol-used", Kind: KindBranch, LastUsed: old, CreatedAt: old})

	got := idx.Collectable(now, 14*24*time.Hour, 3, map[string]bool{"vol-used": true})

	collected := map[string]bool{}
	for _, e := range got {
		collected[e.Volume] = true
	}

	if !collected["vol-golden-d"] {
		t.Error("the oldest golden should be collected")
	}
	for _, keep := range []string{"vol-golden-a", "vol-golden-b", "vol-golden-c"} {
		if collected[keep] {
			t.Errorf("%s should be kept (keep_goldens = 3)", keep)
		}
	}
	if !collected["vol-branch"] {
		t.Error("a stale branch snapshot should be collected")
	}
	if collected["vol-cp"] {
		t.Error("a named checkpoint must never be collected")
	}
	if collected["vol-fresh"] {
		t.Error("a recently used snapshot should be kept")
	}
	if collected["vol-used"] {
		t.Error("a snapshot a live env refers to must be kept")
	}
}

func TestCheckpointsAreScopedToOneEnv(t *testing.T) {
	idx := &Index{}
	idx.Put(Entry{Key: "cp-a-x", Project: "p", Service: "db", Env: "a", Kind: KindCheckpoint, Name: "x"})
	idx.Put(Entry{Key: "cp-b-y", Project: "p", Service: "db", Env: "b", Kind: KindCheckpoint, Name: "y"})
	idx.Put(Entry{Key: "golden", Project: "p", Service: "db", Kind: KindGolden})

	got := idx.Checkpoints("p", "a")
	if len(got) != 1 || got[0].Name != "x" {
		t.Errorf("checkpoints = %+v", got)
	}
}
