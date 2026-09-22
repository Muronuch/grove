package doctor

import (
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/registry"
)

func fixture(t *testing.T, name, overlay string) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", name)
	dir := t.TempDir()
	copyTree(t, src, dir, "fixed")
	if overlay != "" {
		copyTree(t, filepath.Join(src, overlay), dir, "")
	}

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "fixture")
	return dir
}

func copyTree(t *testing.T, src, dst, skip string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != "" && rel == skip {
			return fs.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runStatic(t *testing.T, dir string) Report {
	t.Helper()
	ctx := context.Background()
	p, err := project.Find(ctx, dir)
	if err != nil {
		t.Fatalf("find project: %v", err)
	}
	store, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, &envctl.Controller{Store: store}, p, io.Discard, Options{SkipDynamic: true})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return report
}

func TestHostileFixtureReportsExactlyTheExpectedCodes(t *testing.T) {
	report := runStatic(t, fixture(t, "hostile", ""))

	want := []string{
		"E_PREPARE_MISSING",
		"E_SERVICE_MISSING",
		"E_STATEFUL_INPUTS",
		"E_VOLUME_MISSING",
		"W_ABSOLUTE_BIND",
		"W_CONTAINER_NAME",
		"W_HARDCODED_URL",
		"W_VOLUME_ISOLATED",
	}
	if got := report.Findings.Codes(); !reflect.DeepEqual(got, want) {
		t.Errorf("codes = %v\nwant  = %v", got, want)
	}
	if report.OK {
		t.Error("a project with four errors must not be reported as isolated")
	}
	for _, f := range report.Findings {
		if f.Hint == "" {
			t.Errorf("%s has no fix hint; an agent cannot act on it", f.Code)
		}
	}
}

func TestHostileFixtureIsCleanAfterTheHintedFixes(t *testing.T) {
	report := runStatic(t, fixture(t, "hostile", "fixed"))
	if len(report.Findings) != 0 {
		for _, f := range report.Findings {
			t.Errorf("%s: %s", f.Code, f.Message)
		}
	}
	if !report.OK {
		t.Error("the fixed project should be reported as isolated")
	}
}

func TestGoodFixturePassesStaticChecks(t *testing.T) {
	report := runStatic(t, fixture(t, "go-react-pg", ""))
	for _, f := range report.Findings {
		t.Errorf("unexpected %s: %s", f.Code, f.Message)
	}
}

func TestSequencePrefix(t *testing.T) {
	cases := map[string]string{
		"0001_init.sql":              "0001",
		"20260921120000_add.sql":     "20260921120000",
		"add_users.sql":              "",
		"12_too_short.sql":           "",
		"003_three_digits_is_ok.sql": "003",
	}
	for in, want := range cases {
		if got := sequencePrefix(in); got != want {
			t.Errorf("sequencePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLookup(t *testing.T) {
	out := `== api
Server:		127.0.0.11
Address:	127.0.0.11:53

Name:	api
Address: 172.18.0.3
== db
Server:		127.0.0.11
Address:	127.0.0.11:53

Name:	db
Address: 172.18.0.2
`
	got := parseLookup(out)
	if len(got["api"]) != 1 || got["api"][0] != "172.18.0.3" {
		t.Errorf("api = %v", got["api"])
	}
	if len(got["db"]) != 1 || got["db"][0] != "172.18.0.2" {
		t.Errorf("db = %v", got["db"])
	}

	for name, addrs := range got {
		for _, a := range addrs {
			if a == "127.0.0.11" {
				t.Errorf("%s: the DNS server address leaked into the answers", name)
			}
		}
	}
}
