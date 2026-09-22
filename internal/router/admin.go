package router

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/meta"
)

type Admin struct {
	store     *Store
	activity  *Activity
	proxy     *Proxy
	token     string
	log       *slog.Logger
	statePath string
}

func NewAdmin(store *Store, activity *Activity, proxy *Proxy, token, statePath string, log *slog.Logger) *Admin {
	return &Admin{store: store, activity: activity, proxy: proxy, token: token, statePath: statePath, log: log}
}

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", a.health)
	mux.Handle("PUT /v1/routes", a.auth(http.HandlerFunc(a.putRoutes)))
	mux.Handle("GET /v1/routes", a.auth(http.HandlerFunc(a.getRoutes)))
	mux.Handle("PATCH /v1/routes/state", a.auth(http.HandlerFunc(a.patchState)))
	mux.Handle("GET /v1/activity", a.auth(http.HandlerFunc(a.getActivity)))
	mux.Handle("GET /v1/wake", a.auth(http.HandlerFunc(a.getWake)))
	mux.Handle("POST /v1/woke", a.auth(http.HandlerFunc(a.postWoke)))
	return mux
}

func (a *Admin) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(a.token)) != 1 {
			w.Header().Set("www-authenticate", `Bearer realm="`+meta.Name+`"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type HealthResponse struct {
	OK      bool   `json:"ok"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Routes  int64  `json:"routes_version"`
	Envs    int    `json:"envs"`
	Uptime  string `json:"uptime"`
}

var started = time.Now()

func (a *Admin) health(w http.ResponseWriter, _ *http.Request) {
	t := a.store.Table()
	writeJSON(w, http.StatusOK, HealthResponse{
		OK: true, Name: meta.Name, Version: meta.VersionString(),
		Routes: t.Version, Envs: len(t.Envs), Uptime: time.Since(started).Round(time.Second).String(),
	})
}

func (a *Admin) putRoutes(w http.ResponseWriter, r *http.Request) {
	var t Table
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	accepted, current := a.store.Replace(t)
	if !accepted {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the router already has a newer route table",
			"have":  current, "got": t.Version,
		})
		return
	}
	a.persist()
	a.log.Info("route table replaced", "version", t.Version, "envs", len(t.Envs))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": current})
}

func (a *Admin) getRoutes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.store.Table())
}

type statePatch struct {
	Project string   `json:"project"`
	Env     string   `json:"env"`
	State   EnvState `json:"state"`
}

func (a *Admin) patchState(w http.ResponseWriter, r *http.Request) {
	var p statePatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !a.store.SetState(p.Project, p.Env, p.State) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown env " + p.Project + "/" + p.Env})
		return
	}
	if p.State == StateRunning {
		a.activity.Woke(p.Project, p.Env)
	}
	a.persist()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) getActivity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.activity.Snapshot())
}

const wakeTimeout = 30 * time.Second

func (a *Admin) getWake(w http.ResponseWriter, r *http.Request) {
	a.proxy.DaemonSeen()
	ctx, cancel := context.WithTimeout(r.Context(), wakeTimeout)
	defer cancel()
	a.activity.PendingWakes(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"wake": a.activity.PendingList()})
}

func (a *Admin) postWoke(w http.ResponseWriter, r *http.Request) {
	var p statePatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.activity.Woke(p.Project, p.Env)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) persist() {
	if a.statePath == "" {
		return
	}
	b, err := json.MarshalIndent(a.store.Table(), "", "  ")
	if err != nil {
		a.log.Warn("could not serialise the route table", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.statePath), 0o755); err != nil {
		a.log.Warn("could not create the router state directory", "err", err)
		return
	}
	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		a.log.Warn("could not write the route table", "err", err)
		return
	}
	if err := os.Rename(tmp, a.statePath); err != nil {
		a.log.Warn("could not replace the route table", "err", err)
	}
}

func (a *Admin) Restore() error {
	if a.statePath == "" {
		return nil
	}
	b, err := os.ReadFile(a.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", a.statePath, err)
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("parse %s: %w", a.statePath, err)
	}
	a.store.Replace(t)
	a.log.Info("restored route table", "version", t.Version, "envs", len(t.Envs))
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
