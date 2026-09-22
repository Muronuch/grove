//go:build integration

package envctl_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/routerctl"
)

type harness struct {
	t    *testing.T
	ctl  *envctl.Controller
	proj *project.Project
	repo string
	home string
}

func newHarness(t *testing.T, fixture string) *harness {
	t.Helper()
	ctx := context.Background()

	home := t.TempDir()
	repo := t.TempDir()
	copyFixture(t, filepath.Join("..", "..", "testdata", "fixtures", fixture), repo)
	gitInit(t, repo)

	ctl, err := envctl.NewController(ctx, home, testWriter{t})
	if err != nil {
		t.Skipf("no usable Docker engine: %v", err)
	}
	t.Cleanup(func() { _ = ctl.Close() })

	src, err := routerctl.FindSource()
	if err != nil {
		t.Fatalf("cannot locate the grove source to build the router image: %v", err)
	}
	ref := routerctl.ImageRef("")
	if _, err := ctl.Runtime.ImageID(ctx, ref); err != nil {
		t.Logf("building the router image %s", ref)
		if err := routerctl.BuildImage(ctx, ctl.Runtime, src, ref, io.Discard); err != nil {
			t.Fatalf("build the router image: %v", err)
		}
	}

	p, err := ctl.FindProject(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctl: ctl, proj: p, repo: repo, home: home}
	t.Cleanup(h.cleanup)
	return h
}

func (h *harness) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	reg, err := h.ctl.Store.Read()
	if err != nil {
		return
	}
	for _, e := range reg.All() {
		t := &envctl.Target{Project: h.proj, Entry: e, Config: h.proj.Config}
		_ = h.ctl.Down(ctx, t, envctl.DownOptions{KeepWorktree: true, Force: true})
	}

	_, _ = h.ctl.GC(ctx, h.proj.Config, envctl.GCOptions{AllProjects: true, After: -time.Hour})
	for _, v := range h.volumes(ctx) {
		_ = h.ctl.Runtime.RemoveVolume(ctx, v, true)
	}
}

func (h *harness) volumes(ctx context.Context) []string {
	vs, err := h.ctl.Runtime.Volumes(ctx, engine.Selector{
		Labels: map[string]string{meta.LabelManaged: "true", meta.LabelProject: h.proj.Name()},
	})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func (h *harness) create(branch string) *envctl.Target {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	t, err := h.ctl.Create(ctx, h.proj, envctl.CreateOptions{Branch: branch})
	if err != nil {
		h.t.Fatalf("create %s: %v", branch, err)
	}
	return t
}

func (h *harness) get(t *envctl.Target, service, path string) (int, string) {
	h.t.Helper()
	code, body, err := h.try(t, service, path)
	if err != nil {
		h.t.Fatalf("GET %s%s: %v", t.Hosts().Host(service), path, err)
	}
	return code, body
}

func (h *harness) try(t *envctl.Target, service, path string) (int, string, error) {
	host := t.Hosts().Host(service)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", t.Config.Router.Port, path)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableKeepAlives: true},
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, string(body), nil
}

func TestTwoEnvsAreIsolated(t *testing.T) {
	h := newHarness(t, "go-react-pg")

	a := h.create("feat/a")
	b := h.create("feat/b")

	for _, env := range []*envctl.Target{a, b} {
		code, body := h.get(env, "api", "/api/whoami")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", env.Entry.Slug, code, body)
		}
		if !strings.Contains(body, `"env":"`+env.Entry.Slug+`"`) {
			t.Errorf("%s answered with %s", env.Entry.Slug, body)
		}

		if !strings.Contains(body, `"db_marker":"`+env.Entry.Slug+`"`) {
			t.Errorf("%s reached another env's database: %s", env.Entry.Slug, body)
		}
	}

	post(t, a, `{"title":"only in a"}`)
	_, aItems := h.get(a, "api", "/api/items")
	_, bItems := h.get(b, "api", "/api/items")
	if !strings.Contains(aItems, "only in a") {
		t.Errorf("the write did not land in a: %s", aItems)
	}
	if strings.Contains(bItems, "only in a") {
		t.Errorf("a's write leaked into b: %s", bItems)
	}
}

