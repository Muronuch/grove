package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/state"
)

const HoldFor = 10 * time.Minute

const maxOutput = 64 << 10

type Server struct {
	ctl    *envctl.Controller
	target *envctl.Target
	dir    string
}

func New(ctx context.Context, ctl *envctl.Controller, dir string) (*Server, error) {
	s := &Server{ctl: ctl, dir: dir}
	t, err := ctl.Resolve(ctx, dir, "")
	switch {
	case err == nil:
		s.target = t
	case errors.Is(err, envctl.ErrNotInEnv):

	default:
		return nil, err
	}
	return s, nil
}

func (s *Server) Serve(ctx context.Context) error {
	impl := &sdk.Implementation{
		Name:    meta.Name,
		Title:   meta.Name + " environment",
		Version: meta.VersionString(),
		Description: "Tools for the one isolated development environment this " +
			"working directory belongs to.",
	}
	srv := sdk.NewServer(impl, &sdk.ServerOptions{
		Instructions: s.instructions(),
	})
	s.register(srv)
	return srv.Run(ctx, &sdk.StdioTransport{})
}

func (s *Server) instructions() string {
	if s.target == nil {
		return fmt.Sprintf(
			"This directory is not inside a %s-managed worktree, so only env_list is available. "+
				"Create an environment with `%s new <branch>`.", meta.Name, meta.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are working inside the %s environment %q (slot %d, branch %s).\n\n",
		meta.Name, s.target.Entry.Slug, s.target.Entry.Slot, s.target.Entry.Branch)
	b.WriteString("Every tool here acts on that environment and no other.\n\n")
	for svc, url := range s.target.Hosts().URLs() {
		fmt.Fprintf(&b, "  %s: %s\n", svc, url)
	}
	b.WriteString("\nNever publish host ports: the environment is reached through those URLs.\n")
	if len(s.target.Config.Stateful) > 0 {
		b.WriteString("After writing a migration, call db_prepare. " +
			"To start over from the shared snapshot, call db_reset.\n")
	}
	return b.String()
}

func (s *Server) register(srv *sdk.Server) {
	if s.target == nil {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "env_list",
			Description: "List the environments " + meta.Name + " manages. Read-only.",
		}, s.envList)
		return
	}

	sdk.AddTool(srv, &sdk.Tool{
		Name: "env_status",
		Description: "The state of this environment and each of its services, " +
			"with URLs and snapshot provenance.",
	}, s.envStatus)

	sdk.AddTool(srv, &sdk.Tool{
		Name: "env_urls",
		Description: "The URL of each service in this environment, plus any " +
			"loopback TCP addresses for database clients.",
	}, s.envURLs)

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "env_logs",
		Description: "Recent log lines from one service of this environment.",
	}, s.envLogs)

	sdk.AddTool(srv, &sdk.Tool{
		Name: "env_exec",
		Description: "Run a command inside one service's container of this " +
			"environment and return its exit code and output. The command runs " +
			"in the container, never on the host.",
	}, s.envExec)

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "env_restart",
		Description: "Restart this environment, or named services of it.",
	}, s.envRestart)

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "env_wait_healthy",
		Description: "Wait until every service of this environment is healthy.",
	}, s.envWaitHealthy)

	if len(s.target.Config.Stateful) == 0 {
		return
	}
	sdk.AddTool(srv, &sdk.Tool{
		Name: "db_prepare",
		Description: "Apply any migrations that are not applied yet. Call this " +
			"after writing a migration; it is the cheap operation.",
	}, s.dbPrepare)

	sdk.AddTool(srv, &sdk.Tool{
		Name: "db_reset",
		Description: "Discard this environment's database and rebuild it from " +
			"the shared snapshot plus this branch's own migrations.",
	}, s.dbReset)

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "db_checkpoint",
		Description: "Save, restore or list named checkpoints of this environment's database.",
	}, s.dbCheckpoint)
}

func (s *Server) touch(ctx context.Context) {
	if s.target == nil {
		return
	}
	_ = s.ctl.TouchActivity(ctx, s.target, HoldFor)
}

type Empty struct{}

type ListResult struct {
	InEnv bool               `json:"in_env"`
	Note  string             `json:"note"`
	Envs  []envctl.EnvStatus `json:"envs"`
}

func (s *Server) envList(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, ListResult, error) {
	report, err := s.ctl.List(ctx, s.dir, envctl.StatusOptions{})
	if err != nil {
		return nil, ListResult{}, err
	}
	return nil, ListResult{
		InEnv: false,
		Note: fmt.Sprintf("This directory is not inside a %s worktree, so only this listing is available.",
			meta.Name),
		Envs: report.Envs,
	}, nil
}

