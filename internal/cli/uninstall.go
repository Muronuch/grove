package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
)

func newUninstallCmd(app *App) *cobra.Command {
	var (
		yes       bool
		keepState bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove every " + meta.Name + " resource from this machine",
		Long: `uninstall deletes every container, network and volume ` + meta.Name + ` created —
including every env's database — and empties the state directory.

Worktrees are left on disk: they hold your code. The command lists them so you
can remove them yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			store, err := app.Store()
			if err != nil {
				return err
			}
			reg, err := store.Read()
			if err != nil {
				return err
			}
			worktrees := c.WorktreesOf(reg)

			if !yes {
				report, err := c.GC(ctx, nil, envctl.GCOptions{DryRun: true, AllProjects: true})
				_ = report
				if err != nil {
					return classify(err)
				}
				fmt.Fprintf(app.Err(), "This removes every %s container, network and volume on this machine,\n"+
					"including all env databases and cached snapshots.\n", meta.Name)
				if len(worktrees) > 0 {
					fmt.Fprintf(app.Err(), "\nThese worktrees are kept, and are yours to remove:\n")
					for _, w := range worktrees {
						fmt.Fprintf(app.Err(), "  %s\n", w)
					}
				}
				ok, err := confirm(app, "\nProceed?")
				if err != nil {
					return err
				}
				if !ok {
					return exitf(ExitGeneric, "aborted")
				}
			}

			report, err := c.Uninstall(ctx, keepState)
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				report.Schema = envctl.Schema
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": true,
					"removed": report.Removed, "worktrees_kept": worktrees,
				})
			}
			app.Printf("removed %d resources\n", len(report.Removed))
			for _, w := range worktrees {
				app.Printf("kept %s\n", w)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	f.BoolVar(&keepState, "keep-state", false, "leave the state directory in place")
	return cmd
}
