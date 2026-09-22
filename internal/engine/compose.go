package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrComposeMissing = errors.New("docker compose is not available")

type Compose struct {
	Bin        []string
	File       string
	ProjectDir string
	Project    string
	Env        []string
	Out        io.Writer
	Err        io.Writer
}

var (
	composeOnce sync.Once
	composeBin  []string
	composeErr  error
)

func ComposeCommand() ([]string, error) {
	composeOnce.Do(func() {
		if v := os.Getenv("GROVE_COMPOSE"); v != "" {
			composeBin = strings.Fields(v)
			return
		}
		if docker, err := exec.LookPath("docker"); err == nil {
			cmd := exec.Command(docker, "compose", "version", "--short")
			if err := cmd.Run(); err == nil {
				composeBin = []string{docker, "compose"}
				return
			}
		}
		if legacy, err := exec.LookPath("docker-compose"); err == nil {
			composeBin = []string{legacy}
			return
		}
		composeErr = fmt.Errorf("%w: install Docker Compose v2, or set GROVE_COMPOSE to the command that runs it", ErrComposeMissing)
	})
	return composeBin, composeErr
}

func ComposeVersion(ctx context.Context) (string, error) {
	bin, err := ComposeCommand()
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, bin[0], append(append([]string{}, bin[1:]...), "version", "--short")...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *Compose) args(extra ...string) []string {
	args := append([]string{}, c.Bin[1:]...)
	args = append(args, "--ansi", "never", "-f", c.File)
	if c.ProjectDir != "" {
		args = append(args, "--project-directory", c.ProjectDir)
	}
	if c.Project != "" {
		args = append(args, "-p", c.Project)
	}
	return append(args, extra...)
}

func (c *Compose) env() []string {
	cleared := map[string]bool{
		"COMPOSE_FILE": true, "COMPOSE_PATH_SEPARATOR": true, "COMPOSE_PROFILES": true,
		"COMPOSE_PROJECT_NAME": true, "COMPOSE_ENV_FILES": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && cleared[k] {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "COMPOSE_IGNORE_ORPHANS=false")
	return append(out, c.Env...)
}

func (c *Compose) command(ctx context.Context, extra ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin[0], c.args(extra...)...)
	cmd.Env = c.env()
	if c.ProjectDir != "" {
		cmd.Dir = c.ProjectDir
	}
	return cmd
}

func (c *Compose) Run(ctx context.Context, args ...string) error {
	cmd := c.command(ctx, args...)
	cmd.Stdout = c.Out
	cmd.Stderr = c.Err
	if err := cmd.Run(); err != nil {
		return composeError(args, err, "")
	}
	return nil
}

func (c *Compose) Capture(ctx context.Context, args ...string) (string, error) {
	cmd := c.command(ctx, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return out, composeError(args, err, out)
	}
	return out, nil
}

func (c *Compose) RunInteractive(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := c.command(ctx, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 1, composeError(args, err, "")
}

func (c *Compose) Up(ctx context.Context, build bool, services ...string) error {
	args := []string{"up", "-d", "--remove-orphans"}
	if build {
		args = append(args, "--build")
	}
	return c.Run(ctx, append(args, services...)...)
}

func (c *Compose) Stop(ctx context.Context, grace time.Duration, services ...string) error {
	args := []string{"stop"}
	if grace > 0 {
		args = append(args, "-t", strconv.Itoa(int(grace.Seconds())))
	}
	return c.Run(ctx, append(args, services...)...)
}

func (c *Compose) Start(ctx context.Context, services ...string) error {
	return c.Run(ctx, append([]string{"start"}, services...)...)
}

func (c *Compose) Pause(ctx context.Context, services ...string) error {
	return c.Run(ctx, append([]string{"pause"}, services...)...)
}

func (c *Compose) Unpause(ctx context.Context, services ...string) error {
	return c.Run(ctx, append([]string{"unpause"}, services...)...)
}

func (c *Compose) Restart(ctx context.Context, services ...string) error {
	return c.Run(ctx, append([]string{"restart"}, services...)...)
}

func (c *Compose) Down(ctx context.Context, volumes bool) error {
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	return c.Run(ctx, args...)
}

func (c *Compose) RunOnce(ctx context.Context, service string, cmdline []string) (int, string, error) {
	args := append([]string{"run", "--rm", "-T", service}, cmdline...)
	cmd := c.command(ctx, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err == nil {
		return 0, out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out, nil
	}
	return 1, out, composeError(args, err, out)
}

func (c *Compose) ExecOnce(ctx context.Context, service string, cmdline []string) (int, string, error) {
	args := append([]string{"exec", "-T", service}, cmdline...)
	cmd := c.command(ctx, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err == nil {
		return 0, out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out, nil
	}
	return 1, out, composeError(args, err, out)
}

func (c *Compose) Config(ctx context.Context) (string, error) {
	return c.Capture(ctx, "config")
}

func composeError(args []string, err error, output string) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if msg != "" {
			return fmt.Errorf("docker compose %s failed (exit %d):\n%s", args[0], ee.ExitCode(), LastLines(msg, 40))
		}
		return fmt.Errorf("docker compose %s failed (exit %d)", args[0], ee.ExitCode())
	}
	return fmt.Errorf("docker compose %s: %w", args[0], err)
}
