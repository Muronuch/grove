package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/registry"
)

func TestCreateInputAcceptsEveryKnownSpelling(t *testing.T) {
	cases := map[string]string{
		`{"name":"feat/x"}`:                    "feat/x",
		`{"worktree_name":"feat/x"}`:           "feat/x",
		`{"branch":"feat/x"}`:                  "feat/x",
		`{"branch_name":"feat/x"}`:             "feat/x",
		`{"name":" feat/x "}`:                  "feat/x",
		`{"name":"","branch":"feat/x"}`:        "feat/x",
		`{"hook_event_name":"WorktreeCreate"}`: "",
		`{}`:                                   "",
	}
	for body, want := range cases {
		var in CreateInput
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := in.branch(); got != want {
			t.Errorf("%s → %q, want %q", body, got, want)
		}
	}
}

func TestRemoveInputPath(t *testing.T) {
	cases := map[string]string{
		`{"worktree_path":"/a/b"}`:         "/a/b",
		`{"path":"/a/b"}`:                  "/a/b",
		`{"worktree_path":"","path":"/c"}`: "/c",
		`{"name":"feat/x"}`:                "",
	}
	for body, want := range cases {
		var in RemoveInput
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := in.path(); got != want {
			t.Errorf("%s → %q, want %q", body, got, want)
		}
	}
}

func TestCreateRefusesAPayloadWithNoName(t *testing.T) {
	var stdout, progress strings.Builder
	err := Create(context.Background(), &envctl.Controller{}, t.TempDir(),
		strings.NewReader(`{"hook_event_name":"WorktreeCreate"}`), &stdout, &progress)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no worktree name") {
		t.Errorf("error = %v", err)
	}
	if stdout.String() != "" {
		t.Errorf("stdout must stay empty on failure, got %q", stdout.String())
	}
}

func TestCreateRejectsNonJSON(t *testing.T) {
	var stdout, progress strings.Builder
	err := Create(context.Background(), &envctl.Controller{}, t.TempDir(),
		strings.NewReader("not json at all"), &stdout, &progress)
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v", err)
	}
	if stdout.String() != "" {
		t.Errorf("stdout must stay empty, got %q", stdout.String())
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	store, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctl := &envctl.Controller{Store: store}
	for _, body := range []string{`{}`, `{"worktree_path":"/nowhere/at/all"}`, ``} {
		var progress strings.Builder
		if err := Remove(context.Background(), ctl, t.TempDir(), strings.NewReader(body), &progress); err != nil {
			t.Errorf("payload %q: %v", body, err)
		}
		if !strings.Contains(progress.String(), "nothing to do") {
			t.Errorf("payload %q: progress = %q", body, progress.String())
		}
	}
}

func TestAgentDocCoversWhatAnAgentNeeds(t *testing.T) {
	cfg, err := config.Parse([]byte(`
[project]
name = "demo"
[[service]]
name = "web"
port = 5173
default = true
[[service]]
name = "api"
port = 8080
[[stateful]]
service = "db"
volume = "pgdata"
inputs = ["migrations/**"]
  [stateful.prepare]
  mode = "run"
  service = "api"
  command = ["migrate"]
`), "grove.toml")
	if err != nil {
		t.Fatal(err)
	}
	doc := agentDoc(cfg)
	for _, want := range []string{
		"Never publish",
		"grove url web",
		"grove db prepare",
		"grove db reset",
		"idempotent",
		"docker compose",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("AGENT.md does not mention %q", want)
		}
	}
}

func TestWriteClaudeSettingsPreservesOtherKeys(t *testing.T) {
	root := t.TempDir()
	settingsDir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`
	path := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteClaudeSettings(root); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("the settings file is no longer valid JSON: %v", err)
	}
	if out["model"] != "opus" {
		t.Error("an unrelated setting was lost")
	}
	hooks, _ := out["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("an unrelated hook was lost")
	}
	for _, want := range []string{"WorktreeCreate", "WorktreeRemove"} {
		if _, ok := hooks[want]; !ok {
			t.Errorf("%s was not registered", want)
		}
	}
	if !strings.Contains(string(b), "hook worktree-create") {
		t.Error("the create hook command is wrong")
	}
}

func TestAppendIncludeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(path, []byte("# project rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := AppendInclude(path)
	if err != nil || !added {
		t.Fatalf("first append: added=%v err=%v", added, err)
	}
	added, err = AppendInclude(path)
	if err != nil || added {
		t.Fatalf("second append should be a no-op: added=%v err=%v", added, err)
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), AgentDir+"/"+AgentFile); n != 1 {
		t.Errorf("the include line appears %d times", n)
	}
}
