package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/meta"
)

var AgentDir = "." + meta.Name

const AgentFile = "AGENT.md"

func WriteAgentDoc(root string, cfg *config.Config) (string, error) {
	dir := filepath.Join(root, AgentDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, AgentFile)
	if err := os.WriteFile(path, []byte(agentDoc(cfg)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func agentDoc(cfg *config.Config) string {
	var b strings.Builder
	name := meta.Name

	fmt.Fprintf(&b, "# Working inside a %s environment\n\n", name)
	fmt.Fprintf(&b, "This worktree has its own running copy of the stack: its own containers,\n"+
		"its own database, its own URLs. Nothing you do here touches another branch.\n\n")

	b.WriteString("## Reaching it\n\n")
	b.WriteString("Every service is reached by hostname through a shared router. **Never publish\n" +
		"a host port** — a second environment would collide with it, which is the one\n" +
		"thing that breaks this setup.\n\n")
	if svcs := cfg.RoutableServices(); len(svcs) > 0 {
		b.WriteString("Your URLs are in the `" + meta.EnvVarName("URL", "<SERVICE>") + "` variables, " +
			"and `" + name + " url <service>` prints one:\n\n```\n")
		for _, s := range svcs {
			fmt.Fprintf(&b, "%s url %s\n", name, s.Name)
		}
		b.WriteString("```\n\n")
	}
	if tcp := cfg.TCPServices(); len(tcp) > 0 {
		b.WriteString("Database clients connect to the loopback addresses in `" + name + " status`.\n\n")
	}

	b.WriteString("## Commands\n\n```\n")
	fmt.Fprintf(&b, "%-34s %s\n", name+" status", "what is running, and its URLs")
	fmt.Fprintf(&b, "%-34s %s\n", name+" logs <service> [-f]", "recent output")
	fmt.Fprintf(&b, "%-34s %s\n", name+" exec <service> -- <cmd>", "run a command in a container")
	fmt.Fprintf(&b, "%-34s %s\n", name+" restart [service]", "restart the stack or one service")
	if len(cfg.Stateful) > 0 {
		fmt.Fprintf(&b, "%-34s %s\n", name+" db prepare", "apply migrations you just wrote")
		fmt.Fprintf(&b, "%-34s %s\n", name+" db reset", "start over from the shared snapshot")
		fmt.Fprintf(&b, "%-34s %s\n", name+" db save <name>", "checkpoint before something risky")
	}
	b.WriteString("```\n\n")

	if len(cfg.Stateful) > 0 {
		b.WriteString("## The database\n\n")
		b.WriteString("It was **cloned from a prepared snapshot**, not migrated from scratch, and\n" +
			"then your branch's own migrations were applied on top.\n\n")
		b.WriteString("- After writing a migration, run `" + name + " db prepare`. It is quick.\n")
		b.WriteString("- If the data gets into a state you do not want, `" + name + " db reset`\n" +
			"  rebuilds it from the snapshot in seconds. Prefer that to hand-repairing rows.\n")
		b.WriteString("- Your migrate command must stay idempotent: running it twice must be a\n" +
			"  no-op, because that is what makes the snapshot reusable.\n\n")
	}

	b.WriteString("## Checking your work in the real app\n\n")
	if d, ok := cfg.DefaultService(); ok {
		fmt.Fprintf(&b, "Open the URL from `%s url %s` rather than starting a server yourself.\n",
			name, d.Name)
	}
	b.WriteString("The stack is already running; if something looks stale, `" + name +
		" restart <service>` is faster than rebuilding.\n\n")

	b.WriteString("## What not to do\n\n")
	b.WriteString("- Do not add `ports:` to the compose file.\n")
	b.WriteString("- Do not run `docker compose` directly here: " + name +
		" runs a generated, isolated copy of the compose file, and the one in the\n" +
		"  repository is not what is running.\n")
	b.WriteString("- Do not touch another environment's containers or volumes.\n")
	return b.String()
}

func IncludeLine() string {
	return fmt.Sprintf("\n@%s/%s\n", AgentDir, AgentFile)
}

func AppendInclude(path string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	marker := AgentDir + "/" + AgentFile
	if strings.Contains(string(existing), marker) {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.WriteString(IncludeLine()); err != nil {
		return false, err
	}
	return true, nil
}

const ClaudeSettingsPath = ".claude/settings.json"

func WriteClaudeSettings(root string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(ClaudeSettingsPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &settings); err != nil {
			return "", fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	hooks["WorktreeCreate"] = hookEntry(meta.Name + " hook worktree-create")
	hooks["WorktreeRemove"] = hookEntry(meta.Name + " hook worktree-remove")
	settings["hooks"] = hooks

	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func hookEntry(command string) []any {
	return []any{
		map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": command},
			},
		},
	}
}

func MCPCommand() []string {
	return []string{"claude", "mcp", "add", meta.Name, "--", meta.Name, "mcp"}
}

func GitignoreEntries(cfg *config.Config) []string {
	out := []string{}
	if cfg != nil {
		if dir := cfg.Worktree.Dir; strings.HasPrefix(dir, "../") {
			_ = dir
		} else if dir != "" {
			out = append(out, "/"+strings.TrimPrefix(dir, "./"))
		}
	}
	sort.Strings(out)
	return out
}
