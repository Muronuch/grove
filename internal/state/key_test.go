package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Muronuch/grove/internal/config"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseInput(dir string) KeyInput {
	return KeyInput{
		Driver: "volume-copy",
		Image:  "sha256:abc",
		Prepare: config.PrepareConfig{
			Mode: config.PrepareRun, Service: "api", Command: []string{"migrate", "up"},
		},
		Worktree: dir,
		Inputs:   []string{"migrations/**", "seed/**"},
	}
}

func key(t *testing.T, in KeyInput) KeyResult {
	t.Helper()
	res, err := ComputeKey(in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestKeyIsStable(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"migrations/0001_init.sql": "create table a();",
		"migrations/0002_more.sql": "create table b();",
		"seed/data.sql":            "insert into a values ();",
		"README.md":                "not an input",
	})
	a := key(t, baseInput(dir))
	b := key(t, baseInput(dir))
	if a.Key != b.Key {
		t.Errorf("the same inputs produced different keys: %s vs %s", a.Key, b.Key)
	}
	if len(a.Files) != 3 {
		t.Errorf("files = %v, want the three matched by the globs", a.Files)
	}
	for _, f := range a.Files {
		if f == "README.md" {
			t.Error("a file outside the globs went into the key")
		}
	}
}

func TestKeyChangesWithEveryInput(t *testing.T) {
	files := map[string]string{
		"migrations/0001_init.sql": "create table a();",
		"seed/data.sql":            "insert into a values ();",
	}
	dir := writeTree(t, files)
	base := key(t, baseInput(dir)).Key

	t.Run("changed migration", func(t *testing.T) {
		d := writeTree(t, map[string]string{
			"migrations/0001_init.sql": "create table a(id int);",
			"seed/data.sql":            files["seed/data.sql"],
		})
		if key(t, baseInput(d)).Key == base {
			t.Error("editing a migration did not change the key")
		}
	})
	t.Run("new migration", func(t *testing.T) {
		d := writeTree(t, map[string]string{
			"migrations/0001_init.sql": files["migrations/0001_init.sql"],
			"migrations/0002_new.sql":  "create table b();",
			"seed/data.sql":            files["seed/data.sql"],
		})
		if key(t, baseInput(d)).Key == base {
			t.Error("adding a migration did not change the key")
		}
	})
	t.Run("different image", func(t *testing.T) {
		in := baseInput(dir)
		in.Image = "sha256:def"
		if key(t, in).Key == base {
			t.Error("a different database image did not change the key")
		}
	})
	t.Run("different command", func(t *testing.T) {
		in := baseInput(dir)
		in.Prepare.Command = []string{"migrate", "up", "--seed"}
		if key(t, in).Key == base {
			t.Error("a different prepare command did not change the key")
		}
	})
	t.Run("different mode", func(t *testing.T) {
		in := baseInput(dir)
		in.Prepare.Mode = config.PrepareExec
		if key(t, in).Key == base {
			t.Error("a different prepare mode did not change the key")
		}
	})
	t.Run("different driver", func(t *testing.T) {
		in := baseInput(dir)
		in.Driver = "reflink"
		if key(t, in).Key == base {
			t.Error("a different driver did not change the key")
		}
	})
}

func TestKeyIgnoresUnrelatedFiles(t *testing.T) {
	a := writeTree(t, map[string]string{
		"migrations/0001.sql": "x",
		"src/main.go":         "package main",
	})
	b := writeTree(t, map[string]string{
		"migrations/0001.sql": "x",
		"src/main.go":         "package main // changed",
		"docs/readme.md":      "new file",
	})
	if key(t, baseInput(a)).Key != key(t, baseInput(b)).Key {
		t.Error("a change outside the input globs changed the key")
	}
}

func TestKeyReadsTheWorkingTree(t *testing.T) {
	dir := writeTree(t, map[string]string{"migrations/0001.sql": "x"})
	before := key(t, baseInput(dir)).Key
	if err := os.WriteFile(filepath.Join(dir, "migrations", "0002_wip.sql"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if key(t, baseInput(dir)).Key == before {
		t.Error("an uncommitted migration did not change the key")
	}
}

func TestKeyWithMissingInputDirectory(t *testing.T) {
	dir := writeTree(t, map[string]string{"README.md": "x"})
	res, err := ComputeKey(baseInput(dir))
	if err != nil {
		t.Fatalf("a missing migrations directory should not be an error: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("files = %v", res.Files)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"migrations/**", "migrations/0001.sql", true},
		{"migrations/**", "migrations/pg/0001.sql", true},
		{"migrations/**", "migrations", false},
		{"migrations/**", "seed/0001.sql", false},
		{"**", "anything/at/all.sql", true},
		{"**/*.sql", "a/b/c.sql", true},
		{"**/*.sql", "a/b/c.go", false},
		{"db/*.sql", "db/x.sql", true},
		{"db/*.sql", "db/sub/x.sql", false},
		{"backend/migrations/**", "backend/migrations/x/y.sql", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/c", false},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestRefVolumeNaming(t *testing.T) {
	r := Ref{Project: "acme", Service: "postgres", Key: "abcdef0123456789abcdef"}
	if got, want := r.Volume(), "grove-snap-acme-postgres-abcdef012345"; got != want {
		t.Errorf("volume = %q, want %q", got, want)
	}
	if got := r.Short(); len(got) != 12 {
		t.Errorf("short key = %q", got)
	}
	cp := CheckpointRef("acme", "postgres", "feat-x", "Before Demo!")
	if !cp.IsCheckpoint() {
		t.Error("a checkpoint ref should report itself as one")
	}
	if got, want := cp.Key, "cp-feat-x-before-demo"; got != want {
		t.Errorf("checkpoint key = %q, want %q", got, want)
	}
}
