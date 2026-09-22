package state

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
)

type VolumeCopy struct {
	Runtime engine.Runtime
	Image   string
}

const DefaultHelperImage = "busybox:1.37.0"

func (d *VolumeCopy) image() string {
	if d.Image != "" {
		return d.Image
	}
	return DefaultHelperImage
}

func (d *VolumeCopy) Name() string { return "volume-copy" }

func (d *VolumeCopy) Save(ctx context.Context, src string, snap Ref) (Stats, error) {
	dst := snap.Volume()
	if err := d.Runtime.CreateVolume(ctx, engine.VolumeSpec{
		Name: dst,
		Labels: map[string]string{
			meta.LabelManaged: "true",
			meta.LabelRole:    "snapshot",
			meta.LabelProject: snap.Project,
			meta.LabelService: snap.Service,
			meta.LabelKey:     snap.Key,
			meta.LabelCreated: time.Now().UTC().Format(time.RFC3339),
		},
	}); err != nil {
		return Stats{}, err
	}
	stats, err := d.copy(ctx, src, dst, true)
	if err != nil {
		_ = d.Runtime.RemoveVolume(context.WithoutCancel(ctx), dst, true)
		return Stats{}, err
	}
	return stats, nil
}

func (d *VolumeCopy) Clone(ctx context.Context, snap Ref, dst VolumeTarget) (Stats, error) {
	ok, err := d.Exists(ctx, snap)
	if err != nil {
		return Stats{}, err
	}
	if !ok {
		return Stats{}, fmt.Errorf("%w: %s", ErrNoSnapshot, snap)
	}
	if err := d.Runtime.CreateVolume(ctx, engine.VolumeSpec{Name: dst.Name, Labels: dst.Labels()}); err != nil {
		return Stats{}, err
	}
	return d.copy(ctx, snap.Volume(), dst.Name, false)
}

func (d *VolumeCopy) Exists(ctx context.Context, snap Ref) (bool, error) {
	_, err := d.Runtime.Volume(ctx, snap.Volume())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, engine.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (d *VolumeCopy) Remove(ctx context.Context, snap Ref) error {
	return d.Runtime.RemoveVolume(ctx, snap.Volume(), true)
}

func (d *VolumeCopy) Size(ctx context.Context, snap Ref) (int64, error) {
	res, err := d.Runtime.Run(ctx, engine.RunSpec{
		Image:   d.image(),
		Cmd:     []string{"sh", "-c", "du -sk /vol 2>/dev/null | cut -f1"},
		Mounts:  []engine.Mount{{Volume: snap.Volume(), Target: "/vol", ReadOnly: true}},
		User:    "0:0",
		Timeout: 2 * time.Minute,
	})
	if err != nil || res.ExitCode != 0 {
		return -1, nil
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(lastToken(res.Output)), 10, 64)
	if err != nil {
		return -1, nil
	}
	return kb * 1024, nil
}

func (d *VolumeCopy) copy(ctx context.Context, src, dst string, readOnlySource bool) (Stats, error) {
	start := time.Now()
	script := "set -e; " +
		"if [ -z \"$(ls -A /to 2>/dev/null)\" ]; then :; else rm -rf /to/* /to/.[!.]* /to/..?* 2>/dev/null || true; fi; " +
		"cp -a /from/. /to/; " +
		"du -sk /to 2>/dev/null | cut -f1"

	res, err := d.Runtime.Run(ctx, engine.RunSpec{
		Image: d.image(),
		Cmd:   []string{"sh", "-c", script},
		Mounts: []engine.Mount{
			{Volume: src, Target: "/from", ReadOnly: readOnlySource},
			{Volume: dst, Target: "/to"},
		},

		User:    "0:0",
		Timeout: 2 * time.Hour,
	})
	if err != nil {
		return Stats{}, fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	if res.ExitCode != 0 {
		return Stats{}, fmt.Errorf("copy %s → %s failed (exit %d):\n%s",
			src, dst, res.ExitCode, engine.LastLines(res.Output, 20))
	}
	stats := Stats{Duration: time.Since(start)}
	if kb, err := strconv.ParseInt(strings.TrimSpace(lastToken(res.Output)), 10, 64); err == nil {
		stats.Bytes = kb * 1024
	}
	return stats, nil
}

func lastToken(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
