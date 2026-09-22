package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
)

type App struct {
	Home      string
	JSONOut   bool
	Verbose   bool
	Quiet     bool
	Dir       string
	Color     bool
	Trust     bool
	out       io.Writer
	err       io.Writer
	storeOnce sync.Once
	store     *registry.Store
	storeErr  error
}

type appKey struct{}

func withApp(ctx context.Context, a *App) context.Context {
	return context.WithValue(ctx, appKey{}, a)
}

func (a *App) Out() io.Writer { return a.out }

func (a *App) Err() io.Writer { return a.err }

func (a *App) Store() (*registry.Store, error) {
	a.storeOnce.Do(func() {
		a.store, a.storeErr = registry.Open(a.Home)
	})
	return a.store, a.storeErr
}

func (a *App) Cwd() (string, error) {
	if a.Dir != "" {
		return a.Dir, nil
	}
	return os.Getwd()
}

func (a *App) Printf(format string, args ...any) {
	fmt.Fprintf(a.out, format, args...)
}

func (a *App) Step(format string, args ...any) {
	if a.Quiet {
		return
	}
	fmt.Fprintf(a.err, "%s %s\n", a.dim("::"), fmt.Sprintf(format, args...))
}

func (a *App) Detail(format string, args ...any) {
	if !a.Verbose || a.Quiet {
		return
	}
	fmt.Fprintf(a.err, "   %s\n", a.dim(fmt.Sprintf(format, args...)))
}

func (a *App) Warn(format string, args ...any) {
	fmt.Fprintf(a.err, "%s %s\n", a.yellow("warning:"), fmt.Sprintf(format, args...))
}

func (a *App) WriteJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) color(code, s string) string {
	if !a.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *App) dim(s string) string    { return a.color("2", s) }
func (a *App) bold(s string) string   { return a.color("1", s) }
func (a *App) red(s string) string    { return a.color("31", s) }
func (a *App) green(s string) string  { return a.color("32", s) }
func (a *App) yellow(s string) string { return a.color("33", s) }

func (a *App) setupLogging() {
	level := slog.LevelWarn
	if a.Verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(a.err, &slog.HandlerOptions{Level: level})))
}

const (
	ExitOK          = 0
	ExitGeneric     = 1
	ExitUsage       = 2
	ExitEnvNotFound = 3
	ExitNoDocker    = 4
	ExitUnhealthy   = 5
	ExitDoctor      = 6
	ExitLockBusy    = 7
)

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func exitf(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

func usageErr(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	switch {
	case errors.Is(err, registry.ErrEnvNotFound):
		return ExitEnvNotFound
	case errors.Is(err, registry.ErrLockBusy):
		return ExitLockBusy
	case errors.Is(err, context.Canceled):
		return ExitGeneric
	}
	return ExitGeneric
}

func (a *App) reportError(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if a.JSONOut {
		_ = a.WriteJSON(map[string]any{
			"schema": 1,
			"ok":     false,
			"error":  err.Error(),
			"exit":   exitCode(err),
		})
		return
	}
	msg := err.Error()

	prefix := a.red(meta.Name + ":")
	if strings.Contains(msg, "\n") {
		fmt.Fprintf(a.err, "%s %s\n", prefix, msg)
		return
	}
	fmt.Fprintf(a.err, "%s %s\n", prefix, msg)
}
