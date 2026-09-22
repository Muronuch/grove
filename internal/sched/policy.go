package sched

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/registry"
	"github.com/Muronuch/grove/internal/router"
)

type ActionKind string

const (
	ActionPause ActionKind = "pause"
	ActionStop  ActionKind = "stop"
)

type Action struct {
	Project string     `json:"project"`
	Env     string     `json:"env"`
	Kind    ActionKind `json:"kind"`
	Reason  string     `json:"reason"`
}

type EnvView struct {
	Project string        `json:"project"`
	Env     string        `json:"env"`
	State   string        `json:"state"`
	Pinned  bool          `json:"pinned"`
	Busy    bool          `json:"busy"`
	Held    bool          `json:"held"`
	Idle    time.Duration `json:"idle_ns"`
	Memory  int64         `json:"memory_bytes"`
	CPU     float64       `json:"cpu_percent"`
	Keep    string        `json:"keep,omitempty"`
}

type Plan struct {
	At          time.Time `json:"at"`
	Envs        []EnvView `json:"envs"`
	Actions     []Action  `json:"actions"`
	MemoryUsed  int64     `json:"memory_used_bytes"`
	MemoryLimit int64     `json:"memory_limit_bytes"`
}

func (d *Daemon) Plan(ctx context.Context) (Plan, error) {
	now := time.Now().UTC()
	plan := Plan{At: now}

	reg, err := d.Ctl.Store.Read()
	if err != nil {
		return plan, err
	}
	activity := d.activity(ctx)

	info, err := d.Ctl.Runtime.Info(ctx)
	if err != nil {
		return plan, err
	}

	configs := map[string]*config.Config{}
	for name, ps := range reg.Projects {
		if p, err := d.Ctl.FindProject(ctx, ps.Root); err == nil {
			configs[name] = p.Config
		}
	}

	type candidate struct {
		view EnvView
		cfg  config.SleepConfig
		env  *registry.Env
	}
	var live []candidate
	var budget int64

	for _, e := range reg.All() {
		cfg, ok := configs[e.Project]
		if !ok {
			continue
		}
		sleep := cfg.Sleep
		if budget == 0 {
			budget = sleep.MemoryBudget.Resolve(info.MemTotal)
		}

		view := EnvView{
			Project: e.Project, Env: e.Slug, State: string(e.State),
			Pinned: e.Pinned, Held: e.Held(now),
		}
		if !e.State.Live() {
			plan.Envs = append(plan.Envs, view)
			continue
		}

		if lock, err := d.Ctl.Store.EnvLock(e.Project, e.Slug); err == nil && lock.Busy() {
			view.Busy = true
		}

		stats := activity.stats[e.Project+"/"+e.Slug]
		view.Memory, view.CPU = stats.memory, stats.cpu
		plan.MemoryUsed += stats.memory

		view.Idle = idleFor(e, activity.lastRequest[e.Project+"/"+e.Slug], now)

		switch {
		case !sleep.IsEnabled():
			view.Keep = "sleeping is disabled for this project"
		case e.Pinned:
			view.Keep = "pinned"
		case view.Busy:
			view.Keep = "an operation is in progress"
		case view.Held:
			view.Keep = fmt.Sprintf("held until %s", e.HoldUntil.Format(time.Kitchen))
		case view.CPU > sleep.CPUFloor:

			view.Keep = fmt.Sprintf("busy: %.0f%% CPU", view.CPU)
		}

		plan.Envs = append(plan.Envs, view)
		if view.Keep == "" {
			live = append(live, candidate{view: view, cfg: sleep, env: e})
		}
	}
	plan.MemoryLimit = budget

	acted := map[string]bool{}
	for _, c := range live {
		key := c.view.Project + "/" + c.view.Env
		switch {
		case c.cfg.StopAfter > 0 && c.view.Idle >= c.cfg.StopAfter.Duration() && c.env.State != registry.StateStopped:
			plan.Actions = append(plan.Actions, Action{
				Project: c.view.Project, Env: c.view.Env, Kind: ActionStop,
				Reason: fmt.Sprintf("idle for %s (stop_after %s)", round(c.view.Idle), c.cfg.StopAfter),
			})
			acted[key] = true
		case c.cfg.PauseAfter > 0 && c.view.Idle >= c.cfg.PauseAfter.Duration() && c.env.State == registry.StateRunning:
			plan.Actions = append(plan.Actions, Action{
				Project: c.view.Project, Env: c.view.Env, Kind: ActionPause,
				Reason: fmt.Sprintf("idle for %s (pause_after %s)", round(c.view.Idle), c.cfg.PauseAfter),
			})
			acted[key] = true
		}
	}

	if budget > 0 && plan.MemoryUsed > budget {
		byIdle := make([]candidate, 0, len(live))
		for _, c := range live {
			if !acted[c.view.Project+"/"+c.view.Env] {
				byIdle = append(byIdle, c)
			}
		}
		sort.Slice(byIdle, func(i, j int) bool { return byIdle[i].view.Idle > byIdle[j].view.Idle })

		freed := int64(0)
		for _, c := range byIdle {
			if plan.MemoryUsed-freed <= budget {
				break
			}
			kind := ActionPause
			if c.env.State == registry.StatePaused {
				kind = ActionStop
			}
			plan.Actions = append(plan.Actions, Action{
				Project: c.view.Project, Env: c.view.Env, Kind: kind,
				Reason: fmt.Sprintf("memory budget: %s used of %s",
					humanBytes(plan.MemoryUsed), humanBytes(budget)),
			})
			if kind == ActionStop {
				freed += c.view.Memory
			}
		}
	}
	return plan, nil
}