func (s *Server) envStatus(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, envctl.EnvStatus, error) {
	s.touch(ctx)
	st, err := s.ctl.Status(ctx, s.target, envctl.StatusOptions{Stats: true})
	if err != nil {
		return nil, envctl.EnvStatus{}, err
	}
	return nil, st, nil
}

type URLsResult struct {
	Env      string            `json:"env"`
	Slot     int               `json:"slot"`
	URLs     map[string]string `json:"urls"`
	SlotURLs map[string]string `json:"slot_urls"`
	TCP      map[string]string `json:"tcp,omitempty"`
}

func (s *Server) envURLs(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, URLsResult, error) {
	s.touch(ctx)
	hosts := s.target.Hosts()
	slotURLs := map[string]string{}
	for _, svc := range s.target.Config.RoutableServices() {
		slotURLs[svc.Name] = hosts.SlotURL(svc.Name)
	}
	return nil, URLsResult{
		Env:      s.target.Entry.Slug,
		Slot:     s.target.Entry.Slot,
		URLs:     hosts.URLs(),
		SlotURLs: slotURLs,
		TCP:      s.target.Entry.TCP,
	}, nil
}

type LogsArgs struct {
	Service string `json:"service" jsonschema:"the compose service to read logs from"`
	Tail    int    `json:"tail,omitempty" jsonschema:"how many trailing lines to return (default 100, maximum 500)"`
	Since   string `json:"since,omitempty" jsonschema:"only lines after this time, e.g. 10m or an RFC3339 timestamp"`
}

type LogsResult struct {
	Service string `json:"service"`
	Lines   string `json:"lines"`
}

func (s *Server) envLogs(ctx context.Context, _ *sdk.CallToolRequest, in LogsArgs) (*sdk.CallToolResult, LogsResult, error) {
	s.touch(ctx)
	if in.Service == "" {
		return nil, LogsResult{}, fmt.Errorf("service is required (services: %s)", strings.Join(s.serviceNames(), ", "))
	}
	tail := in.Tail
	if tail <= 0 {
		tail = 100
	}
	if tail > 500 {
		tail = 500
	}
	var buf strings.Builder
	err := s.ctl.Logs(ctx, s.target, in.Service, engine.LogOptions{
		Tail: fmt.Sprint(tail), Since: in.Since,
	}, &buf)
	if err != nil {
		return nil, LogsResult{}, err
	}
	return nil, LogsResult{Service: in.Service, Lines: truncate(buf.String())}, nil
}

type ExecArgs struct {
	Service string   `json:"service" jsonschema:"the compose service whose container runs the command"`
	Command []string `json:"command" jsonschema:"the command and its arguments, e.g. [\"go\",\"test\",\"./...\"]"`
	Timeout string   `json:"timeout,omitempty" jsonschema:"how long to allow, e.g. 5m (default 10m)"`
}

