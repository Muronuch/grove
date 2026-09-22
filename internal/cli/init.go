package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/transform"
)

func newInitCmd(app *App) *cobra.Command {
	var (
		yes    bool
		force  bool
		claude bool
		name   string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Inspect the compose file and write a " + config.FileName + " draft",
		Long: `init reads the project's compose file and writes a commented ` + config.FileName + `.

Everything it can infer is filled in; everything it cannot — above all how the
project migrates and seeds its database — is left as a TODO. Run ` + meta.Name + ` doctor
afterwards to check the result against two real environments.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, app, initOptions{yes: yes, force: force, claude: claude, name: name})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "accept the draft without prompting")
	f.BoolVar(&force, "force", false, "overwrite an existing "+config.FileName)
	f.BoolVar(&claude, "claude", false, "also register the worktree hooks and the MCP server with Claude Code")
	f.StringVar(&name, "name", "", "project name (default: the repository directory name)")
	return cmd
}

type initOptions struct {
	yes    bool
	force  bool
	claude bool
	name   string
}

func runInit(cmd *cobra.Command, app *App, o initOptions) error {
	ctx := cmd.Context()
	cwd, err := app.Cwd()
	if err != nil {
		return err
	}

	git := project.Git{Dir: cwd}
	root, err := git.TopLevel(ctx)
	if err != nil {
		return fmt.Errorf("%s init must run inside a git repository: %w", meta.Name, err)
	}
	target := filepath.Join(root, config.FileName)
	if _, err := os.Stat(target); err == nil && !o.force {
		return exitf(ExitGeneric, "%s already exists; pass --force to overwrite it", rel(cwd, target))
	}

	projectName := o.name
	if projectName == "" {
		projectName = filepath.Base(root)
	}

	app.Step("reading the compose project in %s", rel(cwd, root))
	p, err := transform.Load(ctx, transform.LoadOptions{
		WorkingDir: root,
		Name:       "grove-init-probe",
	})
	if err != nil {
		return fmt.Errorf("%w\n\nIf the stack needs a profile or a specific file, run init from the\ndirectory holding the compose file, or set COMPOSE_FILE first", err)
	}
	if len(p.Services) == 0 {
		return exitf(ExitGeneric, "the compose project in %s declares no services", root)
	}

	files := make([]string, 0, len(p.ComposeFiles))
	for _, f := range p.ComposeFiles {
		files = append(files, rel(root, f))
	}
	draft := transform.Draft(p, transform.DraftOptions{
		ProjectName:   projectName,
		Repo:          root,
		DefaultBranch: git.DefaultBranch(ctx, ""),
		ComposeFiles:  files,
	})

	cfg, err := config.Parse(draft, target)
	if err != nil {
		return fmt.Errorf("the generated draft is not valid (this is a bug in %s init):\n%w", meta.Name, err)
	}

	if !o.yes && !app.JSONOut {
		fmt.Fprintf(app.Err(), "\n%s\n", strings.TrimRight(string(draft), "\n"))
		ok, err := confirm(app, fmt.Sprintf("\nWrite this to %s?", rel(cwd, target)))
		if err != nil {
			return err
		}
		if !ok {
			return exitf(ExitGeneric, "aborted")
		}
	}

	if err := os.WriteFile(target, draft, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}

	written := []string{rel(cwd, target)}
	extra, err := writeAgentIntegration(app, root, cfg, o.claude)
	if err != nil {
		return err
	}
	written = append(written, extra...)

	if app.JSONOut {
		return app.WriteJSON(map[string]any{
			"schema":  1,
			"ok":      true,
			"written": written,
			"project": cfg.Project.Name,
			"todo":    draftTODOs(draft),
		})
	}

	for _, w := range written {
		app.Printf("%s\n", w)
	}
	todos := draftTODOs(draft)
	if len(todos) > 0 {
		fmt.Fprintf(app.Err(), "\n%s %d thing(s) grove could not guess:\n", app.yellow("TODO:"), len(todos))
		for _, t := range todos {
			fmt.Fprintf(app.Err(), "  %s:%d  %s\n", config.FileName, t.line, t.text)
		}
	}
	fmt.Fprintf(app.Err(), "\nNext: %s doctor\n", meta.Name)
	return nil
}

type todo struct {
	line int
	text string
}

func draftTODOs(draft []byte) []todo {
	var out []todo
	for i, line := range strings.Split(string(draft), "\n") {
		idx := strings.Index(line, "TODO:")
		if idx < 0 {
			continue
		}
		out = append(out, todo{line: i + 1, text: strings.TrimSpace(line[idx+len("TODO:"):])})
	}
	return out
}

func confirm(app *App, question string) (bool, error) {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("stdin is not a terminal; pass --yes to accept without prompting")
	}
	fmt.Fprintf(app.Err(), "%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

func rel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil || strings.HasPrefix(r, "..") || len(r) >= len(path) {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}
