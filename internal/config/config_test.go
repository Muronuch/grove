package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimal = `
[project]
name = "demo"

[[service]]
name = "web"
port = 5173
default = true
`

func parse(t *testing.T, body string) *Config {
	t.Helper()
	c, err := Parse([]byte(body), "grove.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func parseErr(t *testing.T, body string) error {
	t.Helper()
	_, err := Parse([]byte(body), "grove.toml")
	if err == nil {
		t.Fatalf("expected an error")
	}
	return err
}

func TestDefaults(t *testing.T) {
	c := parse(t, minimal)
	if c.Project.MaxSlots != DefaultMaxSlots {
		t.Errorf("max_slots = %d, want %d", c.Project.MaxSlots, DefaultMaxSlots)
	}
	if c.Router.BaseDomain != "localhost" {
		t.Errorf("base_domain = %q", c.Router.BaseDomain)
	}
	if c.Router.Port != 80 || c.Router.AdminPort != 9180 {
		t.Errorf("router ports = %d/%d", c.Router.Port, c.Router.AdminPort)
	}
	if c.State.Driver != DefaultDriver {
		t.Errorf("driver = %q", c.State.Driver)
	}
	if got := c.Up.Timeout.Duration(); got != DefaultUpTimeout {
		t.Errorf("up.timeout = %s", got)
	}
	if !c.State.CacheBranch() {
		t.Error("cache_branch_snapshots should default to true")
	}
	if len(c.Agent.Command) != 1 || c.Agent.Command[0] != DefaultAgentCommand {
		t.Errorf("agent.command = %v", c.Agent.Command)
	}
}

func TestValidationNamesKeyAndLine(t *testing.T) {
	err := parseErr(t, `
[project]
name = "Demo Project"

[[service]]
name = "web"
port = 5173
default = true
`)
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatalf("want Errors, got %T: %v", err, err)
	}
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %d: %v", len(errs), err)
	}
	if errs[0].Key != "project.name" {
		t.Errorf("key = %q", errs[0].Key)
	}
	if errs[0].Line != 3 {
		t.Errorf("line = %d, want 3", errs[0].Line)
	}
}

func TestValidationReportsUnknownKeyWithLine(t *testing.T) {
	err := parseErr(t, `
[project]
name = "demo"
maxslots = 3
`)
	if !strings.Contains(err.Error(), "maxslots") {
		t.Errorf("error should name the unknown key: %v", err)
	}
	if !strings.Contains(err.Error(), ":4") {
		t.Errorf("error should carry the line: %v", err)
	}
}

func TestValidationCatchesSemanticProblems(t *testing.T) {
	cases := map[string]struct{ body, wantKey string }{
		"two defaults": {`
[project]
name = "demo"
[[service]]
name = "a"
port = 1
default = true
[[service]]
name = "b"
port = 2
default = true
`, "service.default"},
		"tcp default": {`
[project]
name = "demo"
[[service]]
name = "db"
port = 5432
tcp = true
default = true
`, "service[0].default"},
		"duplicate service": {`
[project]
name = "demo"
[[service]]
name = "web"
port = 1
default = true
[[service]]
name = "web"
port = 2
`, "service[1].name"},
		"unknown driver": {`
[project]
name = "demo"
[[service]]
name = "web"
port = 1
default = true
[state]
driver = "magic"
`, "state.driver"},
		"bad path prefix": {`
[project]
name = "demo"
[[service]]
name = "web"
port = 1
default = true
paths = ["api"]
`, "service[0].paths[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, tc.body)
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("want key %q in error, got: %v", tc.wantKey, err)
			}
		})
	}
}

func TestStatefulPrepareIsNotRequiredToParse(t *testing.T) {
	c := parse(t, minimal+`
[[stateful]]
service = "postgres"
volume = "pgdata"
inputs = []
`)
	if len(c.Stateful) != 1 {
		t.Fatalf("stateful = %v", c.Stateful)
	}
	if c.Stateful[0].Prepare.Defined() {
		t.Error("prepare should be undefined")
	}
	if c.Stateful[0].StopGrace.Duration() != DefaultStopGrace {
		t.Errorf("stop_grace = %s", c.Stateful[0].StopGrace)
	}
}

func TestStatefulPrepareModeValidated(t *testing.T) {
	err := parseErr(t, minimal+`
[[stateful]]
service = "postgres"
volume = "pgdata"
inputs = ["m/**"]
  [stateful.prepare]
  mode = "sideways"
  command = ["true"]
`)
	if !strings.Contains(err.Error(), "stateful[0].prepare.mode") {
		t.Errorf("want the mode key named: %v", err)
	}
}

func TestPrepareModeRunNeedsService(t *testing.T) {
	err := parseErr(t, minimal+`
[[stateful]]
service = "postgres"
volume = "pgdata"
inputs = ["m/**"]
  [stateful.prepare]
  mode = "run"
  command = ["true"]
`)
	if !strings.Contains(err.Error(), "prepare.service") {
		t.Errorf("want prepare.service named: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":   30 * time.Second,
		"15m":   15 * time.Minute,
		"2h":    2 * time.Hour,
		"14d":   14 * 24 * time.Hour,
		"1d12h": 36 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s = %s, want %s", in, got, want)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("want an error for a nonsense duration")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]Size{
		"512m": {Bytes: 512 << 20},
		"8g":   {Bytes: 8 << 30},
		"1024": {Bytes: 1024},
		"50%":  {Percent: 50},
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got.Bytes != want.Bytes || got.Percent != want.Percent {
			t.Errorf("%s = %+v, want %+v", in, got, want)
		}
	}
	if got := (Size{Percent: 50}).Resolve(1000); got != 500 {
		t.Errorf("50%% of 1000 = %d", got)
	}
	if _, err := ParseSize("150%"); err == nil {
		t.Error("want an error above 100%")
	}
}
