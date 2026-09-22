package cli

import (
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/sched"
)

func newDaemonCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "The background scheduler that sleeps idle envs and wakes them again",
		Long: `The daemon does two things: it pauses environments nobody has used for a
while, and it brings one back the moment someone opens its URL.

It starts itself when it is needed, so running these commands by hand is
usually only for looking at what it is thinking.`,
	}
	cmd.AddCommand(
		daemonStartCmd(app),
		daemonStopCmd(app),
		daemonStatusCmd(app),
		daemonRunCmd(app),
	)

	cmd.RunE = daemonStatusCmd(app).RunE
	cmd.Args = cobra.NoArgs
	return cmd
}

func daemonStartCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the scheduler in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := app.Store()
			if err != nil {
				return err
			}
			if sched.Running(store) {
				app.Printf("already running (pid %d)\n", sched.PID(store))
				return nil
			}
			if err := sched.Start(store, nil); err != nil {
				return classify(err)
			}
			app.Printf("started (pid %d)\n", sched.PID(store))
			app.hint("logs: %s", store.Path(sched.LogFile))
			return nil
		},
	}
}

func daemonStopCmd(app *App) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the scheduler",
		Long:  "Environments keep running; nothing is put to sleep or woken until it is back.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := app.Store()
			if err != nil {
				return err
			}
			if !sched.Running(store) {
				app.Printf("not running\n")
				return nil
			}
			if err := sched.Stop(store, timeout); err != nil {
				return classify(err)
			}
			app.Printf("stopped\n")
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "how long to wait for it to exit")
	return cmd
}

func daemonStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what the scheduler would do right now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := app.Store()
			if err != nil {
				return err
			}
			running := sched.Running(store)

			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			d := &sched.Daemon{Ctl: c, Log: quietLogger()}
			plan, planErr := d.Plan(ctx)

			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema":  envctl.Schema,
					"running": running,
					"pid":     sched.PID(store),
					"log":     store.Path(sched.LogFile),
					"plan":    plan,
				})
			}

			state := app.red("not running")
			if running {
				state = app.green("running") + app.dim(" (pid "+itoa(sched.PID(store))+")")
			}
			app.Printf("%-10s %s\n", "scheduler", state)
			app.Printf("%-10s %s\n", "log", store.Path(sched.LogFile))
			if plan.MemoryLimit > 0 {
				app.Printf("%-10s %s of %s\n", "memory", humanBytes(plan.MemoryUsed), humanBytes(plan.MemoryLimit))
			}
			if planErr != nil {
				app.Warn("could not evaluate the policy: %v", planErr)
				return nil
			}

			if len(plan.Envs) > 0 {
				app.Printf("\n")
				rows := make([][]string, 0, len(plan.Envs))
				for _, e := range plan.Envs {
					note := e.Keep
					if note == "" {
						note = "eligible"
					}
					rows = append(rows, []string{
						e.Env, app.stateLabel(e.State),
						humanDuration(e.Idle), humanBytes(e.Memory),
						note,
					})
				}
				app.table([]string{"ENV", "STATE", "IDLE", "MEM", "NOTE"}, rows)
			}
			if len(plan.Actions) > 0 {
				app.Printf("\n")
				rows := make([][]string, 0, len(plan.Actions))
				for _, a := range plan.Actions {
					rows = append(rows, []string{a.Env, string(a.Kind), a.Reason})
				}
				app.table([]string{"ENV", "NEXT", "WHY"}, rows)
			}
			if !running {
				app.hint("start it with `%s daemon start`", meta.Name)
			}
			return nil
		},
	}
}

func daemonRunCmd(app *App) *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the scheduler in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			c.Progress = nil

			level := slog.LevelInfo
			if app.Verbose {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			d := &sched.Daemon{Ctl: c, Log: log, Interval: interval}
			if err := d.Run(ctx); err != nil {
				return classify(err)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 0, "how often to evaluate the sleep policy")
	return cmd
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func itoa(n int) string {
	if n == 0 {
		return "?"
	}
	return strconv.Itoa(n)
}

func (a *App) ensureScheduler(enabled bool) {
	if !enabled {
		return
	}
	store, err := a.Store()
	if err != nil {
		return
	}
	if sched.Running(store) {
		return
	}
	if err := sched.Start(store, nil); err != nil {
		a.hint("the scheduler did not start (%v); envs will stay awake until `%s daemon start`",
			err, meta.Name)
	}
}