func idleFor(e *registry.Env, lastRequest time.Time, now time.Time) time.Duration {
	newest := e.LastActivity
	if lastRequest.After(newest) {
		newest = lastRequest
	}
	if newest.IsZero() {
		newest = e.CreatedAt
	}
	if newest.IsZero() {
		return 0
	}
	if d := now.Sub(newest); d > 0 {
		return d
	}
	return 0
}

type sample struct {
	memory int64
	cpu    float64
}

type activitySnapshot struct {
	lastRequest map[string]time.Time
	stats       map[string]sample
}

func (d *Daemon) activity(ctx context.Context) activitySnapshot {
	out := activitySnapshot{
		lastRequest: map[string]time.Time{},
		stats:       map[string]sample{},
	}

	if client, err := d.Ctl.Router.Client(); err == nil {
		if entries, err := client.Activity(ctx); err == nil {
			for k, e := range entries {
				out.lastRequest[k] = e.LastRequestAt
			}
		}
	}

	containers, err := d.Ctl.Runtime.Containers(ctx, engine.Selector{
		Labels: map[string]string{meta.LabelManaged: "true"},
	})
	if err != nil {
		return out
	}
	ids := make([]string, 0, len(containers))
	byID := map[string]string{}
	for _, c := range containers {
		if c.State != engine.StateRunning {
			continue
		}
		project, env := c.Labels[meta.LabelProject], c.Labels[meta.LabelEnv]
		if env == "" {
			continue
		}
		ids = append(ids, c.ID)
		byID[c.ID] = project + "/" + env
	}
	if len(ids) == 0 {
		return out
	}
	stats, err := d.Ctl.Runtime.Stats(ctx, ids)
	if err != nil {
		return out
	}
	for id, s := range stats {
		key := byID[id]
		cur := out.stats[key]
		cur.memory += s.MemoryBytes
		cur.cpu += s.CPUPercent
		out.stats[key] = cur
	}
	return out
}

func RouterState(k ActionKind) router.EnvState {
	if k == ActionStop {
		return router.StateStopped
	}
	return router.StatePaused
}

func round(d time.Duration) time.Duration {
	if d > time.Minute {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}
