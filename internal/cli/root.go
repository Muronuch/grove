package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/meta"
)

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	app := &App{out: stdout, err: stderr, Color: colorEnabled(stderr)}
	root := newRootCmd(app)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := root.ExecuteContext(withApp(ctx, app))
	if err != nil {
		app.reportError(err)
		return exitCode(err)
	}
	return ExitOK
}

func newRootCmd(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   meta.Name,
		Short: "Parallel per-branch development environments for coding agents",
		Long: meta.Name + ` gives every git branch its own isolated, running copy of the
project's compose stack, reachable at a stable URL, with databases cloned from a
cached snapshot instead of migrated from scratch.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if app.Dir != "" {
				abs, err := filepath.Abs(app.Dir)
				if err != nil {
					return usageErr("--chdir: %v", err)
				}
				app.Dir = abs
				if st, err := os.Stat(app.Dir); err != nil || !st.IsDir() {
					return usageErr("--chdir: %s is not a directory", app.Dir)
				}
			}
			if app.JSONOut {
				app.Color = false
			}
			app.setupLogging()
			return nil
		},
	}

	f := root.PersistentFlags()
	f.BoolVar(&app.JSONOut, "json", false, "machine-readable output on stdout")
	f.BoolVarP(&app.Verbose, "verbose", "v", false, "show every step")
	f.BoolVarP(&app.Quiet, "quiet", "q", false, "suppress progress output")
	f.StringVarP(&app.Dir, "chdir", "C", "", "run as if started in this directory")
	f.StringVar(&app.Home, "home", "", "state directory (default "+homeHint()+")")
	f.BoolVar(&app.Trust, "trust", false, "run host hooks from "+meta.Name+".toml without confirmation")

	root.AddCommand(
		newVersionCmd(app),
		newInitCmd(app),
		newNewCmd(app),
		newUpCmd(app),
		newDownCmd(app),
		newLsCmd(app),
		newMonitorCmd(app),
		newStatusCmd(app),
		newURLCmd(app),
		newOpenCmd(app),
		newLogsCmd(app),
		newExecCmd(app),
		newRestartCmd(app),
		newHoldCmd(app),
		newPinCmd(app, true),
		newPinCmd(app, false),
		newAdoptCmd(app),
		newRouterCmd(app),
		newRouterServeCmd(app),
		newDBCmd(app),
		newGCCmd(app),
		newUninstallCmd(app),
		newDoctorCmd(app),
		newMCPCmd(app),
		newHookCmd(app),
		newDaemonCmd(app),
	)
	return root
}

func homeHint() string {
	if runtime.GOOS == "windows" {
		return "%USERPROFILE%\\." + meta.Name
	}
	return "~/." + meta.Name
}

func newVersionCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema":  1,
					"name":    meta.Name,
					"version": meta.VersionString(),
					"go":      runtime.Version(),
					"os":      runtime.GOOS,
					"arch":    runtime.GOARCH,
				})
			}
			app.Printf("%s %s (%s %s/%s)\n", meta.Name, meta.VersionString(),
				runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
