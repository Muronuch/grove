package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/state"
)

func (a *App) controller(ctx context.Context) (*envctl.Controller, error) {
	store, err := a.Store()
	if err != nil {
		return nil, err
	}
	rt, err := engine.NewDocker(ctx)
	if err != nil {
		return nil, &ExitError{Code: ExitNoDocker, Err: err}
	}
	c := &envctl.Controller{
		Store:    store,
		Runtime:  rt,
		Progress: a.Err(),
		Verbose:  a.Verbose,
		Trust:    a.Trust,
		Confirm:  a.confirmHooks,
	}
	if a.Quiet || a.JSONOut {
		c.Progress = nil
	}
	c.Router = newRouterManager(rt, store, c.Progress)
	c.Index = state.NewStore(store)
	return c, nil
}

func (a *App) resolve(ctx context.Context, name string) (*envctl.Controller, *envctl.Target, error) {
	c, err := a.controller(ctx)
	if err != nil {
		return nil, nil, err
	}
	cwd, err := a.Cwd()
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	t, err := c.Resolve(ctx, cwd, name)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, t, nil
}

func (a *App) confirmHooks(prompt string, commands []string) (bool, error) {
	if a.JSONOut {
		return false, errors.New("host hooks need approval; run the command interactively once, or pass --trust")
	}
	fmt.Fprintf(a.Err(), "\n%s %s\n", a.yellow("!"), prompt)
	for _, c := range commands {
		fmt.Fprintf(a.Err(), "    %s\n", c)
	}
	fmt.Fprintf(a.Err(), "\nThey run with your privileges, in the new worktree.\n")
	return confirm(a, "Run them?")
}

func (a *App) table(header []string, rows [][]string) {
	fmt.Fprint(a.Out(), renderTable(a.dimEach(header), rows))
}

func (a *App) dimEach(header []string) []string {
	out := make([]string, len(header))
	for i, h := range header {
		out[i] = a.dim(h)
	}
	return out
}

func renderTable(header []string, rows [][]string) string {
	cols := len(header)
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	width := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			width[i] = max(width[i], visibleWidth(c))
		}
	}
	measure(header)
	for _, r := range rows {
		measure(r)
	}

	var b strings.Builder
	line := func(cells []string) {
		for i, c := range cells {
			b.WriteString(c)

			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", width[i]-visibleWidth(c)+2))
			}
		}
		b.WriteString("\n")
	}
	if len(header) > 0 {
		line(header)
	}
	for _, r := range rows {
		line(r)
	}
	return b.String()
}

func visibleWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

func skipEscape(s string, i int) int {
	j := i + 1
	if j >= len(s) || s[j] != '[' {
		return j
	}

	for j++; j < len(s) && (s[j] < '@' || s[j] > '~'); j++ {
	}
	if j < len(s) {
		j++
	}
	return j
}

func (a *App) stateLabel(s string) string {
	switch registry.State(s) {
	case registry.StateRunning:
		return a.green(s)
	case registry.StateFailed, registry.StateMissing:
		return a.red(s)
	case registry.StatePaused, registry.StateStopped:
		return a.dim(s)
	default:
		return s
	}
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%cB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func (a *App) printURLs(t *envctl.Target) {
	urls := t.Hosts().URLs()
	def, hasDefault := t.Config.DefaultService()
	names := make([]string, 0, len(urls))
	for n := range urls {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if hasDefault {
			if names[i] == def.Name {
				return true
			}
			if names[j] == def.Name {
				return false
			}
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		a.Printf("%-10s %s\n", n, urls[n])
	}
	for _, svc := range sortedMapKeys(t.Entry.TCP) {
		a.Printf("%-10s %s\n", svc, t.Entry.TCP[svc])
	}
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, engine.ErrDockerUnavailable):
		return &ExitError{Code: ExitNoDocker, Err: err}
	case errors.Is(err, envctl.ErrUnhealthy):
		return &ExitError{Code: ExitUnhealthy, Err: err}
	case errors.Is(err, registry.ErrLockBusy):
		return &ExitError{Code: ExitLockBusy, Err: err}
	case errors.Is(err, registry.ErrEnvNotFound), errors.Is(err, envctl.ErrNotInEnv):
		return &ExitError{Code: ExitEnvNotFound, Err: err}
	}
	return err
}

func (a *App) hint(format string, args ...any) {
	if a.JSONOut || a.Quiet {
		return
	}
	fmt.Fprintf(a.Err(), "%s %s\n", a.dim("hint:"), fmt.Sprintf(format, args...))
}
