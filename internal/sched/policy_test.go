package sched

import (
	"testing"
	"time"

	"github.com/Muronuch/grove/internal/registry"
)

func TestIdleForUsesTheNewestSignal(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		env         registry.Env
		lastRequest time.Time
		want        time.Duration
	}{
		{
			name: "router traffic is newer than the CLI's record",
			env:  registry.Env{LastActivity: now.Add(-30 * time.Minute)},

			lastRequest: now.Add(-2 * time.Minute),
			want:        2 * time.Minute,
		},
		{
			name:        "a CLI command is newer than the last request",
			env:         registry.Env{LastActivity: now.Add(-1 * time.Minute)},
			lastRequest: now.Add(-30 * time.Minute),
			want:        time.Minute,
		},
		{
			name:        "a brand new env falls back to its creation time",
			env:         registry.Env{CreatedAt: now.Add(-5 * time.Minute)},
			lastRequest: time.Time{},
			want:        5 * time.Minute,
		},
		{
			name:        "a clock skew into the future is not negative idleness",
			env:         registry.Env{LastActivity: now.Add(time.Hour)},
			lastRequest: time.Time{},
			want:        0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := idleFor(&tc.env, tc.lastRequest, now); got != tc.want {
				t.Errorf("idle = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRouterStateForAction(t *testing.T) {
	if got := RouterState(ActionPause); string(got) != "paused" {
		t.Errorf("pause → %q", got)
	}
	if got := RouterState(ActionStop); string(got) != "stopped" {
		t.Errorf("stop → %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:     "512B",
		2048:    "2.0KB",
		5 << 20: "5.0MB",
		3 << 30: "3.0GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRoundKeepsShortDurationsReadable(t *testing.T) {
	if got := round(95 * time.Second); got != 2*time.Minute {
		t.Errorf("95s → %s", got)
	}
	if got := round(9500 * time.Millisecond); got != 10*time.Second {
		t.Errorf("9.5s → %s", got)
	}
}
