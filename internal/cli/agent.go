package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/hooks"
	"github.com/Muronuch/grove/internal/mcp"
	"github.com/Muronuch/grove/internal/meta"
)

func newMCPCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server for the env in the current directory",
		Long: `mcp speaks the Model Context Protocol over stdin and stdout.

It resolves its environment from the working directory once, at startup, and
every tool it offers acts on that environment alone. There is no tool that
addresses another environment, runs a command on the host, or touches Docker.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			c.Progress = nil

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			srv, err := mcp.New(ctx, c, cwd)
			if err != nil {
				return classify(err)
			}
			return srv.Serve(ctx)
		},
	}
}

func newHookCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Claude Code hook handlers",
		Hidden: true,
		Long: `Handlers for Claude Code's worktree hooks, so that ` + "`claude --worktree <name>`" + `
also produces a running environment.

The main path is the other way round: ` + meta.Name + ` new <branch> creates the worktree and
starts the agent in it.`,
	}
	cmd.AddCommand(hookCreateCmd(app), hookRemoveCmd(app))
	return cmd
}

func hookCreateCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "worktree-create",
		Short: "Create a worktree and its environment (reads the hook JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			c.Progress = app.Err()
			c.Trust = true

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			if err := hooks.Create(ctx, c, cwd, os.Stdin, app.Out(), app.Err()); err != nil {
				return classify(err)
			}
			return nil
		},
	}
}

func hookRemoveCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "worktree-remove",
		Short: "Remove the environment of a worktree (reads the hook JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			c.Progress = app.Err()

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			if err := hooks.Remove(ctx, c, cwd, os.Stdin, app.Err()); err != nil {
				return classify(err)
			}
			return nil
		},
	}
}

func writeAgentIntegration(app *App, root string, cfg *config.Config, claude bool) ([]string, error) {
	var written []string

	doc, err := hooks.WriteAgentDoc(root, cfg)
	if err != nil {
		return written, err
	}
	written = append(written, rel(root, doc))

	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		added, err := hooks.AppendInclude(path)
		if err != nil {
			return written, err
		}
		if added {
			written = append(written, rel(root, path))
		}
	}

	if !claude {
		return written, nil
	}

	settings, err := hooks.WriteClaudeSettings(root)
	if err != nil {
		return written, err
	}
	written = append(written, rel(root, settings))

	argv := hooks.MCPCommand()
	if _, err := exec.LookPath(argv[0]); err != nil {
		app.hint("register the MCP server yourself: %s", strings.Join(argv, " "))
		return written, nil
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err != nil {
		app.Warn("could not register the MCP server (%v); run it yourself:\n    %s",
			err, strings.Join(argv, " "))
		if len(out) > 0 {
			fmt.Fprintln(app.Err(), strings.TrimSpace(string(out)))
		}
		return written, nil
	}
	app.Step("registered the MCP server with Claude Code")
	return written, nil
}
