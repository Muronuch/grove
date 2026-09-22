package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Muronuch/grove/internal/meta"
)

const dialRetryWindow = 10 * time.Second

const dialTimeout = 2 * time.Second

type Activity struct {
	mu      sync.Mutex
	last    map[string]time.Time
	counts  map[string]int64
	pending map[string]chan struct{}
	browser map[string]bool
	waiters []chan []string
}

func NewActivity() *Activity {
	return &Activity{
		last:    map[string]time.Time{},
		counts:  map[string]int64{},
		pending: map[string]chan struct{}{},
		browser: map[string]bool{},
	}
}

func envKey(project, env string) string { return project + "/" + env }

func (a *Activity) Touch(project, env string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := envKey(project, env)
	a.last[k] = time.Now().UTC()
	a.counts[k]++
}

func (a *Activity) Snapshot() map[string]ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]ActivityEntry, len(a.last))
	for k, t := range a.last {
		out[k] = ActivityEntry{LastRequestAt: t, Requests: a.counts[k]}
	}
	return out
}

type ActivityEntry struct {
	LastRequestAt time.Time `json:"last_request_at"`
	Requests      int64     `json:"requests"`
}

func (a *Activity) RequestWake(project, env string, browser bool) <-chan struct{} {
	a.mu.Lock()
	k := envKey(project, env)
	if browser {
		a.browser[k] = true
	}
	ch, ok := a.pending[k]
	if !ok {
		ch = make(chan struct{})
		a.pending[k] = ch
	}
	waiters := a.waiters
	a.waiters = nil
	keys := a.pendingKeysLocked()
	a.mu.Unlock()

	for _, w := range waiters {
		select {
		case w <- keys:
		default:
		}
	}
	return ch
}

func (a *Activity) pendingKeysLocked() []string {
	out := make([]string, 0, len(a.pending))
	for k := range a.pending {
		out = append(out, k)
	}
	return out
}

func (a *Activity) Woke(project, env string) {
	a.mu.Lock()
	k := envKey(project, env)
	ch, ok := a.pending[k]
	delete(a.pending, k)
	delete(a.browser, k)
	a.mu.Unlock()
	if ok {
		close(ch)
	}
}

type Wake struct {
	Project string `json:"project"`
	Env     string `json:"env"`
	Browser bool   `json:"browser"`
}

func (a *Activity) PendingWakes(ctx context.Context) []string {
	a.mu.Lock()
	if keys := a.pendingKeysLocked(); len(keys) > 0 {
		a.mu.Unlock()
		return keys
	}
	ch := make(chan []string, 1)
	a.waiters = append(a.waiters, ch)
	a.mu.Unlock()

	select {
	case keys := <-ch:
		return keys
	case <-ctx.Done():
		return nil
	}
}

type Proxy struct {
	store      *Store
	activity   *Activity
	log        *slog.Logger
	daemonSeen atomic.Int64
	proxy      *httputil.ReverseProxy
}

func NewProxy(store *Store, activity *Activity, log *slog.Logger) *Proxy {
	p := &Proxy{store: store, activity: activity, log: log}
	transport := &http.Transport{
		DialContext:           p.dial,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,

		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: p.rewrite,

		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler:  p.upstreamError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return p
}

func (p *Proxy) DaemonSeen() { p.daemonSeen.Store(time.Now().Unix()) }

func (p *Proxy) daemonAlive() bool {
	last := p.daemonSeen.Load()
	return last > 0 && time.Since(time.Unix(last, 0)) < 2*time.Minute
}

type ctxKey int

const (
	ctxMatch ctxKey = iota
	ctxEnvState
)

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := normaliseHost(r.Host)

	if p.serveInternal(w, r, host) {
		return
	}

	env, known := p.store.EnvForHost(host)
	if !known {
		p.unknownHost(w, r, host)
		return
	}
	p.activity.Touch(env.Project, env.Env)

	if env.State.Asleep() {
		p.wake(w, r, env)
		return
	}

	if env.State != StateRunning && env.State != StateCreating {
		p.notReady(w, r, env)
		return
	}

	m, ok := p.store.Lookup(host, r.URL.Path)
	if !ok {
		p.noRoute(w, r, env)
		return
	}

	ctx := context.WithValue(r.Context(), ctxMatch, m)
	p.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	m, _ := pr.In.Context().Value(ctxMatch).(Match)
	if m.Route == nil {
		return
	}
	target := &url.URL{Scheme: "http", Host: m.Route.Upstream}
	pr.SetURL(target)
	pr.Out.Host = pr.In.Host
	if m.Route.RewriteHost != "" {
		pr.Out.Host = m.Route.RewriteHost
	}
	pr.SetXForwarded()
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	pr.Out.Header.Set("X-Forwarded-Proto", "http")
	pr.Out.Header.Set(meta.HeaderName("Env"), m.Env.Env)
	pr.Out.Header.Set(meta.HeaderName("Project"), m.Env.Project)
	if m.Env.Slot > 0 {
		pr.Out.Header.Set(meta.HeaderName("Slot"), fmt.Sprint(m.Env.Slot))
	}
}

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	deadline := time.Now().Add(dialRetryWindow)
	for attempt := 0; ; attempt++ {
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil {
			return conn, nil
		}
		if !isRetryableDialError(err) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(100*(1<<min(attempt, 4))) * time.Millisecond
	return min(d, time.Second)
}

