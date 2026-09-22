package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/Muronuch/grove/internal/meta"
)

const FileName = meta.Name + ".toml"

var ErrNotFound = errors.New("no " + FileName + " found")

type Config struct {
	Project  ProjectConfig     `toml:"project"`
	Compose  ComposeConfig     `toml:"compose"`
	Worktree WorktreeConfig    `toml:"worktree"`
	Router   RouterConfig      `toml:"router"`
	Service  []ServiceConfig   `toml:"service"`
	Env      map[string]EnvMap `toml:"env"`
	Stateful []StatefulConfig  `toml:"stateful"`
	Cache    []CacheConfig     `toml:"cache"`
	Limits   map[string]Limit  `toml:"limits"`
	Hooks    HooksConfig       `toml:"hooks"`
	State    StateConfig       `toml:"state"`
	Sleep    SleepConfig       `toml:"sleep"`
	Agent    AgentConfig       `toml:"agent"`
	Up       UpConfig          `toml:"up"`
	Doctor   DoctorConfig      `toml:"doctor"`
	Path     string            `toml:"-"`
	Root     string            `toml:"-"`
	raw      []byte
}

type EnvMap map[string]string

type ProjectConfig struct {
	Name          string `toml:"name"`
	DefaultBranch string `toml:"default_branch"`
	MaxSlots      int    `toml:"max_slots"`
}

type ComposeConfig struct {
	Files         []string `toml:"files"`
	Profiles      []string `toml:"profiles"`
	SharedVolumes []string `toml:"shared_volumes"`
	EnvFile       []string `toml:"env_file"`
}

type WorktreeConfig struct {
	Dir      string   `toml:"dir"`
	Copy     []string `toml:"copy"`
	Template []string `toml:"template"`
}

type RouterConfig struct {
	Port        int    `toml:"port"`
	AdminPort   int    `toml:"admin_port"`
	BaseDomain  string `toml:"base_domain"`
	Bind        string `toml:"bind"`
	Image       string `toml:"image"`
	SelfAliases bool   `toml:"self_aliases"`
}

type ServiceConfig struct {
	Name              string   `toml:"name"`
	Port              int      `toml:"port"`
	Default           bool     `toml:"default"`
	Headless          *bool    `toml:"headless"`
	Paths             []string `toml:"paths"`
	TCP               bool     `toml:"tcp"`
	WebsocketPath     string   `toml:"websocket_path"`
	WebsocketProtocol string   `toml:"websocket_protocol"`
	RewriteHost       string   `toml:"rewrite_host"`
}

func (s ServiceConfig) InHeadless() bool {
	return s.Headless == nil || *s.Headless
}

func (s ServiceConfig) Routable() bool { return !s.TCP && s.Port > 0 }

type StatefulConfig struct {
	Service   string        `toml:"service"`
	Volume    string        `toml:"volume"`
	Inputs    []string      `toml:"inputs"`
	StopGrace Duration      `toml:"stop_grace"`
	Prepare   PrepareConfig `toml:"prepare"`
}

type PrepareMode string

const (
	PrepareRun  PrepareMode = "run"
	PrepareExec PrepareMode = "exec"
	PrepareHost PrepareMode = "host"
)

type PrepareConfig struct {
	Mode    PrepareMode `toml:"mode"`
	Service string      `toml:"service"`
	Command []string    `toml:"command"`
	Timeout Duration    `toml:"timeout"`
}

func (p PrepareConfig) Defined() bool { return len(p.Command) > 0 }

type CacheConfig struct {
	Name     string   `toml:"name"`
	Path     string   `toml:"path"`
	Services []string `toml:"services"`
}

type Limit struct {
	Memory Size    `toml:"mem"`
	CPUs   float64 `toml:"cpus"`
}

type HooksConfig struct {
	PostCreate []string `toml:"post_create"`
	PostUp     []string `toml:"post_up"`
	PreDown    []string `toml:"pre_down"`
}

func (h HooksConfig) All() []string {
	out := make([]string, 0, len(h.PostCreate)+len(h.PostUp)+len(h.PreDown))
	out = append(out, h.PostCreate...)
	out = append(out, h.PostUp...)
	out = append(out, h.PreDown...)
	return out
}

type StateConfig struct {
	Driver               string   `toml:"driver"`
	CacheBranchSnapshots *bool    `toml:"cache_branch_snapshots"`
	GCAfter              Duration `toml:"gc_after"`
	KeepGoldens          int      `toml:"keep_goldens"`
	HelperImage          string   `toml:"helper_image"`
	WarnSize             Size     `toml:"warn_size"`
}

func (s StateConfig) CacheBranch() bool {
	return s.CacheBranchSnapshots == nil || *s.CacheBranchSnapshots
}

type SleepConfig struct {
	Enabled      *bool    `toml:"enabled"`
	PauseAfter   Duration `toml:"pause_after"`
	StopAfter    Duration `toml:"stop_after"`
	MemoryBudget Size     `toml:"memory_budget"`
	CPUFloor     float64  `toml:"cpu_floor"`
	Interval     Duration `toml:"interval"`
}

func (s SleepConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

type AgentConfig struct {
	Command []string `toml:"command"`
}

type UpConfig struct {
	Timeout Duration `toml:"timeout"`
	Build   *bool    `toml:"build"`
}

func (u UpConfig) ShouldBuild() bool { return u.Build == nil || *u.Build }

type DoctorConfig struct {
	HTTP []DoctorHTTP `toml:"http"`
}

type DoctorHTTP struct {
	Service            string `toml:"service"`
	Path               string `toml:"path"`
	ExpectStatus       int    `toml:"expect_status"`
	ExpectBodyContains string `toml:"expect_body_contains"`
}

func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(abs, FileName)
		st, err := os.Stat(candidate)
		if err == nil && !st.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("%w at or above %s", ErrNotFound, dir)
		}
		abs = parent
	}
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(b, path)
}

func Parse(b []byte, path string) (*Config, error) {
	var c Config
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, decodeError(b, path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	c.Path = abs
	c.Root = filepath.Dir(abs)
	c.raw = b
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) ServiceByName(name string) (ServiceConfig, bool) {
	for _, s := range c.Service {
		if s.Name == name {
			return s, true
		}
	}
	return ServiceConfig{}, false
}

func (c *Config) DefaultService() (ServiceConfig, bool) {
	for _, s := range c.Service {
		if s.Default && s.Routable() {
			return s, true
		}
	}
	for _, s := range c.Service {
		if s.Routable() {
			return s, true
		}
	}
	return ServiceConfig{}, false
}

func (c *Config) RoutableServices() []ServiceConfig {
	out := make([]ServiceConfig, 0, len(c.Service))
	for _, s := range c.Service {
		if s.Routable() {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) TCPServices() []ServiceConfig {
	out := make([]ServiceConfig, 0, len(c.Service))
	for _, s := range c.Service {
		if s.TCP && s.Port > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) StatefulByService(service string) (StatefulConfig, bool) {
	for _, s := range c.Stateful {
		if s.Service == service {
			return s, true
		}
	}
	return StatefulConfig{}, false
}

func (c *Config) CachesFor(service string) []CacheConfig {
	var out []CacheConfig
	for _, ch := range c.Cache {
		for _, s := range ch.Services {
			if s == service {
				out = append(out, ch)
				break
			}
		}
	}
	return out
}

func (c *Config) Raw() []byte { return c.raw }
