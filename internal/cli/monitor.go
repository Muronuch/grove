package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/router"
	"github.com/Muronuch/grove/internal/sched"
)

func newMonitorCmd(app *App) *cobra.Command {
	var (
		interval    time.Duration
		once        bool
		allProjects bool
	)
	cmd := &cobra.Command{
		Use:     "monitor",
		Aliases: []string{"top"},
		Short:   "Watch every env live: state, memory, CPU, traffic and what sleeps next",
		Long: `monitor is ` + meta.Name + ` ls that keeps itself up to date, with everything the
scheduler knows: how much memory each env holds, whether it is doing any work,
how long it has been idle, how much traffic it has served, and which env is
about to be paused or stopped and why.

It redraws in place until interrupted. Piped into another program, or with
--once, it prints one frame and exits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := app.controller(ctx)
			if err != nil {
				return err
			}
			defer c.Close()

			c.Progress = nil

			cwd, err := app.Cwd()
			if err != nil {
				return err
			}
			m := &monitor{app: app, ctl: c, cwd: cwd, allProjects: allProjects}

			live := !once && !app.JSONOut && isTerminal(os.Stdout)
			if !live {
				frame, err := m.collect(ctx)
				if err != nil {
					return classify(err)
				}
				if app.JSONOut {
					return app.WriteJSON(frame)
				}
				app.Printf("%s", m.render(frame))
				return nil
			}
			return m.watch(ctx, interval)
		},
	}
	f := cmd.Flags()
	f.DurationVar(&interval, "interval", 2*time.Second, "how often to redraw")
	f.BoolVar(&once, "once", false, "print one frame and exit")
	f.BoolVar(&allProjects, "all-projects", false, "include every project, not just this one")
	return cmd
}

type monitor struct {
	app         *App
	ctl         *envctl.Controller
	cwd         string
	allProjects bool
}

type Frame struct {
	Schema      int                `json:"schema"`
	At          time.Time          `json:"at"`
	Envs        []MonitorEnv       `json:"envs"`
	Actions     []sched.Action     `json:"next,omitempty"`
	Router      *envctl.RouterInfo `json:"router,omitempty"`
	Scheduler   string             `json:"scheduler"`
	MemoryUsed  int64              `json:"memory_used_bytes"`
	MemoryLimit int64              `json:"memory_limit_bytes"`
}

type MonitorEnv struct {
	Project  string  `json:"project"`
	Env      string  `json:"env"`
	Slot     int     `json:"slot"`
	State    string  `json:"state"`
	Pinned   bool    `json:"pinned"`
	Healthy  int     `json:"healthy"`
	Services int     `json:"services"`
	Memory   int64   `json:"memory_bytes"`
	CPU      float64 `json:"cpu_percent"`
	Idle     int64   `json:"idle_seconds"`
	Requests int64   `json:"requests"`
	URL      string  `json:"url"`
	Note     string  `json:"note,omitempty"`
}

func (m *monitor) collect(ctx context.Context) (Frame, error) {
	frame := Frame{Schema: envctl.Schema, At: time.Now()}

	report, err := m.ctl.List(ctx, m.cwd, envctl.StatusOptions{
		Stats: true, AllProjects: m.allProjects,
	})
	if err != nil {
		return frame, err
	}
	frame.Router = report.Router

	d := &sched.Daemon{Ctl: m.ctl, Log: quietLogger()}
	plan, planErr := d.Plan(ctx)
	notes := map[string]string{}
	idle := map[string]time.Duration{}
	if planErr == nil {
		frame.Actions = plan.Actions
		frame.MemoryUsed, frame.MemoryLimit = plan.MemoryUsed, plan.MemoryLimit
		for _, e := range plan.Envs {
			key := e.Project + "/" + e.Env
			notes[key] = e.Keep
			idle[key] = e.Idle
		}
	}

	activity := map[string]router.ActivityEntry{}
	if client, err := m.ctl.Router.Client(); err == nil {
		if a, err := client.Activity(ctx); err == nil {
			activity = a
		}
	}

	store, err := m.app.Store()
	if err != nil {
		return frame, err
	}
	frame.Scheduler = "stopped"
	if sched.Running(store) {
		frame.Scheduler = "running"
	}

	for _, e := range report.Envs {
		key := e.Project + "/" + e.Env
		row := MonitorEnv{
			Project: e.Project, Env: e.Env, Slot: e.Slot, State: e.State,
			Pinned: e.Pinned, Memory: e.MemoryBytes,
			Idle: e.IdleSeconds, Note: notes[key],
			Requests: activity[key].Requests,
			URL:      firstOr(defaultURL(e)),
			Services: len(e.Services),
		}
		if d, ok := idle[key]; ok {
			row.Idle = int64(d.Seconds())
		}
		for _, s := range e.Services {
			row.CPU += s.CPUPercent
			if s.State == "running" && s.Health != "unhealthy" && s.Health != "starting" {
				row.Healthy++
			}
		}
		frame.Envs = append(frame.Envs, row)
	}
	sort.Slice(frame.Envs, func(i, j int) bool {
		if frame.Envs[i].Project != frame.Envs[j].Project {
			return frame.Envs[i].Project < frame.Envs[j].Project
		}
		return frame.Envs[i].Slot < frame.Envs[j].Slot
	})
	return frame, nil
}

func (m *monitor) watch(ctx context.Context, interval time.Duration) error {
	if interval < time.Second {
		interval = time.Second
	}
	out := m.app.Out()
	fmt.Fprint(out, hideCursor)
	defer fmt.Fprint(out, showCursor+"\n")

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		frame, err := m.collect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			fmt.Fprint(out, home+clearBelow)
			fmt.Fprintf(out, "%s %v\n", m.app.red("cannot read the environments:"), err)
		} else {
			fmt.Fprint(out, home+wipeLines(m.render(frame))+clearBelow)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

const (
	home       = "\x1b[H"
	clearLine  = "\x1b[K"
	clearBelow = "\x1b[J"
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
)

func wipeLines(frame string) string {
	return strings.ReplaceAll(frame, "\n", clearLine+"\n")
}

func (m *monitor) render(f Frame) string {
	a := m.app
	var b strings.Builder

	parts := []string{a.bold(meta.Name)}
	parts = append(parts, fmt.Sprintf("%d env%s", len(f.Envs), plural(len(f.Envs))))
	if f.MemoryLimit > 0 {
		mem := fmt.Sprintf("%s / %s", humanBytes(f.MemoryUsed), humanBytes(f.MemoryLimit))
		if f.MemoryUsed > f.MemoryLimit {
			mem = a.red(mem)
		}
		parts = append(parts, mem)
	}
	if f.Router != nil {
		routerPart := fmt.Sprintf("router :%d", f.Router.Port)
		if f.Router.State != "running" {
			routerPart = a.red(fmt.Sprintf("router %s", f.Router.State))
		}
		parts = append(parts, routerPart)
	}
	sched := "scheduler " + f.Scheduler
	if f.Scheduler != "running" {
		sched = a.dim(sched)
	}
	parts = append(parts, sched)
	b.WriteString(strings.Join(parts, a.dim(" · ")) + "\n\n")

	if len(f.Envs) == 0 {
		b.WriteString(a.dim(fmt.Sprintf("no environments; create one with `%s new <branch>`\n", meta.Name)))
		return b.String()
	}

	header := []string{"ENV", "SLOT", "STATE", "UP", "MEM", "CPU", "IDLE", "REQS", "URL"}
	if m.allProjects {
		header = append([]string{"PROJECT"}, header...)
	}
	rows := make([][]string, 0, len(f.Envs))
	for _, e := range f.Envs {
		name := e.Env
		if e.Pinned {
			name += a.dim(" *")
		}
		up, mem, cpu := "—", "—", "—"
		if e.Services > 0 {
			up = fmt.Sprintf("%d/%d", e.Healthy, e.Services)
			if e.Healthy < e.Services && e.State == "running" {
				up = a.yellow(up)
			}
		}
		if e.Memory > 0 {
			mem = humanBytes(e.Memory)
		}
		if e.CPU > 0.05 {
			cpu = strconv.FormatFloat(e.CPU, 'f', 1, 64) + "%"
		}
		reqs := "—"
		if e.Requests > 0 {
			reqs = strconv.FormatInt(e.Requests, 10)
		}
		row := []string{
			name, strconv.Itoa(e.Slot), a.stateLabel(e.State), up, mem, cpu,
			humanDuration(time.Duration(e.Idle) * time.Second), reqs,
			a.dim(e.URL),
		}
		if m.allProjects {
			row = append([]string{e.Project}, row...)
		}
		rows = append(rows, row)
	}
	b.WriteString(renderTable(a.dimEach(header), rows))

	if len(f.Actions) > 0 {
		b.WriteString("\n")
		for _, act := range f.Actions {
			b.WriteString(fmt.Sprintf("%s %s %s %s\n",
				a.dim("next:"), act.Env, a.yellow(string(act.Kind)), a.dim("— "+act.Reason)))
		}
	}
	if notes := exemptions(f.Envs); len(notes) > 0 {
		b.WriteString("\n")
		for _, n := range notes {
			b.WriteString(a.dim("held: "+n) + "\n")
		}
	}
	return b.String()
}

func exemptions(envs []MonitorEnv) []string {
	var out []string
	for _, e := range envs {
		if e.Note != "" && e.State == "running" {
			out = append(out, fmt.Sprintf("%s — %s", e.Env, e.Note))
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstOr(url string, ok bool) string {
	if !ok {
		return ""
	}
	return url
}
