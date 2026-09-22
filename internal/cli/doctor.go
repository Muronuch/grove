package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/doctor"
	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/meta"
)

func newDoctorCmd(app *App) *cobra.Command {
	var (
		keep    bool
		static  bool
		timeout time.Duration
		codes   bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Prove that this project can be isolated",
		Long: `doctor boots two throwaway environments and checks that they cannot see one
another: no shared host ports, no shared data volumes, no service name that
resolves across envs, every URL reachable through the router.

Every problem it reports carries a stable code and a fix recipe, so it can be
run in a loop until it exits 0. See docs/doctor-codes.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if codes {
				app.Printf("%s", finding.Markdown())
				return nil
			}

			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			p, err := c.FindProject(ctx, cwd)
			if err != nil {
				return classify(err)
			}

			progress := app.Err()
			if app.Quiet || app.JSONOut {
				progress = nil
			}
			report, err := doctor.Run(ctx, c, p, progress, doctor.Options{
				Keep:        keep,
				SkipDynamic: static,
				Timeout:     timeout,
			})
			if err != nil {
				return classify(err)
			}

			if app.JSONOut {
				if err := app.WriteJSON(report); err != nil {
					return err
				}
				if !report.OK {
					return &ExitError{Code: ExitDoctor, Err: fmt.Errorf("doctor found %d error(s)", len(report.Findings.Errors()))}
				}
				return nil
			}
			return app.printDoctor(report)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&keep, "keep", false, "leave the throwaway envs running")
	f.BoolVar(&static, "static", false, "only run the checks that need no Docker")
	f.DurationVar(&timeout, "timeout", 0, "how long a throwaway env may take to start (default: up.timeout)")
	f.BoolVar(&codes, "codes", false, "print the catalogue of finding codes and exit")
	return cmd
}

func (a *App) printDoctor(report doctor.Report) error {
	errs, warns := 0, 0
	for _, f := range report.Findings {
		switch f.Severity {
		case finding.Error:
			errs++
		case finding.Warning:
			warns++
		}
		a.printFinding(f)
	}

	if len(report.Timings) > 0 && a.Verbose {
		a.Printf("\n")
		rows := make([][]string, 0, len(report.Timings))
		for _, t := range report.Timings {
			rows = append(rows, []string{t.Name, t.Duration.Round(time.Millisecond).String()})
		}
		a.table([]string{"PHASE", "TOOK"}, rows)
	}

	a.Printf("\n")
	switch {
	case errs > 0:
		a.Printf("%s %d error(s), %d warning(s) in %s\n",
			a.red("not isolated:"), errs, warns, report.Duration.Round(time.Second))
		a.hint("each code is explained in docs/doctor-codes.md, or `%s doctor --codes`", meta.Name)
		return &ExitError{Code: ExitDoctor, Err: fmt.Errorf("doctor found %d error(s)", errs)}
	case warns > 0:
		a.Printf("%s %d warning(s) in %s\n",
			a.green("isolated:"), warns, report.Duration.Round(time.Second))
	default:
		a.Printf("%s no findings in %s\n", a.green("isolated:"), report.Duration.Round(time.Second))
	}
	return nil
}

func (a *App) printFinding(f finding.Finding) {
	tag := a.red(string(f.Code))
	switch f.Severity {
	case finding.Warning:
		tag = a.yellow(string(f.Code))
	case finding.Info:
		tag = a.dim(string(f.Code))
	}
	where := ""
	if f.Env != "" {
		where += " [" + f.Env + "]"
	}
	if f.Service != "" {
		where += " (" + f.Service + ")"
	}
	a.Printf("\n%s%s %s\n", tag, a.dim(where), f.Message)
	if file, ok := f.Evidence["file"]; ok {
		if line, ok := f.Evidence["line"]; ok {
			a.Printf("  %s\n", a.dim(fmt.Sprintf("%v:%v", file, line)))
		}
	}
	if f.Hint != "" {
		for _, line := range strings.Split(f.Hint, "\n") {
			a.Printf("  %s %s\n", a.dim("fix:"), line)
		}
	}
}
