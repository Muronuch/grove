package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"

	"github.com/Muronuch/grove/internal/registry"
)

func newNewCmd(app *App) *cobra.Command {
	var (
		from     string
		headless bool
		noAgent  bool
		agentCmd string
		keep     bool
		timeout  time.Duration
		skipPrep bool
	)
	cmd := &cobra.Command{
		Use:   "new <branch>",
		Short: "Create a worktree, a cloned database and a running stack for a branch",
		Long: `new gives a branch its own complete environment: a git worktree, isolated
containers, a database cloned from a cached snapshot, and a stable URL.

Unless --no-agent is passed, the configured agent is then started in the new
worktree, so that ` + meta.Name + ` new <branch> is the whole setup step.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			p, err := c.FindProject(ctx, cwd)
			if err != nil {
				return err
			}

			start := time.Now()
			t, err := c.Create(ctx, p, envctl.CreateOptions{
				Branch:        args[0],
				From:          from,
				Headless:      headless,
				KeepOnFailure: keep,
				SkipPrepare:   skipPrep,
				Timeout:       timeout,
			})
			if err != nil {
				return classify(err)
			}

			if app.JSONOut {
				st, err := c.Status(ctx, t, envctl.StatusOptions{})
				if err != nil {
					return err
				}
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}

			app.ensureScheduler(p.Config.Sleep.IsEnabled())

			app.Step("ready in %s", time.Since(start).Round(time.Second))
			app.printURLs(t)
			app.Printf("%-10s %s\n", "worktree", t.Entry.Worktree)

			if noAgent {
				return nil
			}
			return runAgent(ctx, app, t, agentCmd)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "ref a new branch starts at (default: the default branch)")
	f.BoolVar(&headless, "headless", false, "leave out services declared headless = false")
	f.BoolVar(&noAgent, "no-agent", false, "do not start the agent afterwards")
	f.StringVar(&agentCmd, "agent", "", "command to run in the new worktree (default: agent.command)")
	f.BoolVar(&keep, "keep-on-failure", false, "leave a partially created env in place for debugging")
	f.DurationVar(&timeout, "timeout", 0, "how long to wait for services (default: up.timeout)")
	f.BoolVar(&skipPrep, "no-prepare", false, "do not run the database prepare command")
	return cmd
}

func runAgent(ctx context.Context, app *App, t *envctl.Target, override string) error {
	argv := t.Config.Agent.Command
	if override != "" {
		argv = strings.Fields(override)
	}
	if len(argv) == 0 {
		return nil
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		app.Warn("agent %q is not on PATH; the env is ready at %s", argv[0], t.Entry.Worktree)
		return nil
	}
	app.Step("starting %s in %s", strings.Join(argv, " "), t.Entry.Worktree)
	c := exec.CommandContext(ctx, bin, argv[1:]...)
	c.Dir = t.Entry.Worktree
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), agentEnv(t)...)
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &ExitError{Code: ee.ExitCode(), Err: fmt.Errorf("%s exited with code %d", argv[0], ee.ExitCode())}
		}
		return err
	}
	return nil
}

func agentEnv(t *envctl.Target) []string {
	env := []string{
		meta.EnvVarName("ENV") + "=" + t.Entry.Slug,
		meta.EnvVarName("PROJECT") + "=" + t.Entry.Project,
		meta.EnvVarName("SLOT") + "=" + fmt.Sprint(t.Entry.Slot),
		meta.EnvVarName("WORKTREE") + "=" + t.Entry.Worktree,
	}
	for name, url := range t.Hosts().URLs() {
		env = append(env, meta.EnvVarName("URL", name)+"="+url)
	}
	return env
}

func newUpCmd(app *App) *cobra.Command {
	var (
		full     bool
		headless bool
		build    bool
		noBuild  bool
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "up [env]",
		Short: "Start or resume an env",
		Long: `up starts an env that is stopped, thaws one that is paused, and adopts the
current worktree if ` + meta.Name + ` does not manage it yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := first(args)

			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			t, err := c.Resolve(ctx, cwd, name)
			if err != nil {
				if name != "" {
					return classify(err)
				}
				t, err = adoptHere(ctx, app, c, cwd, headless)
				if err != nil {
					return classify(err)
				}
			}

			o := envctl.UpOptions{Full: full, Headless: headless, Timeout: timeout}
			if build {
				o.Build = boolPtr(true)
			}
			if noBuild {
				o.Build = boolPtr(false)
			}
			if err := c.Up(ctx, t, o); err != nil {
				return classify(err)
			}
			app.ensureScheduler(t.Config.Sleep.IsEnabled())
			if app.JSONOut {
				st, err := c.Status(ctx, t, envctl.StatusOptions{})
				if err != nil {
					return err
				}
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}
			app.printURLs(t)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&full, "full", false, "start every service, including ones excluded from headless envs")
	f.BoolVar(&headless, "headless", false, "leave out services declared headless = false")
	f.BoolVar(&build, "build", false, "rebuild images before starting")
	f.BoolVar(&noBuild, "no-build", false, "never rebuild images")
	f.DurationVar(&timeout, "timeout", 0, "how long to wait for services (default: up.timeout)")
	return cmd
}

func adoptHere(ctx context.Context, app *App, c *envctl.Controller, cwd string, headless bool) (*envctl.Target, error) {
	p, err := c.FindProject(ctx, cwd)
	if err != nil {
		return nil, err
	}
	git := p.GitIn(cwd)
	top, err := git.TopLevel(ctx)
	if err != nil {
		return nil, err
	}
	branch, err := git.CurrentBranch(ctx)
	if err != nil || branch == "" {
		return nil, fmt.Errorf("%s is on a detached HEAD; check out a branch first", top)
	}
	if top == p.Root {
		return nil, fmt.Errorf("%w: this is the main worktree of %s, not an env.\n"+
			"Create one with `%s new <branch>`, or run `%s up <env>` to start an existing one",
			envctl.ErrNotInEnv, p.Name(), meta.Name, meta.Name)
	}
	app.Step("adopting worktree %s (branch %s)", top, branch)
	return c.Adopt(ctx, p, top, branch, headless)
}

