package envctl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
)

type PrepareResult struct {
	Service  string        `json:"service"`
	ExitCode int           `json:"exit_code"`
	Output   string        `json:"output"`
	Duration time.Duration `json:"duration_ns"`
}

func (c *Controller) PrepareState(ctx context.Context, t *Target) error {
	err := c.prepareOnce(ctx, t)
	if err != nil && c.usedSnapshot(t) {
		c.step("prepare failed on the cloned database, starting from an empty one instead")
		c.detail("%s", shortError(err))
		c.hintScratchFallback()
		if rerr := c.ResetState(ctx, t, ResetOptions{Scratch: true, SkipPrepare: true}); rerr != nil {
			return fmt.Errorf("%w\n\nstarting from an empty database also failed: %v", err, rerr)
		}
		err = c.prepareOnce(ctx, t)
	}
	if err != nil {
		return err
	}
	return c.cacheBranchSnapshot(ctx, t)
}

func (c *Controller) prepareOnce(ctx context.Context, t *Target) error {
	results, err := c.RunPrepare(ctx, t)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.ExitCode != 0 {
			return fmt.Errorf("prepare for %s exited with code %d:\n%s",
				r.Service, r.ExitCode, indent(engine.LastLines(r.Output, 40), "  "))
		}
	}
	return nil
}

func (c *Controller) usedSnapshot(t *Target) bool {
	for _, ref := range t.Entry.Snapshots {
		switch ref.Source {
		case SourceGolden, SourceExact, SourceCheckpoint:
			return true
		}
	}
	return false
}

func (c *Controller) hintScratchFallback() {
	c.detail("this usually means the branch is behind the default branch and its " +
		"migration tool rejects applied migrations it does not know about")
}

func (c *Controller) RunPrepare(ctx context.Context, t *Target) ([]PrepareResult, error) {
	if len(t.Config.Stateful) == 0 {
		return nil, nil
	}
	stack, err := c.Prepare(ctx, t, PrepareOptions{Headless: t.Entry.Headless})
	if err != nil {
		return nil, err
	}
	return c.runPrepareOn(ctx, t, stack)
}

func (c *Controller) runPrepareOn(ctx context.Context, t *Target, stack *Stack) ([]PrepareResult, error) {
	var out []PrepareResult
	for _, st := range t.Config.Stateful {
		if !st.Prepare.Defined() {
			continue
		}
		if _, ok := stack.Model.Services[st.Service]; !ok {
			c.detail("skipping prepare for %s: it is not part of this stack", st.Service)
			continue
		}
		res, err := c.runOnePrepare(ctx, t, stack, st)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func (c *Controller) runOnePrepare(ctx context.Context, t *Target, stack *Stack, st config.StatefulConfig) (PrepareResult, error) {
	timeout := st.Prepare.Timeout.Duration()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c.step("preparing %s: %s", st.Service, strings.Join(st.Prepare.Command, " "))
	start := time.Now()

	var (
		code int
		out  string
		err  error
	)
	switch st.Prepare.Mode {
	case config.PrepareExec:
		code, out, err = stack.Compose.ExecOnce(ctx, st.Prepare.Service, st.Prepare.Command)
	case config.PrepareHost:
		code, out, err = c.prepareOnHost(ctx, t, st)
	default:
		code, out, err = stack.Compose.RunOnce(ctx, st.Prepare.Service, st.Prepare.Command)
	}
	if err != nil {
		return PrepareResult{Service: st.Service, Output: out}, fmt.Errorf("prepare for %s: %w", st.Service, err)
	}
	res := PrepareResult{Service: st.Service, ExitCode: code, Output: out, Duration: time.Since(start)}
	c.detail("prepare for %s finished in %s (exit %d)", st.Service, res.Duration.Round(time.Millisecond), code)
	if c.Verbose && out != "" {
		fmt.Fprint(c.Progress, indent(strings.TrimRight(out, "\n"), "   ")+"\n")
	}
	return res, nil
}

func (c *Controller) prepareOnHost(ctx context.Context, t *Target, st config.StatefulConfig) (int, string, error) {
	env := []string{
		meta.EnvVarName("ENV") + "=" + t.Entry.Slug,
		meta.EnvVarName("PROJECT") + "=" + t.Entry.Project,
		meta.EnvVarName("SLOT") + "=" + fmt.Sprint(t.Entry.Slot),
	}
	for svc, addr := range t.Entry.TCP {
		host, port, _ := strings.Cut(addr, ":")
		env = append(env,
			meta.EnvVarName("TCP", svc)+"="+addr,
			meta.EnvVarName("TCP", svc, "HOST")+"="+host,
			meta.EnvVarName("TCP", svc, "PORT")+"="+port,
		)
	}
	dir := t.Entry.Worktree
	if dir == "" {
		dir = t.Project.Root
	}
	var buf strings.Builder
	err := project.RunHook(ctx, dir, strings.Join(st.Prepare.Command, " "), env, &buf)
	if err != nil {
		return 1, buf.String(), nil
	}
	return 0, buf.String(), nil
}
