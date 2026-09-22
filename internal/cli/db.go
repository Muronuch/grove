package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
)

func newDBCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage an env's database state",
		Long: `grove treats database state like a build cache: it prepares one golden copy
per set of inputs and clones it for every env, then applies only that branch's
own migrations on top.`,
	}
	cmd.AddCommand(
		dbResetCmd(app),
		dbPrepareCmd(app),
		dbSaveCmd(app),
		dbRestoreCmd(app),
		dbCheckpointsCmd(app),
	)
	return cmd
}

func dbResetCmd(app *App) *cobra.Command {
	var (
		scratch bool
		noPrep  bool
	)
	cmd := &cobra.Command{
		Use:   "reset [env]",
		Short: "Re-clone the snapshot and re-apply this branch's migrations",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			start := time.Now()
			if err := c.ResetState(ctx, t, envctl.ResetOptions{
				Scratch:     scratch,
				SkipPrepare: noPrep,
			}); err != nil {
				return classify(err)
			}
			if app.JSONOut {
				st, err := c.Status(ctx, t, envctl.StatusOptions{})
				if err != nil {
					return err
				}
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}
			app.Printf("reset %s in %s\n", t.Entry.Slug, time.Since(start).Round(time.Millisecond))
			for svc, ref := range t.Entry.Snapshots {
				app.Printf("%-10s %s (%s)\n", svc, meta.SnapshotKeyShort(ref.Key), ref.Source)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&scratch, "scratch", false, "start from an empty database instead of a snapshot")
	f.BoolVar(&noPrep, "no-prepare", false, "do not run the prepare command afterwards")
	return cmd
}

func dbPrepareCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "prepare [env]",
		Short: "Run the project's migrate and seed command",
		Long: `prepare applies whatever migrations are not applied yet. Agents call it after
writing a migration; it is the cheap operation, and reset is the expensive one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			results, err := c.RunPrepare(ctx, t)
			if err != nil {
				return classify(err)
			}
			failed := false
			for _, r := range results {
				if r.ExitCode != 0 {
					failed = true
				}
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": !failed, "env": t.Entry.Slug, "results": results,
				})
			}
			for _, r := range results {
				status := app.green("ok")
				if r.ExitCode != 0 {
					status = app.red(fmt.Sprintf("exit %d", r.ExitCode))
				}
				app.Printf("%-10s %s in %s\n", r.Service, status, r.Duration.Round(time.Millisecond))
				if r.ExitCode != 0 {
					fmt.Fprintln(app.Err(), r.Output)
				}
			}
			if failed {
				return &ExitError{Code: ExitGeneric, Err: fmt.Errorf("prepare failed")}
			}
			if len(results) == 0 {
				app.Printf("nothing to prepare: this project declares no [[stateful]] service with a command\n")
			}
			return nil
		},
	}
}

func dbSaveCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "save <name> [env]",
		Short: "Save a named checkpoint of the env's databases",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env := ""
			if len(args) > 1 {
				env = args[1]
			}
			c, t, err := app.resolve(ctx, env)
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			if err := c.SaveCheckpoint(ctx, t, args[0]); err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": true, "env": t.Entry.Slug, "checkpoint": args[0],
				})
			}
			app.Printf("saved %s\n", args[0])
			app.hint("restore it with `%s db restore %s`", meta.Name, args[0])
			return nil
		},
	}
}

func dbRestoreCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <name> [env]",
		Short: "Restore a named checkpoint",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env := ""
			if len(args) > 1 {
				env = args[1]
			}
			c, t, err := app.resolve(ctx, env)
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			if err := c.ResetState(ctx, t, envctl.ResetOptions{From: args[0]}); err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": true, "env": t.Entry.Slug, "restored": args[0],
				})
			}
			app.Printf("restored %s\n", args[0])
			return nil
		},
	}
}

func dbCheckpointsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "checkpoints [env]",
		Aliases: []string{"list"},
		Short:   "List an env's named checkpoints",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			entries, err := c.Checkpoints(ctx, t)
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "env": t.Entry.Slug, "checkpoints": entries,
				})
			}
			if len(entries) == 0 {
				app.Printf("no checkpoints for %s\n", t.Entry.Slug)
				app.hint("save one with `%s db save <name>`", meta.Name)
				return nil
			}
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []string{
					e.Name, e.Service, humanBytes(e.SizeBytes),
					humanDuration(time.Since(e.CreatedAt)) + " ago",
				})
			}
			app.table([]string{"NAME", "SERVICE", "SIZE", "AGE"}, rows)
			return nil
		},
	}
}

func newGCCmd(app *App) *cobra.Command {
	var (
		dryRun      bool
		allProjects bool
		after       time.Duration
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove unused snapshots and orphaned resources",
		Long: `gc deletes snapshots nothing has cloned for a while, keeping the newest
goldens, and any ` + meta.Name + `-labelled container, network or volume that no env
claims. Named checkpoints are never collected.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}

			var cfg *config.Config
			if p, perr := c.FindProject(ctx, cwd); perr == nil {
				cfg = p.Config
			} else {
				allProjects = true
			}

			report, err := c.GC(ctx, cfg, envctl.GCOptions{
				DryRun: dryRun, AllProjects: allProjects, After: after,
			})
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(report)
			}
			if len(report.Removed) == 0 {
				app.Printf("nothing to collect\n")
				return nil
			}
			rows := make([][]string, 0, len(report.Removed))
			for _, it := range report.Removed {
				rows = append(rows, []string{it.Kind, it.Name, it.Reason})
			}
			app.table([]string{"KIND", "NAME", "REASON"}, rows)
			verb := "removed"
			if dryRun {
				verb = "would remove"
			}
			app.Printf("\n%s %d resources", verb, len(report.Removed))
			if report.Freed > 0 {
				app.Printf(", freeing %s", humanBytes(report.Freed))
			}
			app.Printf("\n")
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "report what would be removed")
	f.BoolVar(&allProjects, "all-projects", false, "collect across every project")
	f.DurationVar(&after, "after", 0, "remove snapshots unused for longer than this (default: state.gc_after)")
	return cmd
}
