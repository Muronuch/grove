package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muronuch/grove/internal/config"
)

func fixture(t *testing.T, dir string, copy, tmpl []string) *Project {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{Root: root}
	cfg.Project.Name = "demo"
	cfg.Worktree.Dir = dir
	cfg.Worktree.Copy = copy
	cfg.Worktree.Template = tmpl
	cfg.Router.BaseDomain = "localhost"
	return &Project{Config: cfg, Root: root}
}

func TestEnsureWorktreeBaseSelfIgnores(t *testing.T) {
	p := fixture(t, "worktrees", nil, nil)
	if err := p.EnsureWorktreeBase(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(p.Root, "worktrees", ".gitignore"))
	if err != nil {
		t.Fatalf("a worktree directory inside the repo must ignore itself: %v", err)
	}
	if strings.TrimSpace(string(body)) != "*" {
		t.Errorf("worktrees/.gitignore = %q, want \"*\"", body)
	}
}

func TestEnsureWorktreeBaseLeavesExistingIgnoreAlone(t *testing.T) {
	p := fixture(t, "worktrees", nil, nil)
	base := filepath.Join(p.Root, "worktrees")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".gitignore"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureWorktreeBase(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(base, ".gitignore"))
	if strings.TrimSpace(string(body)) != "mine" {
		t.Errorf("an existing .gitignore was overwritten: %q", body)
	}
}

func TestEnsureWorktreeBaseOutsideRepoWritesNothing(t *testing.T) {
	p := fixture(t, "../elsewhere", nil, nil)
	if err := p.EnsureWorktreeBase(); err != nil {
		t.Fatal(err)
	}
	base, err := p.WorktreeBase()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, ".gitignore")); err == nil {
		t.Error("grove wrote a .gitignore outside the repository")
	}
}

func TestCopyFilesVerbatimAndTemplated(t *testing.T) {
	p := fixture(t, "worktrees", []string{".env"}, []string{".env.grove"})
	write(t, p.Root, ".env", "SECRET=abc\nLITERAL={{ not_a_template }}\n")
	write(t, p.Root, ".env.grove", "DB=app_{{ .Slug }}\nSLOT={{ .Slot }}\n")

	dst := t.TempDir()
	id := config.EnvIdentity{Project: "demo", Slug: "feat-x", Slot: 3, Repo: "demo"}
	placed, err := p.CopyFiles(dst, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 2 {
		t.Fatalf("placed %v, want both entries", placed)
	}

	if got := read(t, dst, ".env"); !strings.Contains(got, "{{ not_a_template }}") {
		t.Errorf("worktree.copy rendered a file it should have copied: %q", got)
	}
	got := read(t, dst, ".env.grove")
	if !strings.Contains(got, "DB=app_feat-x") || !strings.Contains(got, "SLOT=3") {
		t.Errorf("worktree.template did not render per env: %q", got)
	}
}

func TestCopyFilesSkipsMissing(t *testing.T) {
	p := fixture(t, "worktrees", []string{".env", ".envrc"}, nil)
	write(t, p.Root, ".env", "A=1\n")

	placed, err := p.CopyFiles(t.TempDir(), config.EnvIdentity{Project: "demo", Slug: "s", Slot: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 || placed[0] != ".env" {
		t.Errorf("placed %v, want only .env", placed)
	}
}

func TestCopyFilesRejectsEscapingPaths(t *testing.T) {
	p := fixture(t, "worktrees", []string{"../outside"}, nil)
	if _, err := p.CopyFiles(t.TempDir(), config.EnvIdentity{Project: "demo", Slug: "s", Slot: 1}); err == nil {
		t.Fatal("a path escaping the repository was accepted")
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
