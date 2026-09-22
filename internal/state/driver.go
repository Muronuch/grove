package state

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
)

type Kind string

const (
	KindGolden     Kind = "golden"
	KindBranch     Kind = "branch"
	KindCheckpoint Kind = "checkpoint"
)

type Ref struct {
	Project string
	Service string
	Key     string
}

func (r Ref) Volume() string {
	return meta.SnapshotVolume(r.Project, r.Service, meta.SnapshotKeyShort(r.Key))
}

func (r Ref) Short() string { return meta.SnapshotKeyShort(r.Key) }

func (r Ref) String() string { return r.Project + "/" + r.Service + "@" + r.Short() }

func CheckpointRef(project, service, env, name string) Ref {
	return Ref{Project: project, Service: service, Key: "cp-" + env + "-" + sanitise(name)}
}

func (r Ref) IsCheckpoint() bool { return strings.HasPrefix(r.Key, "cp-") }

func sanitise(s string) string {
	var b strings.Builder
	last := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			last = false
		default:
			if !last {
				b.WriteByte('-')
				last = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

type Driver interface {
	Name() string
	Save(ctx context.Context, src string, snap Ref) (Stats, error)
	Clone(ctx context.Context, snap Ref, dst VolumeTarget) (Stats, error)
	Exists(ctx context.Context, snap Ref) (bool, error)
	Remove(ctx context.Context, snap Ref) error
	Size(ctx context.Context, snap Ref) (int64, error)
}

type VolumeTarget struct {
	Name           string
	ComposeProject string
	ComposeVolume  string
	Project        string
	Env            string
}

func (v VolumeTarget) Labels() map[string]string {
	return map[string]string{
		meta.ComposeLabelProject: v.ComposeProject,
		meta.ComposeLabelVolume:  v.ComposeVolume,
		meta.LabelManaged:        "true",
		meta.LabelProject:        v.Project,
		meta.LabelEnv:            v.Env,
	}
}

type Stats struct {
	Bytes    int64
	Duration time.Duration
}

func (s Stats) Rate() string {
	if s.Duration <= 0 || s.Bytes <= 0 {
		return ""
	}
	mbps := float64(s.Bytes) / (1 << 20) / s.Duration.Seconds()
	return fmt.Sprintf("%.0f MB/s", mbps)
}

var ErrNoSnapshot = fmt.Errorf("snapshot not found")

func New(name string, rt engine.Runtime, helperImage string) (Driver, error) {
	switch name {
	case "", "volume-copy":
		return &VolumeCopy{Runtime: rt, Image: helperImage}, nil
	default:
		return nil, fmt.Errorf("unknown snapshot driver %q (available: volume-copy)", name)
	}
}
