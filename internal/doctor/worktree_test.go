package doctor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWorktreeInsideBuildContextIsReported(t *testing.T) {
	dir := fixture(t, "go-react-pg", "")
	buildFromRoot(t, dir)

	codes := runStatic(t, dir).Findings.Codes()
	if !slices.Contains(codes, "W_WORKTREE_IN_BUILD_CONTEXT") {
		t.Errorf("codes = %v, want W_WORKTREE_IN_BUILD_CONTEXT", codes)
	}
}

func TestWorktreeInBuildContextIsSilentWhenDockerignored(t *testing.T) {
	dir := fixture(t, "go-react-pg", "")
	buildFromRoot(t, dir)
	write(t, dir, ".dockerignore", "node_modules\nworktrees\n")

	codes := runStatic(t, dir).Findings.Codes()
	if slices.Contains(codes, "W_WORKTREE_IN_BUILD_CONTEXT") {
		t.Errorf("a .dockerignore entry should silence the finding; got %v", codes)
	}
}

func TestCopyingATrackedFileIsReported(t *testing.T) {
	dir := fixture(t, "go-react-pg", "")
	appendToConfig(t, dir, "\n[worktree]\ndir = \"worktrees\"\ncopy = [\"docker-compose.yml\"]\n")

	codes := runStatic(t, dir).Findings.Codes()
	if !slices.Contains(codes, "W_COPY_TRACKED_FILE") {
		t.Errorf("codes = %v, want W_COPY_TRACKED_FILE", codes)
	}
}

func TestCopyingAnUntrackedFileIsSilent(t *testing.T) {
	dir := fixture(t, "go-react-pg", "")
	appendToConfig(t, dir, "\n[worktree]\ndir = \"worktrees\"\ncopy = [\".env\"]\n")
	write(t, dir, ".env", "SECRET=1\n")

	codes := runStatic(t, dir).Findings.Codes()
	if slices.Contains(codes, "W_COPY_TRACKED_FILE") {
		t.Errorf("an untracked file must not be reported; got %v", codes)
	}
}

func buildFromRoot(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "docker-compose.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(body), "build: ./api", "build: .", 1)
	if out == string(body) {
		t.Fatal("fixture no longer has the api build context this test rewrites")
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendToConfig(t *testing.T, dir, block string) {
	t.Helper()
	path := filepath.Join(dir, "grove.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(body), "[worktree]\ndir = \"worktrees\"\ncopy = []\n", "", 1)
	if out == string(body) {
		t.Fatal("fixture no longer has the [worktree] block this test replaces")
	}
	if err := os.WriteFile(path, []byte(out+block), 0o644); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
