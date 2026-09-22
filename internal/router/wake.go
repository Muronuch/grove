package router

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/meta"
)

const heldRequestTimeout = 60 * time.Second

func (p *Proxy) wake(w http.ResponseWriter, r *http.Request, env *EnvRoute) {
	woken := p.activity.RequestWake(env.Project, env.Env, isBrowserNavigation(r))

	if !p.daemonAlive() {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set(ErrorHeader, "router")
		w.WriteHeader(http.StatusServiceUnavailable)
		writePage(w, page{
			Title: "Env is asleep",
			Body: fmt.Sprintf("<p>Env <b>%s</b> is <b>%s</b>, and no %s daemon is running to wake it.</p>",
				htmlEscape(env.Env), htmlEscape(string(env.State)), meta.Name),
			Hints: []string{
				fmt.Sprintf("Start it by hand: <code>%s up %s</code>", meta.Name, htmlEscape(env.Env)),
				fmt.Sprintf("Or start the scheduler: <code>%s daemon start</code>", meta.Name),
			},
		})
		return
	}

	if isBrowserNavigation(r) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.Header().Set("cache-control", "no-store")
		w.Header().Set(ErrorHeader, "router")
		w.WriteHeader(http.StatusServiceUnavailable)
		verb := "resuming"
		if env.State == StateStopped {
			verb = "starting"
		}
		writePage(w, page{
			Title:   fmt.Sprintf("Waking up %s…", env.Env),
			Body:    fmt.Sprintf("<p>%s the stack. This page reloads itself.</p>", strings.ToUpper(verb[:1])+verb[1:]),
			Refresh: true,
		})
		return
	}

	select {
	case <-woken:

		w.Header().Set("cache-control", "no-store")
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusTemporaryRedirect)
	case <-time.After(heldRequestTimeout):
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		w.Header().Set("retry-after", "5")
		w.Header().Set(ErrorHeader, "router")
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprintf(w, "%s: env %q did not wake within %s\n", meta.Name, env.Env, heldRequestTimeout)
		if env.LastError != "" {
			fmt.Fprintf(w, "\n%s\n", env.LastError)
		}
	case <-r.Context().Done():
	}
}

func isBrowserNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (p *Proxy) serveInternal(w http.ResponseWriter, r *http.Request, _ string) bool {
	if r.URL.Path != "/"+meta.Name+"/health" {
		return false
	}
	w.Header().Set("content-type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"version":%q}`+"\n", meta.VersionString())
	return true
}