type ExecResult struct {
	Service   string `json:"service"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *Server) envExec(ctx context.Context, _ *sdk.CallToolRequest, in ExecArgs) (*sdk.CallToolResult, ExecResult, error) {
	s.touch(ctx)
	if in.Service == "" {
		return nil, ExecResult{}, fmt.Errorf("service is required (services: %s)", strings.Join(s.serviceNames(), ", "))
	}
	if len(in.Command) == 0 {
		return nil, ExecResult{}, errors.New("command is required")
	}
	timeout := 10 * time.Minute
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil {
			return nil, ExecResult{}, fmt.Errorf("timeout: %w", err)
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr strings.Builder
	code, err := s.ctl.Exec(ctx, s.target, in.Service, in.Command, nil, &stdout, &stderr, false)
	if err != nil {
		return nil, ExecResult{}, err
	}
	out, cut1 := truncated(stdout.String())
	errOut, cut2 := truncated(stderr.String())
	return nil, ExecResult{
		Service: in.Service, ExitCode: code,
		Stdout: out, Stderr: errOut, Truncated: cut1 || cut2,
	}, nil
}

type RestartArgs struct {
	Services []string `json:"services,omitempty" jsonschema:"services to restart; empty restarts the whole environment"`
}

func (s *Server) envRestart(ctx context.Context, _ *sdk.CallToolRequest, in RestartArgs) (*sdk.CallToolResult, envctl.EnvStatus, error) {
	s.touch(ctx)
	if err := s.ctl.Restart(ctx, s.target, in.Services); err != nil {
		return nil, envctl.EnvStatus{}, err
	}
	return s.envStatus(ctx, nil, Empty{})
}

type WaitArgs struct {
	Timeout string `json:"timeout,omitempty" jsonschema:"how long to wait, e.g. 2m (default: the project's up.timeout)"`
}

func (s *Server) envWaitHealthy(ctx context.Context, _ *sdk.CallToolRequest, in WaitArgs) (*sdk.CallToolResult, envctl.EnvStatus, error) {
	s.touch(ctx)
	timeout := s.target.Config.Up.Timeout.Duration()
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil {
			return nil, envctl.EnvStatus{}, fmt.Errorf("timeout: %w", err)
		}
		timeout = d
	}
	stack, err := s.ctl.Prepare(ctx, s.target, envctl.PrepareOptions{Headless: s.target.Entry.Headless})
	if err != nil {
		return nil, envctl.EnvStatus{}, err
	}
	if err := s.ctl.WaitHealthy(ctx, s.target, stack, timeout); err != nil {
		return nil, envctl.EnvStatus{}, err
	}
	return s.envStatus(ctx, nil, Empty{})
}

type PrepareResult struct {
	Results []envctl.PrepareResult `json:"results"`
	OK      bool                   `json:"ok"`
}

func (s *Server) dbPrepare(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, PrepareResult, error) {
	s.touch(ctx)
	results, err := s.ctl.RunPrepare(ctx, s.target)
	if err != nil {
		return nil, PrepareResult{}, err
	}
	out := PrepareResult{OK: true}
	for _, r := range results {
		r.Output = truncate(r.Output)
		out.Results = append(out.Results, r)
		if r.ExitCode != 0 {
			out.OK = false
		}
	}
	return nil, out, nil
}

type ResetResult struct {
	Env       string            `json:"env"`
	Snapshots map[string]string `json:"snapshots"`
	Status    envctl.EnvStatus  `json:"status"`
}

func (s *Server) dbReset(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, ResetResult, error) {
	s.touch(ctx)
	if err := s.ctl.ResetState(ctx, s.target, envctl.ResetOptions{}); err != nil {
		return nil, ResetResult{}, err
	}
	st, err := s.ctl.Status(ctx, s.target, envctl.StatusOptions{})
	if err != nil {
		return nil, ResetResult{}, err
	}
	sources := map[string]string{}
	for svc, ref := range st.Snapshots {
		sources[svc] = ref.Source
	}
	return nil, ResetResult{Env: s.target.Entry.Slug, Snapshots: sources, Status: st}, nil
}

type CheckpointArgs struct {
	Action string `json:"action" jsonschema:"one of save, restore, list"`
	Name   string `json:"name,omitempty" jsonschema:"the checkpoint name, required for save and restore"`
}

type CheckpointResult struct {
	Action      string        `json:"action"`
	Name        string        `json:"name,omitempty"`
	Checkpoints []state.Entry `json:"checkpoints,omitempty"`
}

func (s *Server) dbCheckpoint(ctx context.Context, _ *sdk.CallToolRequest, in CheckpointArgs) (*sdk.CallToolResult, CheckpointResult, error) {
	s.touch(ctx)
	switch in.Action {
	case "list", "":
		entries, err := s.ctl.Checkpoints(ctx, s.target)
		if err != nil {
			return nil, CheckpointResult{}, err
		}
		return nil, CheckpointResult{Action: "list", Checkpoints: entries}, nil
	case "save":
		if in.Name == "" {
			return nil, CheckpointResult{}, errors.New("name is required to save a checkpoint")
		}
		if err := s.ctl.SaveCheckpoint(ctx, s.target, in.Name); err != nil {
			return nil, CheckpointResult{}, err
		}
		return nil, CheckpointResult{Action: "save", Name: in.Name}, nil
	case "restore":
		if in.Name == "" {
			return nil, CheckpointResult{}, errors.New("name is required to restore a checkpoint")
		}
		if err := s.ctl.ResetState(ctx, s.target, envctl.ResetOptions{From: in.Name}); err != nil {
			return nil, CheckpointResult{}, err
		}
		return nil, CheckpointResult{Action: "restore", Name: in.Name}, nil
	default:
		return nil, CheckpointResult{}, fmt.Errorf("action must be save, restore or list, got %q", in.Action)
	}
}

func (s *Server) serviceNames() []string {
	out := make([]string, 0, len(s.target.Config.Service))
	for _, svc := range s.target.Config.Service {
		out = append(out, svc.Name)
	}
	return out
}

func truncate(s string) string {
	out, _ := truncated(s)
	return out
}

func truncated(s string) (string, bool) {
	if len(s) <= maxOutput {
		return s, false
	}
	cut := s[len(s)-maxOutput:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < 200 {
		cut = cut[i+1:]
	}
	return fmt.Sprintf("… %d bytes omitted; showing the last %d …\n%s",
		len(s)-len(cut), len(cut), cut), true
}