func isRetryableDialError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	m, _ := r.Context().Value(ctxMatch).(Match)
	upstream, service, env := "", "", ""
	if m.Route != nil {
		upstream, service = m.Route.Upstream, m.Route.Service
	}
	if m.Env != nil {
		env = m.Env.Env
	}
	p.log.Warn("upstream failed", "host", r.Host, "path", r.URL.Path, "upstream", upstream, "err", err)

	if m.Env != nil && m.Env.State == StateCreating {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		w.Header().Set(ErrorHeader, "router")
		w.WriteHeader(http.StatusServiceUnavailable)
		writePage(w, page{
			Title:          fmt.Sprintf("Starting %s…", env),
			Body:           "<p>The stack is still coming up. This page reloads itself.</p>",
			Refresh:        true,
			RefreshSeconds: 2,
		})
		return
	}

	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set(ErrorHeader, "router")
	w.WriteHeader(http.StatusBadGateway)
	writePage(w, page{
		Title: "502 · upstream did not answer",
		Body: fmt.Sprintf(`<p>The router reached %s for <code>%s</code>, but the connection failed.</p>
<p class="err">%s</p>`, codeOr(upstream, "no upstream"), htmlEscape(r.URL.Path), htmlEscape(err.Error())),
		Hints: []string{
			fmt.Sprintf("Check the service is listening on the port declared in grove.toml: <code>grove logs %s %s</code>.", env, service),
			"A dev server must bind 0.0.0.0, not 127.0.0.1, or nothing outside its own container can reach it.",
		},
	})
}

func (p *Proxy) noRoute(w http.ResponseWriter, r *http.Request, env *EnvRoute) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set(ErrorHeader, "router")
	w.WriteHeader(http.StatusNotFound)
	writePage(w, page{
		Title: "404 · no route for this path",
		Body: fmt.Sprintf("<p>Env <b>%s</b> has no service serving <code>%s</code> on this hostname.</p>",
			htmlEscape(env.Env), htmlEscape(r.URL.Path)),
		Links: env.URLs,
		Hints: []string{"Add the path prefix to the service's <code>paths</code> in grove.toml, or open the service's own hostname."},
	})
}

func (p *Proxy) notReady(w http.ResponseWriter, r *http.Request, env *EnvRoute) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("retry-after", "2")
	w.Header().Set(ErrorHeader, "router")
	w.WriteHeader(http.StatusServiceUnavailable)
	body := fmt.Sprintf("<p>Env <b>%s</b> is <b>%s</b>.</p>", htmlEscape(env.Env), htmlEscape(string(env.State)))
	if env.LastError != "" {
		body += fmt.Sprintf(`<pre class="err">%s</pre>`, htmlEscape(env.LastError))
	}
	hints := []string{fmt.Sprintf("<code>grove status %s</code> shows what each service is doing.", htmlEscape(env.Env))}
	if env.State == StateCreating {
		hints = append(hints, "This page refreshes itself while the env starts.")
	}
	writePage(w, page{Title: "Env is not ready", Body: body, Hints: hints, Refresh: env.State == StateCreating})
}

func codeOr(s, fallback string) string {
	if s == "" {
		return "<i>" + fallback + "</i>"
	}
	return "<code>" + htmlEscape(s) + "</code>"
}

func (a *Activity) PendingList() []Wake {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Wake, 0, len(a.pending))
	for k := range a.pending {
		project, env, _ := strings.Cut(k, "/")
		out = append(out, Wake{Project: project, Env: env, Browser: a.browser[k]})
	}
	return out
}
