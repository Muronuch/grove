package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/routerctl"
)

const (
	LockFile = "daemon.lock"
	PIDFile  = "daemon.pid"
	LogFile  = "daemon.log"
	StopFile = "daemon.stop"
)

var ErrAlreadyRunning = errors.New("a " + meta.Name + " daemon is already running")

type Daemon struct {
	Ctl      *envctl.Controller
	Log      *slog.Logger
	Interval time.Duration
}

func (d *Daemon) Run(ctx context.Context) error {
	store := d.Ctl.Store
	lock, err := registry.NewLock(store.Path("locks", LockFile), "the "+meta.Name+" daemon")
	if err != nil {
		return err
	}
	held, err := lock.TryAcquire()
	if err != nil {
		return err
	}
	if !held {
		return ErrAlreadyRunning
	}
	defer lock.Release()

	_ = os.Remove(store.Path(StopFile))
	if err := os.WriteFile(store.Path(PIDFile), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(store.Path(PIDFile))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	d.Log.Info("daemon started", "pid", os.Getpid(), "home", store.Home())

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); d.wakeLoop(ctx) }()
	go func() { defer wg.Done(); d.sleepLoop(ctx) }()
	go func() { defer wg.Done(); d.stopWatcher(ctx, cancel) }()
	wg.Wait()

	d.Log.Info("daemon stopped")
	return nil
}

func (d *Daemon) stopWatcher(ctx context.Context, cancel context.CancelFunc) {
	path := d.Ctl.Store.Path(StopFile)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := os.Stat(path); err == nil {
				d.Log.Info("stop requested")
				_ = os.Remove(path)
				cancel()
				return
			}
		}
	}
}

func (d *Daemon) wakeLoop(ctx context.Context) {
	for ctx.Err() == nil {
		client, err := d.Ctl.Router.Client()
		if err != nil {
			d.sleep(ctx, 5*time.Second)
			continue
		}
		wakes, err := client.Wake(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			d.Log.Debug("wake poll failed", "err", err)
			d.sleep(ctx, 3*time.Second)
			continue
		}
		for _, w := range wakes {
			d.wake(ctx, w)
		}
	}
}

func (d *Daemon) wake(ctx context.Context, w routerctl.WakeRequest) {
	log := d.Log.With("project", w.Project, "env", w.Env)
	t, err := d.resolve(ctx, w.Project, w.Env)
	if err != nil {
		log.Warn("cannot wake", "err", err)
		d.reportWoken(ctx, w)
		return
	}

	start := time.Now()
	log.Info("waking", "browser", w.Browser)

	err = d.Ctl.Up(ctx, t, envctl.UpOptions{Full: w.Browser})
	if err != nil {
		log.Warn("wake failed", "err", err, "took", time.Since(start))

		_ = d.Ctl.SyncRouter(ctx, t.Project)
		d.reportWoken(ctx, w)
		return
	}
	log.Info("awake", "took", time.Since(start).Round(time.Millisecond))
	d.reportWoken(ctx, w)
}

func (d *Daemon) reportWoken(ctx context.Context, w routerctl.WakeRequest) {
	client, err := d.Ctl.Router.Client()
	if err != nil {
		return
	}
	_ = client.Woke(ctx, w.Project, w.Env)
}

func (d *Daemon) sleepLoop(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.tick(ctx); err != nil && ctx.Err() == nil {
				d.Log.Warn("scheduler tick failed", "err", err)
			}
		}
	}
}

func (d *Daemon) tick(ctx context.Context) error {
	plan, err := d.Plan(ctx)
	if err != nil {
		return err
	}
	for _, a := range plan.Actions {
		d.apply(ctx, a)
	}
	return nil
}

func (d *Daemon) apply(ctx context.Context, a Action) {
	log := d.Log.With("project", a.Project, "env", a.Env, "reason", a.Reason)
	t, err := d.resolve(ctx, a.Project, a.Env)
	if err != nil {
		log.Debug("cannot act", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	switch a.Kind {
	case ActionPause:
		log.Info("pausing")
		if err := d.Ctl.Pause(ctx, t); err != nil {
			log.Warn("pause failed", "err", err)
		}
	case ActionStop:
		log.Info("stopping")
		if err := d.Ctl.Stop(ctx, t, envctl.StopOptions{Grace: 30 * time.Second}); err != nil {
			log.Warn("stop failed", "err", err)
		}
	}
}

func (d *Daemon) resolve(ctx context.Context, projectName, slug string) (*envctl.Target, error) {
	reg, err := d.Ctl.Store.Read()
	if err != nil {
		return nil, err
	}
	ps, ok := reg.LookupProject(projectName)
	if !ok {
		return nil, fmt.Errorf("unknown project %q", projectName)
	}
	e, ok := ps.Envs[slug]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", registry.ErrEnvNotFound, projectName, slug)
	}
	root := ps.Root
	if e.Worktree != "" {
		if _, err := os.Stat(e.Worktree); err == nil {
			root = e.Worktree
		}
	}
	p, err := d.Ctl.FindProject(ctx, root)
	if err != nil {
		return nil, err
	}
	return &envctl.Target{Project: p, Entry: e, Config: p.Config}, nil
}

func (d *Daemon) sleep(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}

func Running(store *registry.Store) bool {
	lock, err := registry.NewLock(store.Path("locks", LockFile), "the daemon")
	if err != nil {
		return false
	}
	return lock.Busy()
}

func PID(store *registry.Store) int {
	b, err := os.ReadFile(store.Path(PIDFile))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func Start(store *registry.Store, extraEnv []string) error {
	if Running(store) {
		return ErrAlreadyRunning
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := store.Path(LogFile)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "daemon", "run", "--home", store.Home())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), extraEnv...)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the daemon: %w", err)
	}

	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if Running(store) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not start; see %s", logPath)
}

func Stop(store *registry.Store, timeout time.Duration) error {
	if !Running(store) {
		return nil
	}
	if err := os.WriteFile(store.Path(StopFile), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !Running(store) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(store.Path(StopFile))
	return fmt.Errorf("the daemon did not stop within %s", timeout)
}

func EnsureRunning(store *registry.Store, log *slog.Logger) {
	if Running(store) {
		return
	}
	if err := Start(store, nil); err != nil && !errors.Is(err, ErrAlreadyRunning) && log != nil {
		log.Debug("could not start the scheduler", "err", err)
	}
}
