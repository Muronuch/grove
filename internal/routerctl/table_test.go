package routerctl

import (
	"testing"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
)

const cfgBody = `
[project]
name = "acme"

[[service]]
name = "web"
port = 5173
default = true
rewrite_host = "localhost:5173"

[[service]]
name = "api"
port = 8080
paths = ["/api", "/ws"]
websocket_path = "/ws"

[[service]]
name = "postgres"
port = 5432
tcp = true
`

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(cfgBody), "grove.toml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnvRouteShape(t *testing.T) {
	cfg := loadConfig(t)
	e := &registry.Env{
		Slug: "feat-invoices", Project: "acme", Slot: 1, State: registry.StateRunning,
	}
	got := EnvRoute(cfg, e)

	if got.State != router.StateRunning {
		t.Errorf("state = %v", got.State)
	}

	if len(got.Hosts) != 4 {
		t.Errorf("hosts = %v", got.Hosts)
	}

	byKey := map[string]router.Route{}
	for _, r := range got.Routes {
		byKey[r.HostPrefix+r.Path] = r
	}
	want := map[string]string{
		"/":     "grove-acme-feat-invoices-web-1:5173",
		"/api":  "grove-acme-feat-invoices-api-1:8080",
		"/ws":   "grove-acme-feat-invoices-api-1:8080",
		"api./": "grove-acme-feat-invoices-api-1:8080",
	}
	if len(byKey) != len(want) {
		t.Fatalf("routes = %+v", got.Routes)
	}
	for key, upstream := range want {
		r, ok := byKey[key]
		if !ok {
			t.Errorf("missing route %q", key)
			continue
		}

		if r.Upstream != upstream {
			t.Errorf("%s → %s, want %s", key, r.Upstream, upstream)
		}
	}
	if byKey["/"].RewriteHost != "localhost:5173" {
		t.Errorf("rewrite_host was not carried through: %+v", byKey["/"])
	}
	if byKey["/ws"].WebsocketPath != "/ws" {
		t.Errorf("websocket_path was not carried through: %+v", byKey["/ws"])
	}
	if _, ok := got.URLs["postgres"]; ok {
		t.Error("a tcp service must not appear in the URL map")
	}
}

func TestEnvRouteStateMapping(t *testing.T) {
	cfg := loadConfig(t)
	cases := map[registry.State]router.EnvState{
		registry.StateRunning:  router.StateRunning,
		registry.StatePaused:   router.StatePaused,
		registry.StateStopped:  router.StateStopped,
		registry.StateCreating: router.StateCreating,
		registry.StateFailed:   router.StateFailed,
		registry.StateMissing:  router.StateFailed,
	}
	for in, want := range cases {
		e := &registry.Env{Slug: "x", Project: "acme", Slot: 1, State: in}
		if got := EnvRoute(cfg, e).State; got != want {
			t.Errorf("%v → %v, want %v", in, got, want)
		}
	}
}

func TestBuildTableIsDeterministic(t *testing.T) {
	cfg := loadConfig(t)
	sources := []Source{
		{Cfg: cfg, Env: &registry.Env{Slug: "zeta", Project: "acme", Slot: 2, State: registry.StateRunning}},
		{Cfg: cfg, Env: &registry.Env{Slug: "alpha", Project: "acme", Slot: 1, State: registry.StateRunning}},
		{Cfg: nil, Env: &registry.Env{Slug: "orphan", Project: "gone"}},
	}
	table := BuildTable(7, sources)
	if table.Version != 7 {
		t.Errorf("version = %d", table.Version)
	}
	if len(table.Envs) != 2 {
		t.Fatalf("envs = %+v (an env with no config must be skipped)", table.Envs)
	}
	if table.Envs[0].Env != "alpha" || table.Envs[1].Env != "zeta" {
		t.Errorf("envs are not sorted: %s, %s", table.Envs[0].Env, table.Envs[1].Env)
	}
}

func TestRoutesResolveThroughTheRouter(t *testing.T) {
	cfg := loadConfig(t)
	e := &registry.Env{Slug: "feat-x", Project: "acme", Slot: 3, State: registry.StateRunning}
	store := router.NewStore()
	if ok, _ := store.Replace(BuildTable(1, []Source{{Cfg: cfg, Env: e}})); !ok {
		t.Fatal("table rejected")
	}
	cases := []struct{ host, path, want string }{
		{"feat-x.acme.localhost", "/", "grove-acme-feat-x-web-1:5173"},
		{"feat-x.acme.localhost", "/api/items", "grove-acme-feat-x-api-1:8080"},
		{"api.feat-x.acme.localhost", "/", "grove-acme-feat-x-api-1:8080"},
		{"s3.acme.localhost", "/", "grove-acme-feat-x-web-1:5173"},
		{"api.s3.acme.localhost", "/health", "grove-acme-feat-x-api-1:8080"},
	}
	for _, tc := range cases {
		m, ok := store.Lookup(tc.host, tc.path)
		if !ok {
			t.Errorf("%s%s did not resolve", tc.host, tc.path)
			continue
		}
		if m.Route.Upstream != tc.want {
			t.Errorf("%s%s → %s, want %s", tc.host, tc.path, m.Route.Upstream, tc.want)
		}
	}
}
