package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
)

func newLsCmd(app *App) *cobra.Command {
	var (
		allProjects bool
		noStats     bool
	)
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List envs",
		Args:    cobra.NoArgs,
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
			report, err := c.List(ctx, cwd, envctl.StatusOptions{
				Stats:       !noStats,
				AllProjects: allProjects,
			})
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(report)
			}
			if len(report.Envs) == 0 {
				app.Printf("no envs\n")
				app.hint("create one with `%s new <branch>`", meta.Name)
				return nil
			}

			header := []string{"ENV", "SLOT", "STATE", "URL", "MEM", "IDLE"}
			if allProjects {
				header = append([]string{"PROJECT"}, header...)
			}
			rows := make([][]string, 0, len(report.Envs))
			for _, e := range report.Envs {
				url := "—"
				if u, ok := defaultURL(e); ok {
					url = u
				}
				mem := "—"
				if e.MemoryBytes > 0 {
					mem = humanBytes(e.MemoryBytes)
				}
				name := e.Env
				if e.Pinned {
					name += " *"
				}
				row := []string{
					name,
					strconv.Itoa(e.Slot),
					app.stateLabel(e.State),
					url,
					mem,
					humanDuration(time.Duration(e.IdleSeconds) * time.Second),
				}
				if allProjects {
					row = append([]string{e.Project}, row...)
				}
				rows = append(rows, row)
			}
			app.table(header, rows)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&allProjects, "all-projects", false, "list envs of every project")
	f.BoolVar(&noStats, "no-stats", false, "skip memory sampling, which makes the listing instant")
	return cmd
}

func defaultURL(e envctl.EnvStatus) (string, bool) {
	if len(e.URLs) == 0 {
		return "", false
	}
	shortest, found := "", false
	for _, u := range e.URLs {
		host := strings.TrimPrefix(u, "http://")
		if !found || strings.Count(host, ".") < strings.Count(strings.TrimPrefix(shortest, "http://"), ".") {
			shortest, found = u, true
		}
	}
	return shortest, found
}

func newStatusCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [env]",
		Short: "Show one env in detail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			st, err := c.Status(ctx, t, envctl.StatusOptions{Stats: true})
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}

			app.Printf("%s  %s  slot %d  branch %s\n",
				app.bold(st.Env), app.stateLabel(st.State), st.Slot, st.Branch)
			app.Printf("%-10s %s\n", "worktree", st.Worktree)
			if st.Error != "" {
				app.Printf("%-10s %s\n", "error", app.red(firstLine(st.Error)))
			}
			app.Printf("\n")
			app.printURLs(t)

			if len(st.Snapshots) > 0 {
				app.Printf("\n")
				for _, svc := range sortedSnapshotKeys(st) {
					s := st.Snapshots[svc]
					app.Printf("%-10s %s (%s)\n", svc, meta.SnapshotKeyShort(s.Key), s.Source)
				}
			}

			if len(st.Services) > 0 {
				app.Printf("\n")
				rows := make([][]string, 0, len(st.Services))
				for _, s := range st.Services {
					health := s.Health
					if health == "" {
						health = "—"
					}
					rows = append(rows, []string{
						s.Name, app.stateLabel(s.State), health, humanBytes(s.MemoryBytes),
					})
				}
				app.table([]string{"SERVICE", "STATE", "HEALTH", "MEM"}, rows)
			}
			return nil
		},
	}
	return cmd
}

func sortedSnapshotKeys(st envctl.EnvStatus) []string {
	out := make([]string, 0, len(st.Snapshots))
	for k := range st.Snapshots {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func newURLCmd(app *App) *cobra.Command {
	var slot bool
	cmd := &cobra.Command{
		Use:   "url [env] [service]",
		Short: "Print an env's URL",
		Long: `url prints one URL, so it can be piped straight into another tool:

    curl "$(` + meta.Name + ` url api)/healthz"`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, rest, err := resolveWithServices(ctx, app, args)
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			hosts := t.Hosts()
			service := ""
			if len(rest) > 0 {
				service = rest[0]
			}
			if service == "" {
				d, ok := t.Config.DefaultService()
				if !ok {
					return exitf(ExitGeneric, "project %s has no routable service", t.Entry.Project)
				}
				service = d.Name
			}
			if s, ok := t.Config.ServiceByName(service); !ok || !s.Routable() {
				if addr, isTCP := t.Entry.TCP[service]; isTCP {
					app.Printf("%s\n", addr)
					return nil
				}
				return exitf(ExitUsage, "service %q has no URL in project %s", service, t.Entry.Project)
			}
			url := hosts.URL(service)
			if slot {
				url = hosts.SlotURL(service)
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "env": t.Entry.Slug, "service": service, "url": url,
				})
			}
			app.Printf("%s\n", url)
			return nil
		},
	}
	cmd.Flags().BoolVar(&slot, "slot", false, "print the stable slot alias instead of the branch hostname")
	return cmd
}

func newOpenCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "open [env] [service]",
		Short: "Open an env's URL in the browser",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, rest, err := resolveWithServices(ctx, app, args)
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			service := ""
			if len(rest) > 0 {
				service = rest[0]
			}
			if service == "" {
				d, ok := t.Config.DefaultService()
				if !ok {
					return exitf(ExitGeneric, "project %s has no routable service", t.Entry.Project)
				}
				service = d.Name
			}
			url := t.Hosts().URL(service)
			app.Printf("%s\n", url)
			return openBrowser(url)
		},
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Start()
}

func newLogsCmd(app *App) *cobra.Command {
	var (
		follow     bool
		tail       int
		since      string
		timestamps bool
	)
	cmd := &cobra.Command{
		Use:   "logs [env] [service]",
		Short: "Show a service's logs",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, rest, err := resolveWithServices(ctx, app, args)
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			service := ""
			if len(rest) > 0 {
				service = rest[0]
			}
			if service == "" {
				d, ok := t.Config.DefaultService()
				if !ok {
					return usageErr("name a service: `%s logs %s <service>`", meta.Name, t.Entry.Slug)
				}
				service = d.Name
			}

			_ = c.TouchActivity(ctx, t, 0)

			err = c.Logs(ctx, t, service, engine.LogOptions{
				Follow:     follow,
				Tail:       strconv.Itoa(tail),
				Since:      since,
				Timestamps: timestamps,
			}, app.Out())
			if err != nil && ctx.Err() == nil {
				return classify(err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "keep streaming")
	f.IntVar(&tail, "tail", 200, "number of trailing lines to show")
	f.StringVar(&since, "since", "", "only show logs after this time (e.g. 10m, 2026-09-21T10:00:00)")
	f.BoolVarP(&timestamps, "timestamps", "t", false, "prefix each line with its time")
	return cmd
}

func newExecCmd(app *App) *cobra.Command {
	var noTTY bool
	cmd := &cobra.Command{
		Use:   "exec [env] <service> -- <command>...",
		Short: "Run a command inside a service's container",
		Long: `exec runs a command in a service of an env and exits with the command's own
exit code, so it composes with anything that checks exit status:

    ` + meta.Name + ` exec api -- go test ./...`,
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			head, tail := args, []string(nil)
			if i := cmd.ArgsLenAtDash(); i >= 0 {
				head, tail = args[:i], args[i:]
			}
			if len(tail) == 0 {
				return usageErr("no command given; use `%s exec [env] <service> -- <command>...`", meta.Name)
			}
			if len(head) == 0 {
				return usageErr("no service given; use `%s exec [env] <service> -- <command>...`", meta.Name)
			}

			c, t, rest, err := resolveWithServices(ctx, app, head)
			if err != nil {
				return classify(err)
			}
			defer c.Close()
			if len(rest) == 0 {
				return usageErr("no service given; use `%s exec %s <service> -- <command>...`", meta.Name, t.Entry.Slug)
			}
			service := rest[0]

			_ = c.TouchActivity(ctx, t, 10*time.Minute)

			tty := !noTTY && isTerminal(os.Stdin)
			code, err := c.Exec(ctx, t, service, tail, os.Stdin, app.Out(), app.Err(), tty)
			if err != nil {
				return classify(err)
			}
			if code != 0 {
				return &ExitError{Code: code, Err: fmt.Errorf("command exited with code %d", code)}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "do not allocate a TTY")
	return cmd
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
