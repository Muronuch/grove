package router

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func upstream(t *testing.T, name string) (addr string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		hj, ok := w.(http.Hijacker)
		if !ok || key == "" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]))
		brw.Flush()

		line, err := brw.Reader.ReadString('\n')
		if err != nil {
			return
		}
		fmt.Fprintf(conn, "%s echoes %s", name, line)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-upstream", name)
		fmt.Fprintf(w, "%s %s host=%s fwd=%s", name, r.URL.Path, r.Host, r.Header.Get("X-Forwarded-Host"))
	})
	srv := httptest.NewServer(mux)
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

func newTestProxy(t *testing.T, envs ...EnvRoute) *httptest.Server {
	t.Helper()
	store := NewStore()
	store.Replace(Table{Version: 1, Envs: envs})
	p := NewProxy(store, NewActivity(), quietLogger())
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, host, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestProxyRoutesToTheRightUpstream(t *testing.T) {
	webA, stop1 := upstream(t, "web-a")
	defer stop1()
	apiA, stop2 := upstream(t, "api-a")
	defer stop2()
	webB, stop3 := upstream(t, "web-b")
	defer stop3()

	srv := newTestProxy(t,
		EnvRoute{
			Project: "p", Env: "a", Slot: 1, State: StateRunning,
			Hosts: []string{"a.p.localhost", "api.a.p.localhost"},
			Routes: []Route{
				{Path: "/", Upstream: webA, Service: "web"},
				{Path: "/api", Upstream: apiA, Service: "api"},
				{HostPrefix: "api.", Path: "/", Upstream: apiA, Service: "api"},
			},
		},
		EnvRoute{
			Project: "p", Env: "b", Slot: 2, State: StateRunning,
			Hosts:  []string{"b.p.localhost"},
			Routes: []Route{{Path: "/", Upstream: webB, Service: "web"}},
		},
	)

	if _, body := get(t, srv, "a.p.localhost", "/"); !strings.HasPrefix(body, "web-a") {
		t.Errorf("a → %q", body)
	}
	if _, body := get(t, srv, "a.p.localhost", "/api/items"); !strings.HasPrefix(body, "api-a") {
		t.Errorf("a/api → %q", body)
	}
	if _, body := get(t, srv, "api.a.p.localhost", "/x"); !strings.HasPrefix(body, "api-a") {
		t.Errorf("api.a → %q", body)
	}

	if _, body := get(t, srv, "b.p.localhost", "/"); !strings.HasPrefix(body, "web-b") {
		t.Errorf("b → %q", body)
	}
}

func TestProxyPreservesHostAndSetsForwardedHeaders(t *testing.T) {
	up, stop := upstream(t, "web")
	defer stop()
	srv := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StateRunning,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/", Upstream: up, Service: "web"}},
	})
	_, body := get(t, srv, "a.p.localhost", "/")
	if !strings.Contains(body, "host=a.p.localhost") {
		t.Errorf("Host was not preserved: %q", body)
	}
	if !strings.Contains(body, "fwd=a.p.localhost") {
		t.Errorf("X-Forwarded-Host was not set: %q", body)
	}
}

func TestProxyRewritesHostWhenAsked(t *testing.T) {
	up, stop := upstream(t, "web")
	defer stop()
	srv := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StateRunning,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/", Upstream: up, Service: "web", RewriteHost: "localhost:5173"}},
	})
	_, body := get(t, srv, "a.p.localhost", "/")
	if !strings.Contains(body, "host=localhost:5173") {
		t.Errorf("rewrite_host was not applied: %q", body)
	}

	if !strings.Contains(body, "fwd=a.p.localhost") {
		t.Errorf("X-Forwarded-Host was lost: %q", body)
	}
}

func TestProxyMarksItsOwnErrorResponses(t *testing.T) {
	srv := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StateRunning,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/", Upstream: "127.0.0.1:1", Service: "web"}},
	})

	res, body := get(t, srv, "nope.p.localhost", "/")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host status = %d", res.StatusCode)
	}
	if res.Header.Get(ErrorHeader) == "" {
		t.Error("a router-generated response must carry the error header")
	}
	if !strings.Contains(body, "a.p.localhost") {
		t.Error("the 404 page should list the project's envs")
	}
}

func TestProxyHTMLEscapesUntrustedValues(t *testing.T) {
	srv := newTestProxy(t)
	_, body := get(t, srv, "x.p.localhost", "/")
	if strings.Contains(body, "<script>") {
		t.Error("unescaped markup in the error page")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "a<script>alert(1)</script>.p.localhost"
	res, err := srv.Client().Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if strings.Contains(string(b), "<script>alert(1)</script>") {
		t.Error("the hostname was not escaped into the page")
	}
}

func TestProxySleepingEnvWithoutDaemon(t *testing.T) {
	srv := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StatePaused,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/", Upstream: "127.0.0.1:1", Service: "web"}},
	})
	res, body := get(t, srv, "a.p.localhost", "/")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if !strings.Contains(body, "daemon") {
		t.Errorf("the page should say no daemon is running: %q", body)
	}
}

func TestProxyForwardsWebSocketUpgrade(t *testing.T) {
	up, stop := upstream(t, "api")
	defer stop()
	srv := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StateRunning,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/ws", Upstream: up, Service: "api", WebsocketPath: "/ws"}},
	})

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprint(conn, "GET /ws HTTP/1.1\r\nHost: a.p.localhost\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	fmt.Fprint(conn, "ping\n")
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(echo, "api echoes ping") {
		t.Errorf("echo = %q", strings.TrimSpace(echo))
	}
}

func TestProxyStreamsWithoutBuffering(t *testing.T) {
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-ready
		fmt.Fprint(w, "data: second\n\n")
	}))
	defer srv.Close()

	proxy := newTestProxy(t, EnvRoute{
		Project: "p", Env: "a", State: StateRunning,
		Hosts:  []string{"a.p.localhost"},
		Routes: []Route{{Path: "/", Upstream: strings.TrimPrefix(srv.URL, "http://"), Service: "api"}},
	})

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/events", nil)
	req.Host = "a.p.localhost"
	res, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	buf := make([]byte, 64)
	done := make(chan string, 1)
	go func() {
		n, _ := res.Body.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "first") {
			t.Errorf("first chunk = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Error("the first chunk was buffered instead of flushed")
	}
	close(ready)
}
