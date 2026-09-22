package router

import (
	"testing"
	"time"
)

func demoTable(version int64) Table {
	return Table{
		Version: version,
		Envs: []EnvRoute{{
			Project: "acme", Env: "feat-invoices", Slot: 1, State: StateRunning,
			Hosts: []string{
				"feat-invoices.acme.localhost",
				"s1.acme.localhost",
				"api.feat-invoices.acme.localhost",
				"api.s1.acme.localhost",
			},
			Routes: []Route{
				{HostPrefix: "", Path: "/", Upstream: "web-1:5173", Service: "web"},
				{HostPrefix: "", Path: "/api", Upstream: "api-1:8080", Service: "api"},
				{HostPrefix: "api.", Path: "/", Upstream: "api-1:8080", Service: "api"},
			},
		}},
	}
}

func TestLookupResolvesEveryHostForm(t *testing.T) {
	s := NewStore()
	if ok, _ := s.Replace(demoTable(1)); !ok {
		t.Fatal("table was rejected")
	}
	cases := []struct {
		host, path, want string
	}{
		{"feat-invoices.acme.localhost", "/", "web-1:5173"},
		{"feat-invoices.acme.localhost", "/assets/app.js", "web-1:5173"},

		{"feat-invoices.acme.localhost", "/api", "api-1:8080"},
		{"feat-invoices.acme.localhost", "/api/items", "api-1:8080"},

		{"feat-invoices.acme.localhost", "/apizza", "web-1:5173"},
		{"api.feat-invoices.acme.localhost", "/anything", "api-1:8080"},

		{"s1.acme.localhost", "/", "web-1:5173"},
		{"s1.acme.localhost", "/api/x", "api-1:8080"},
		{"api.s1.acme.localhost", "/", "api-1:8080"},

		{"feat-invoices.acme.localhost:7080", "/", "web-1:5173"},
		{"FEAT-INVOICES.acme.localhost.", "/", "web-1:5173"},
	}
	for _, tc := range cases {
		m, ok := s.Lookup(tc.host, tc.path)
		if !ok {
			t.Errorf("%s%s did not resolve", tc.host, tc.path)
			continue
		}
		if m.Route.Upstream != tc.want {
			t.Errorf("%s%s → %s, want %s", tc.host, tc.path, m.Route.Upstream, tc.want)
		}
	}
}

func TestLookupRejectsUnknownHost(t *testing.T) {
	s := NewStore()
	s.Replace(demoTable(1))
	for _, host := range []string{"other.acme.localhost", "acme.localhost", "example.com"} {
		if _, ok := s.Lookup(host, "/"); ok {
			t.Errorf("%s should not resolve", host)
		}
	}
}

func TestReplaceRejectsOlderVersion(t *testing.T) {
	s := NewStore()
	if ok, v := s.Replace(demoTable(5)); !ok || v != 5 {
		t.Fatalf("first push: ok=%v version=%d", ok, v)
	}

	if ok, v := s.Replace(demoTable(4)); ok || v != 5 {
		t.Errorf("stale push was accepted: ok=%v version=%d", ok, v)
	}
	if got := s.Table().Version; got != 5 {
		t.Errorf("version = %d, want 5", got)
	}

	if ok, _ := s.Replace(demoTable(5)); !ok {
		t.Error("an equal version should be accepted")
	}
	if ok, v := s.Replace(demoTable(6)); !ok || v != 6 {
		t.Errorf("newer push: ok=%v version=%d", ok, v)
	}
}

func TestSetState(t *testing.T) {
	s := NewStore()
	s.Replace(demoTable(1))
	if !s.SetState("acme", "feat-invoices", StatePaused) {
		t.Fatal("SetState did not find the env")
	}
	env, ok := s.EnvForHost("feat-invoices.acme.localhost")
	if !ok || env.State != StatePaused {
		t.Errorf("state = %v", env.State)
	}
	if s.SetState("acme", "nope", StateRunning) {
		t.Error("SetState should not invent an env")
	}
}

func TestPathMatches(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/", "/", true},
		{"/anything", "", true},
		{"/api", "/api", true},
		{"/api/", "/api", true},
		{"/api/items", "/api", true},
		{"/apizza", "/api", false},
		{"/ap", "/api", false},
		{"/api", "/api/", true},
	}
	for _, tc := range cases {
		if got := pathMatches(tc.path, tc.prefix); got != tc.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tc.path, tc.prefix, got, tc.want)
		}
	}
}

func TestBaseHosts(t *testing.T) {
	got := baseHosts([]string{
		"api.feat-x.acme.localhost",
		"feat-x.acme.localhost",
		"s2.acme.localhost",
		"api.s2.acme.localhost",
	})
	want := map[string]bool{"feat-x.acme.localhost": true, "s2.acme.localhost": true}
	if len(got) != 2 {
		t.Fatalf("bases = %v, want two", got)
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("%q is not a base host", h)
		}
	}
}

func TestActivityTracking(t *testing.T) {
	a := NewActivity()
	a.Touch("acme", "feat-x")
	a.Touch("acme", "feat-x")
	snap := a.Snapshot()
	e, ok := snap["acme/feat-x"]
	if !ok {
		t.Fatalf("snapshot = %v", snap)
	}
	if e.Requests != 2 {
		t.Errorf("requests = %d, want 2", e.Requests)
	}
	if time.Since(e.LastRequestAt) > time.Minute {
		t.Errorf("last request time looks wrong: %v", e.LastRequestAt)
	}
}

func TestWakeReleasesWaiters(t *testing.T) {
	a := NewActivity()
	woken := a.RequestWake("acme", "feat-x", false)
	select {
	case <-woken:
		t.Fatal("the wake channel closed before the env woke")
	default:
	}
	a.Woke("acme", "feat-x")
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("the wake channel was not released")
	}
}

func TestPendingListCarriesTheBrowserFlag(t *testing.T) {
	a := NewActivity()
	a.RequestWake("acme", "api-only", false)
	a.RequestWake("acme", "page", true)

	got := map[string]bool{}
	for _, w := range a.PendingList() {
		got[w.Env] = w.Browser
	}
	if got["api-only"] {
		t.Error("a non-navigation request must not ask for the full stack")
	}
	if !got["page"] {
		t.Error("a page load should ask for the full stack")
	}

	a.Woke("acme", "page")
	for _, w := range a.PendingList() {
		if w.Env == "page" {
			t.Error("a woken env is still pending")
		}
	}
}
