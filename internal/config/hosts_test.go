package config

import (
	"strings"
	"testing"
)

const twoServices = `
[project]
name = "acme"

[[service]]
name = "web"
port = 5173
default = true

[[service]]
name = "api"
port = 8080
paths = ["/api"]

[[service]]
name = "postgres"
port = 5432
tcp = true
`

func TestHostScheme(t *testing.T) {
	c := parse(t, twoServices)
	h := c.Hosts(EnvIdentity{Project: "acme", Slug: "feat-invoices", Slot: 1})

	if got, want := h.Host("web"), "feat-invoices.acme.localhost"; got != want {
		t.Errorf("default service host = %q, want %q", got, want)
	}
	if got, want := h.Host("api"), "api.feat-invoices.acme.localhost"; got != want {
		t.Errorf("api host = %q, want %q", got, want)
	}
	if got, want := h.SlotHost("web"), "s1.acme.localhost"; got != want {
		t.Errorf("slot host = %q, want %q", got, want)
	}
	if got, want := h.SlotHost("api"), "api.s1.acme.localhost"; got != want {
		t.Errorf("api slot host = %q, want %q", got, want)
	}
	if got, want := h.URL("web"), "http://feat-invoices.acme.localhost"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}

	for _, host := range h.AllHosts() {
		if !strings.HasSuffix(host, ".acme.localhost") {
			t.Errorf("%q is not under the project domain", host)
		}
	}
	if len(h.AllHosts()) != 4 {
		t.Errorf("hosts = %v, want web+api with slot aliases", h.AllHosts())
	}
}

func TestURLCarriesFallbackPort(t *testing.T) {
	c := parse(t, twoServices)
	c.Router.Port = 7080
	h := c.Hosts(EnvIdentity{Project: "acme", Slug: "main", Slot: 2})
	if got, want := h.URL("web"), "http://main.acme.localhost:7080"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestSplitHost(t *testing.T) {
	cases := []struct {
		host                      string
		service, envKey, project_ string
		ok                        bool
	}{
		{"feat-x.acme.localhost", "", "feat-x", "acme", true},
		{"api.feat-x.acme.localhost", "api", "feat-x", "acme", true},
		{"s3.acme.localhost", "", "s3", "acme", true},
		{"api.s3.acme.localhost:7080", "api", "s3", "acme", true},
		{"acme.localhost", "", "", "", false},
		{"example.com", "", "", "", false},
		{"a.b.c.acme.localhost", "", "", "", false},
	}
	for _, tc := range cases {
		svc, env, proj, ok := SplitHost(tc.host, "localhost")
		if ok != tc.ok || svc != tc.service || env != tc.envKey || proj != tc.project_ {
			t.Errorf("SplitHost(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.host, svc, env, proj, ok, tc.service, tc.envKey, tc.project_, tc.ok)
		}
	}
}

func TestTemplates(t *testing.T) {
	c := parse(t, twoServices+`
[env.web]
VITE_API_URL = "{{ url \"api\" }}"
SELF = "{{ url \"web\" }}/app"
[env.api]
CORS_ORIGINS = "{{ url \"web\" }},{{ slotUrl \"web\" }}"
WHO = "{{ .Env }}-{{ .Slot }}-{{ .Project }}"
`)
	id := EnvIdentity{Project: "acme", Slug: "fix-crm", Slot: 3}
	web, err := c.RenderEnvFor("web", id)
	if err != nil {
		t.Fatalf("render web: %v", err)
	}
	if got, want := web["VITE_API_URL"], "http://api.fix-crm.acme.localhost"; got != want {
		t.Errorf("VITE_API_URL = %q, want %q", got, want)
	}
	if got, want := web["SELF"], "http://fix-crm.acme.localhost/app"; got != want {
		t.Errorf("SELF = %q, want %q", got, want)
	}
	api, err := c.RenderEnvFor("api", id)
	if err != nil {
		t.Fatalf("render api: %v", err)
	}
	if got, want := api["CORS_ORIGINS"], "http://fix-crm.acme.localhost,http://s3.acme.localhost"; got != want {
		t.Errorf("CORS_ORIGINS = %q, want %q", got, want)
	}
	if got, want := api["WHO"], "fix-crm-3-acme"; got != want {
		t.Errorf("WHO = %q, want %q", got, want)
	}
}

func TestTemplateRejectsUnknownServiceAtParseTime(t *testing.T) {
	err := parseErr(t, twoServices+`
[env.web]
X = "{{ url \"nope\" }}"
`)
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the unknown service: %v", err)
	}
	if !strings.Contains(err.Error(), "env.web.X") {
		t.Errorf("error should name the key: %v", err)
	}
}

func TestTemplateRejectsTCPServiceURL(t *testing.T) {
	err := parseErr(t, twoServices+`
[env.api]
X = "{{ url \"postgres\" }}"
`)
	if !strings.Contains(err.Error(), "tcp") {
		t.Errorf("error should explain that a tcp service has no URL: %v", err)
	}
}

func TestRenderWorktreeDir(t *testing.T) {
	c := parse(t, twoServices)
	got, err := c.RenderWorktreeDir("acme")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultWorktreeDir; got != want {
		t.Errorf("worktree.dir = %q, want the default %q", got, want)
	}

	c.Worktree.Dir = "../{{ .Repo }}.{{ .Project }}"
	got, err = c.RenderWorktreeDir("acme")
	if err != nil {
		t.Fatal(err)
	}
	if want := "../acme." + c.Project.Name; got != want {
		t.Errorf("worktree.dir = %q, want %q", got, want)
	}
}
