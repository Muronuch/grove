package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
	"github.com/Muronuch/grove/internal/routerctl"
)

func newRouterManager(rt engine.Runtime, store *registry.Store, out io.Writer) *routerctl.Manager {
	return &routerctl.Manager{Runtime: rt, Store: store, Out: out}
}

func newRouterCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "router",
		Short: "Inspect and manage the shared router",
		Long: `The router is one container per machine. It receives browser traffic for
every env hostname and proxies it to the right container. It never touches the
Docker socket, and its admin API is bound to loopback behind a bearer token.`,
	}
	cmd.AddCommand(
		routerStatusCmd(app),
		routerRestartCmd(app),
		routerSyncCmd(app),
		routerBuildCmd(app),
		routerStopCmd(app),
	)
	return cmd
}

func routerStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the router's state and route table",
		Args:  cobra.NoArgs,
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

			state := "missing"
			if ct, err := c.Runtime.Container(ctx, meta.RouterContainer); err == nil {
				state = string(ct.State)
			}

			var table router.Table
			var health router.HealthResponse
			if client, err := c.Router.Client(); err == nil {
				health, _ = client.Health(ctx)
				table, _ = client.Routes(ctx)
			}

			if app.JSONOut {
				return app.WriteJSON(map[string]any{
					"schema": envctl.Schema,
					"router": map[string]any{
						"state":      state,
						"port":       reg.Router.Port,
						"admin_port": reg.Router.AdminPort,
						"image":      reg.Router.Image,
						"version":    health.Version,
						"uptime":     health.Uptime,
					},
					"routes": table,
				})
			}

			app.Printf("%-12s %s\n", "state", app.stateLabel(state))
			app.Printf("%-12s http://127.0.0.1:%d\n", "proxy", reg.Router.Port)
			app.Printf("%-12s http://127.0.0.1:%d\n", "admin", reg.Router.AdminPort)
			app.Printf("%-12s %s\n", "image", reg.Router.Image)
			if health.Version != "" {
				app.Printf("%-12s %s (up %s)\n", "version", health.Version, health.Uptime)
			}
			app.Printf("%-12s %d\n", "table", table.Version)

			if len(table.Envs) > 0 {
				app.Printf("\n")
				rows := make([][]string, 0, len(table.Envs))
				for _, e := range table.Envs {
					for _, r := range e.Routes {
						host := r.HostPrefix + shortestHost(e.Hosts)
						rows = append(rows, []string{e.Project + "/" + e.Env, host + r.Path, r.Upstream})
					}
				}
				app.table([]string{"ENV", "HOST+PATH", "UPSTREAM"}, rows)
			}
			return nil
		},
	}
}

func shortestHost(hosts []string) string {
	best := ""
	for _, h := range hosts {
		if best == "" || len(h) < len(best) {
			best = h
		}
	}
	return best
}

func routerRestartCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Recreate the router container and push the route table",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			if err := c.Router.Remove(ctx, false); err != nil {
				return err
			}
			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			p, perr := c.FindProject(ctx, cwd)
			if perr != nil {
				cfg, err := bareRouterConfig(app)
				if err != nil {
					return err
				}
				st, err := c.Router.Ensure(ctx, cfg)
				if err != nil {
					return classify(err)
				}
				return reportRouter(app, st)
			}
			if err := c.SyncRouter(ctx, p); err != nil {
				return classify(err)
			}
			app.Printf("router restarted\n")
			return nil
		},
	}
}

func routerSyncCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Rebuild the route table from the registry and push it",
		Args:  cobra.NoArgs,
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
			p, err := c.FindProject(ctx, cwd)
			if err != nil {
				return classify(err)
			}
			if err := c.SyncRouter(ctx, p); err != nil {
				return classify(err)
			}
			app.Printf("routes pushed\n")
			return nil
		},
	}
}

func routerBuildCmd(app *App) *cobra.Command {
	var (
		source string
		tag    string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the router image from a " + meta.Name + " source tree",
		Long: `build produces the router image locally. Released versions pull a published
image instead; this exists for development and for CI, where the source tree is
already on disk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			if source == "" {
				source, err = routerctl.FindSource()
				if err != nil {
					return exitf(ExitUsage, "%v; pass --source", err)
				}
			}
			if tag == "" {
				tag = routerctl.ImageRef("")
			}
			app.Step("building %s from %s", tag, source)
			if err := routerctl.BuildImage(ctx, c.Runtime, source, tag, app.Err()); err != nil {
				return classify(err)
			}
			app.Printf("%s\n", tag)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&source, "source", "", "source tree to build from (default: auto-detected)")
	f.StringVar(&tag, "tag", "", "image tag to produce (default: the version's published tag)")
	return cmd
}

func routerStopCmd(app *App) *cobra.Command {
	var withData bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Remove the router container",
		Long:  "Every env keeps running, but no hostname resolves until the router is back.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Router.Remove(ctx, withData); err != nil {
				return classify(err)
			}
			app.Printf("router removed\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&withData, "with-data", false, "also delete the persisted route table")
	return cmd
}

func reportRouter(app *App, st routerctl.Status) error {
	if app.JSONOut {
		return app.WriteJSON(map[string]any{
			"schema": envctl.Schema, "ok": true,
			"router": map[string]any{"port": st.Port, "admin_port": st.AdminPort, "version": st.Version},
		})
	}
	app.Printf("router on http://127.0.0.1:%d\n", st.Port)
	return nil
}

func bareRouterConfig(app *App) (*config.Config, error) {
	cfg, err := config.Parse([]byte("[project]\nname = \""+meta.Name+"\"\n"), config.FileName)
	if err != nil {
		return nil, err
	}
	store, err := app.Store()
	if err != nil {
		return nil, err
	}
	reg, err := store.Read()
	if err != nil {
		return nil, err
	}
	cfg.Router.Port = routerctl.EffectivePort(reg, cfg)
	if reg.Router.AdminPort != 0 {
		cfg.Router.AdminPort = reg.Router.AdminPort
	}
	return cfg, nil
}

func newRouterServeCmd(app *App) *cobra.Command {
	var (
		proxyAddr string
		adminAddr string
		statePath string
	)
	cmd := &cobra.Command{
		Use:    "router-serve",
		Short:  "Run the router (used inside the router container)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return router.Serve(cmd.Context(), router.Options{
				ProxyAddr: proxyAddr,
				AdminAddr: adminAddr,
				StatePath: statePath,
				Log:       log,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&proxyAddr, "proxy-addr", fmt.Sprintf(":%d", meta.RouterProxyPort), "address for browser traffic")
	f.StringVar(&adminAddr, "admin-addr", fmt.Sprintf(":%d", meta.RouterAdminPort), "address for the admin API")
	f.StringVar(&statePath, "state", router.DefaultStatePath, "where to persist the route table")
	return cmd
}
