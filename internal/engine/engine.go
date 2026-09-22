package engine

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrDockerUnavailable = errors.New("no Docker-compatible engine is reachable")

var ErrNotFound = errors.New("not found")

type Runtime interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (Info, error)
	Containers(ctx context.Context, sel Selector) ([]Container, error)
	Container(ctx context.Context, id string) (Container, error)
	Logs(ctx context.Context, id string, o LogOptions) (io.ReadCloser, error)
	Stats(ctx context.Context, ids []string) (map[string]Stats, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
	Networks(ctx context.Context, sel Selector) ([]Network, error)
	Network(ctx context.Context, id string) (Network, error)
	Connect(ctx context.Context, network, container string, aliases []string) error
	Disconnect(ctx context.Context, network, container string) error
	RemoveNetwork(ctx context.Context, id string) error
	Volumes(ctx context.Context, sel Selector) ([]Volume, error)
	Volume(ctx context.Context, name string) (Volume, error)
	CreateVolume(ctx context.Context, v VolumeSpec) error
	RemoveVolume(ctx context.Context, name string, force bool) error
	ImageID(ctx context.Context, ref string) (string, error)
	EnsureImage(ctx context.Context, ref string) error
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
	CreateAndStart(ctx context.Context, spec ContainerSpec) (string, error)
	Build(ctx context.Context, spec BuildSpec, w io.Writer) error
	Close() error
}

type Selector struct {
	Labels map[string]string
	All    bool
	Names  []string
}

type Info struct {
	Version     string
	APIVersion  string
	OSType      string
	MemTotal    int64
	NCPU        int
	ServerName  string
	Experiment  bool
	DockerRootD string
}

type State string

const (
	StateCreated    State = "created"
	StateRunning    State = "running"
	StatePaused     State = "paused"
	StateRestarting State = "restarting"
	StateRemoving   State = "removing"
	StateExited     State = "exited"
	StateDead       State = "dead"
)

type Health string

const (
	HealthNone      Health = ""
	HealthStarting  Health = "starting"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

type Container struct {
	ID        string
	Name      string
	Image     string
	ImageID   string
	State     State
	Health    Health
	ExitCode  int
	Labels    map[string]string
	Ports     map[string][]HostPort
	Networks  map[string]Endpoint
	CreatedAt time.Time
	StartedAt time.Time
}

func (c Container) ComposeService() string {
	return c.Labels["com.docker.compose.service"]
}

func (c Container) ComposeProject() string {
	return c.Labels["com.docker.compose.project"]
}

type HostPort struct {
	IP   string
	Port int
}

type Endpoint struct {
	NetworkID string
	IPAddress string
	Aliases   []string
}

type Network struct {
	ID         string
	Name       string
	Driver     string
	Labels     map[string]string
	Containers map[string]string
}

type Volume struct {
	Name       string
	Driver     string
	Mountpoint string
	Labels     map[string]string
	CreatedAt  time.Time
	Size       int64
}

type VolumeSpec struct {
	Name   string
	Labels map[string]string
}

type Stats struct {
	MemoryBytes int64
	CPUPercent  float64
}

type LogOptions struct {
	Follow     bool
	Tail       string
	Since      string
	Stdout     bool
	Stderr     bool
	Timestamps bool
}

type Mount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

type RunSpec struct {
	Image   string
	Cmd     []string
	Mounts  []Mount
	Labels  map[string]string
	Env     []string
	Network string
	User    string
	Timeout time.Duration
}

type RunResult struct {
	ExitCode int
	Output   string
	Duration time.Duration
}

type PortSpec struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

type ContainerSpec struct {
	Name     string
	Image    string
	Cmd      []string
	Env      []string
	Labels   map[string]string
	Mounts   []Mount
	Ports    []PortSpec
	Restart  string
	Networks []string
}

type BuildSpec struct {
	ContextDir     string
	Dockerfile     string
	DockerfileBody string
	Tags           []string
	BuildArgs      map[string]string
	Exclude        []string
	Platform       string
}