func TestNoHostPortsExceptDeclaredOnes(t *testing.T) {
	h := newHarness(t, "go-react-pg")
	env := h.create("feat/ports")

	ctx := context.Background()
	allowed := map[string]bool{}
	for _, s := range env.Config.TCPServices() {
		allowed[s.Name] = true
	}
	containers, err := h.ctl.Runtime.Containers(ctx, engine.Selector{
		All: true,
		Labels: map[string]string{
			meta.LabelManaged: "true",
			meta.LabelProject: env.Entry.Project,
			meta.LabelEnv:     env.Entry.Slug,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		svc := c.ComposeService()
		for port, bindings := range c.Ports {
			if len(bindings) == 0 {
				continue
			}
			if !allowed[svc] {
				t.Errorf("%s publishes %s, which a second env would collide with", svc, port)
				continue
			}
			for _, b := range bindings {
				if b.IP != "127.0.0.1" && b.IP != "::1" {
					t.Errorf("%s publishes %s on %s, not on loopback", svc, port, b.IP)
				}
			}
		}
	}
}

func TestWebSocketThroughTheRouter(t *testing.T) {
	h := newHarness(t, "go-react-pg")
	env := h.create("feat/ws")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", env.Config.Router.Port), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		env.Hosts().Host("api"))

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no response to the upgrade: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "101") {
		t.Errorf("response = %q", string(buf[:n]))
	}
}

func TestGoldenIsBuiltOnceUnderConcurrentCreates(t *testing.T) {
	h := newHarness(t, "go-react-pg")

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
			defer cancel()
			_, errs[i] = h.ctl.Create(ctx, h.proj, envctl.CreateOptions{
				Branch: fmt.Sprintf("feat/c%d", i),
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	idx, err := h.ctl.Index.Read()
	if err != nil {
		t.Fatal(err)
	}
	goldens := 0
	for _, e := range idx.Snapshots {
		if e.Kind == "golden" && e.Project == h.proj.Name() {
			goldens++
		}
	}
	if goldens != 1 {
		t.Errorf("%d goldens were built, want exactly 1", goldens)
	}
}

func TestDBResetReturnsToTheSnapshotPlusBranchMigrations(t *testing.T) {
	h := newHarness(t, "go-react-pg")
	env := h.create("feat/reset")
	ctx := context.Background()

	mig := filepath.Join(env.Entry.Worktree, "api", "migrations", "0003_branch_only.sql")
	body := "CREATE TABLE branch_only (note TEXT PRIMARY KEY);\n" +
		"INSERT INTO branch_only VALUES ('added on this branch');\n"
	if err := os.WriteFile(mig, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.ctl.PrepareState(ctx, env); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	post(t, env, `{"title":"garbage"}`)
	if _, items := h.get(env, "api", "/api/items"); !strings.Contains(items, "garbage") {
		t.Fatalf("the write did not land: %s", items)
	}

	resetCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := h.ctl.ResetState(resetCtx, env, envctl.ResetOptions{}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	_, items := h.get(env, "api", "/api/items")
	if strings.Contains(items, "garbage") {
		t.Errorf("reset did not discard the change: %s", items)
	}
	if !strings.Contains(items, "from the golden snapshot") {
		t.Errorf("reset did not restore the seeded data: %s", items)
	}

	code, _ := h.get(env, "api", "/healthz")
	if code != http.StatusOK {
		t.Errorf("the env is not healthy after the reset: %d", code)
	}
}

func TestDownLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, "go-react-pg")
	env := h.create("feat/down")
	slug := env.Entry.Slug
	ctx := context.Background()

	if err := h.ctl.Down(ctx, env, envctl.DownOptions{Force: true}); err != nil {
		t.Fatalf("down: %v", err)
	}

	sel := engine.Selector{All: true, Labels: map[string]string{
		meta.LabelManaged: "true", meta.LabelProject: h.proj.Name(), meta.LabelEnv: slug,
	}}
	if cs, err := h.ctl.Runtime.Containers(ctx, sel); err != nil || len(cs) != 0 {
		t.Errorf("%d containers remain (err %v)", len(cs), err)
	}
	if ns, err := h.ctl.Runtime.Networks(ctx, sel); err != nil || len(ns) != 0 {
		t.Errorf("%d networks remain (err %v)", len(ns), err)
	}
	if vs, err := h.ctl.Runtime.Volumes(ctx, sel); err != nil || len(vs) != 0 {
		t.Errorf("%d volumes remain (err %v)", len(vs), err)
	}
	if _, err := os.Stat(env.Entry.Worktree); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	reg, _ := h.ctl.Store.Read()
	if _, err := reg.Env(h.proj.Name(), slug); err == nil {
		t.Error("the registry still claims the env")
	}
}

func TestRouterSurvivesARestart(t *testing.T) {
	h := newHarness(t, "go-react-pg")
	env := h.create("feat/router")

	if code, body := h.get(env, "api", "/healthz"); code != http.StatusOK {
		t.Fatalf("status before the restart: %d\n%s", code, firstLines(body, 40))
	}
	restart := exec.Command("docker", "restart", meta.RouterContainer)
	if out, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("restart the router: %v\n%s", err, out)
	}

	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, body, err := h.try(env, "api", "/healthz")
		switch {
		case err != nil:
			last = err.Error()
		case code == http.StatusOK:
			return
		default:
			last = fmt.Sprintf("status %d: %s", code, firstLines(body, 5))
		}
		time.Sleep(time.Second)
	}
	t.Errorf("the router did not resume routing after a restart; last attempt: %s", last)
}

func TestMySQLFixtureUsesTheSameDriver(t *testing.T) {
	h := newHarness(t, "node-mysql")
	env := h.create("feat/mysql")

	code, body := h.get(env, "api", "/api/items")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if !strings.Contains(body, "from the golden snapshot") {
		t.Errorf("the seeded data did not survive the clone: %s", body)
	}
	st, err := h.ctl.Status(context.Background(), env, envctl.StatusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := st.Snapshots["db"]; !ok || ref.Source == envctl.SourceScratch {
		t.Errorf("the env did not come from a snapshot: %+v", st.Snapshots)
	}
}

func post(t *testing.T, env *envctl.Target, body string) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/items", env.Config.Router.Port)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = env.Hosts().Host("api")
	req.Header.Set("content-type", "application/json")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("POST returned %d: %s", res.StatusCode, b)
	}
}

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
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

		if rel == "fixed" && d.IsDir() {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-qm", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "body{") || strings.Contains(l, "{") && strings.Contains(l, ":") && !strings.Contains(l, "<") {
			continue
		}
		out = append(out, l)
		if len(out) >= n {
			break
		}
	}
	return strings.Join(out, "\n")
}