func newDownCmd(app *App) *cobra.Command {
	var (
		keepWorktree bool
		keepVolumes  bool
		deleteBranch bool
		force        bool
		yes          bool
	)
	cmd := &cobra.Command{
		Use:   "down [env]",
		Short: "Remove an env: stack, volumes and worktree",
		Long: `down removes everything ` + meta.Name + ` created for an env. The branch itself is
kept unless --delete-branch is passed: the code is the point, the environment is
disposable.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()

			if !yes && !app.JSONOut && !keepVolumes {
				what := fmt.Sprintf("Remove env %s (stack, volumes", t.Entry.Slug)
				if !keepWorktree {
					what += ", worktree " + t.Entry.Worktree
				}
				if deleteBranch {
					what += ", branch " + t.Entry.Branch
				}
				ok, err := confirm(app, what+")?")
				if err != nil {
					return err
				}
				if !ok {
					return exitf(ExitGeneric, "aborted")
				}
			}

			if err := c.Down(ctx, t, envctl.DownOptions{
				KeepWorktree: keepWorktree,
				KeepVolumes:  keepVolumes,
				DeleteBranch: deleteBranch,
				Force:        force,
			}); err != nil {
				return classify(err)
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{"schema": envctl.Schema, "ok": true, "removed": t.Entry.Slug})
			}
			app.Printf("removed %s\n", t.Entry.Slug)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&keepWorktree, "keep-worktree", false, "leave the worktree on disk")
	f.BoolVar(&keepVolumes, "keep-volumes", false, "leave the env's volumes in place")
	f.BoolVar(&deleteBranch, "delete-branch", false, "also delete the git branch")
	f.BoolVar(&force, "force", false, "discard uncommitted changes in the worktree")
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func newRestartCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart [env] [service...]",
		Short: "Restart an env or some of its services",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, rest, err := resolveWithServices(ctx, app, args)
			if err != nil {
				return classify(err)
			}
			defer c.Close()
			if err := c.Restart(ctx, t, rest); err != nil {
				return classify(err)
			}
			if app.JSONOut {
				st, err := c.Status(ctx, t, envctl.StatusOptions{})
				if err != nil {
					return err
				}
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}
			app.Printf("restarted %s\n", t.Entry.Slug)
			return nil
		},
	}
	return cmd
}

func newHoldCmd(app *App) *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "hold [env]",
		Short: "Keep an env awake for a while",
		Long: `hold marks an env active so the scheduler does not pause or stop it. MCP
calls renew the hold automatically, so an agent working in an env never has to
think about sleeping.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()
			if err := c.TouchActivity(ctx, t, ttl); err != nil {
				return err
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": true,
					"env": t.Entry.Slug, "hold_until": t.Entry.HoldUntil,
				})
			}
			app.Printf("holding %s for %s\n", t.Entry.Slug, ttl)
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 30*time.Minute, "how long to hold the env awake")
	return cmd
}

func newPinCmd(app *App, pin bool) *cobra.Command {
	use, short := "unpin [env]", "Let the scheduler sleep an env again"
	if pin {
		use, short = "pin [env]", "Exempt an env from sleeping entirely"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, t, err := app.resolve(ctx, first(args))
			if err != nil {
				return classify(err)
			}
			defer c.Close()
			err = c.Store.Update(ctx, func(r *registry.Registry) error {
				e, err := r.Env(t.Entry.Project, t.Entry.Slug)
				if err != nil {
					return err
				}
				e.Pinned = pin
				r.Touch()
				return nil
			})
			if err != nil {
				return err
			}
			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema, "ok": true, "env": t.Entry.Slug, "pinned": pin,
				})
			}
			verb := "unpinned"
			if pin {
				verb = "pinned"
			}
			app.Printf("%s %s\n", verb, t.Entry.Slug)
			return nil
		},
	}
}

func newAdoptCmd(app *App) *cobra.Command {
	var headless bool
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Register the current worktree as an env without starting it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
			t, err := adoptHere(ctx, app, c, cwd, headless)
			if err != nil {
				return classify(err)
			}
			if app.JSONOut {
				st, err := c.Status(ctx, t, envctl.StatusOptions{})
				if err != nil {
					return err
				}
				return app.WriteJSON(envctl.Report{Schema: envctl.Schema, Envs: []envctl.EnvStatus{st}})
			}
			app.Printf("%s\n", t.Entry.Slug)
			app.hint("start it with `%s up %s`", meta.Name, t.Entry.Slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&headless, "headless", false, "leave out services declared headless = false")
	return cmd
}

func resolveWithServices(ctx context.Context, app *App, args []string) (*envctl.Controller, *envctl.Target, []string, error) {
	c, err := app.controller(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	cwd, err := app.Cwd()
	if err != nil {
		c.Close()
		return nil, nil, nil, err
	}

	if len(args) > 0 {
		t, err := c.Resolve(ctx, cwd, args[0])
		switch {
		case err == nil:
			return c, t, args[1:], nil
		case !errors.Is(err, registry.ErrEnvNotFound):
			c.Close()
			return nil, nil, nil, err
		}
	}
	t, err := c.Resolve(ctx, cwd, "")
	if err != nil {
		c.Close()
		return nil, nil, nil, err
	}
	return c, t, args, nil
}

func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func boolPtr(b bool) *bool { return &b }
