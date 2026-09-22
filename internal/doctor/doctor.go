package doctor

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/project"
)

type Options struct {
	Keep        bool
	SkipDynamic bool
	Timeout     time.Duration
	EnvA, EnvB  string
}

const (
	DefaultEnvA = "doctor-a"
	DefaultEnvB = "doctor-b"
)

type Report struct {
	Schema   int           `json:"schema"`
	OK       bool          `json:"ok"`
	Project  string        `json:"project"`
	Findings finding.List  `json:"findings"`
	Timings  []Timing      `json:"timings,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

type Timing struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration_ns"`
}

type Runner struct {
	Ctl      *envctl.Controller
	Project  *project.Project
	Progress io.Writer
	findings finding.List
	timings  []Timing
}

func Run(ctx context.Context, ctl *envctl.Controller, p *project.Project, progress io.Writer, o Options) (Report, error) {
	if o.EnvA == "" {
		o.EnvA = DefaultEnvA
	}
	if o.EnvB == "" {
		o.EnvB = DefaultEnvB
	}
	r := &Runner{Ctl: ctl, Project: p, Progress: progress}
	start := time.Now()

	r.step("checking %s and the compose model", p.Config.Path)
	r.static(ctx)

	if !o.SkipDynamic {
		if err := r.dynamic(ctx, o); err != nil {
			return r.report(p, start), err
		}
	}
	return r.report(p, start), nil
}

func (r *Runner) report(p *project.Project, start time.Time) Report {
	sorted := r.findings.Sorted()
	return Report{
		Schema:   1,
		OK:       !sorted.HasErrors(),
		Project:  p.Name(),
		Findings: sorted,
		Timings:  r.timings,
		Duration: time.Since(start),
	}
}

func (r *Runner) add(f ...finding.Finding) { r.findings.Add(f...) }

func (r *Runner) step(format string, args ...any) {
	if r.Progress != nil {
		fmt.Fprintf(r.Progress, ":: %s\n", fmt.Sprintf(format, args...))
	}
}

func (r *Runner) detail(format string, args ...any) {
	if r.Progress != nil {
		fmt.Fprintf(r.Progress, "   %s\n", fmt.Sprintf(format, args...))
	}
}

func (r *Runner) timed(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	r.timings = append(r.timings, Timing{Name: name, Duration: time.Since(start)})
	return err
}
