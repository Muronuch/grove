package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Muronuch/grove/internal/meta"
)

var NameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)

var serviceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

const (
	DefaultMaxSlots      = 8
	DefaultBaseDomain    = "localhost"
	DefaultWorktreeDir   = "worktrees"
	DefaultDriver        = "volume-copy"
	DefaultKeepGoldens   = 3
	DefaultHelperImage   = "busybox:1.37.0"
	DefaultAgentCommand  = "claude"
	DefaultUpTimeout     = 5 * time.Minute
	DefaultPrepareTimout = 10 * time.Minute
	DefaultStopGrace     = 60 * time.Second
	DefaultGCAfter       = 14 * 24 * time.Hour
	DefaultPauseAfter    = 15 * time.Minute
	DefaultStopAfter     = 2 * time.Hour
	DefaultSchedInterval = 30 * time.Second
	DefaultCPUFloor      = 5.0
	DefaultWarnSizeBytes = 5 << 30
)

func (c *Config) applyDefaults() {
	if c.Project.DefaultBranch == "" {
		c.Project.DefaultBranch = "main"
	}
	if c.Project.MaxSlots == 0 {
		c.Project.MaxSlots = DefaultMaxSlots
	}
	if c.Worktree.Dir == "" {
		c.Worktree.Dir = DefaultWorktreeDir
	}
	if c.Router.Port == 0 {
		c.Router.Port = meta.DefaultRouterPort
	}
	if c.Router.AdminPort == 0 {
		c.Router.AdminPort = meta.DefaultRouterAdminPort
	}
	if c.Router.BaseDomain == "" {
		c.Router.BaseDomain = DefaultBaseDomain
	}
	if c.Router.Bind == "" {
		c.Router.Bind = "127.0.0.1"
	}
	if c.State.Driver == "" {
		c.State.Driver = DefaultDriver
	}
	if c.State.KeepGoldens == 0 {
		c.State.KeepGoldens = DefaultKeepGoldens
	}
	if c.State.GCAfter == 0 {
		c.State.GCAfter = Duration(DefaultGCAfter)
	}
	if c.State.HelperImage == "" {
		c.State.HelperImage = DefaultHelperImage
	}
	if c.State.WarnSize.IsZero() {
		c.State.WarnSize = Size{Bytes: DefaultWarnSizeBytes}
	}
	if c.Sleep.PauseAfter == 0 {
		c.Sleep.PauseAfter = Duration(DefaultPauseAfter)
	}
	if c.Sleep.StopAfter == 0 {
		c.Sleep.StopAfter = Duration(DefaultStopAfter)
	}
	if c.Sleep.MemoryBudget.IsZero() {
		c.Sleep.MemoryBudget = Size{Percent: 50}
	}
	if c.Sleep.CPUFloor == 0 {
		c.Sleep.CPUFloor = DefaultCPUFloor
	}
	if c.Sleep.Interval == 0 {
		c.Sleep.Interval = Duration(DefaultSchedInterval)
	}
	if len(c.Agent.Command) == 0 {
		c.Agent.Command = []string{DefaultAgentCommand}
	}
	if c.Up.Timeout == 0 {
		c.Up.Timeout = Duration(DefaultUpTimeout)
	}
	for i := range c.Stateful {
		if c.Stateful[i].StopGrace == 0 {
			c.Stateful[i].StopGrace = Duration(DefaultStopGrace)
		}
		if c.Stateful[i].Prepare.Timeout == 0 {
			c.Stateful[i].Prepare.Timeout = Duration(DefaultPrepareTimout)
		}
		if c.Stateful[i].Prepare.Mode == "" && c.Stateful[i].Prepare.Defined() {
			c.Stateful[i].Prepare.Mode = PrepareRun
		}
	}
}

type Error struct {
	Key     string
	Line    int
	Message string
	Path    string
}

func (e *Error) Error() string {
	loc := e.Path
	if loc == "" {
		loc = FileName
	}
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
	}
	if e.Key != "" {
		return fmt.Sprintf("%s: %s: %s", loc, e.Key, e.Message)
	}
	return fmt.Sprintf("%s: %s", loc, e.Message)
}

type Errors []*Error

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems in %s:", len(e), FileName)
	for _, err := range e {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

type validator struct {
	c    *Config
	errs Errors
}

func (v *validator) add(key, format string, args ...any) {
	v.errs = append(v.errs, &Error{
		Key:     key,
		Line:    findKeyLine(v.c.raw, key),
		Message: fmt.Sprintf(format, args...),
		Path:    v.c.Path,
	})
}

func (c *Config) Validate() error {
	v := &validator{c: c}

	if c.Project.Name == "" {
		v.add("project.name", "required: the project name appears in hostnames and Docker resource names")
	} else if !NameRE.MatchString(c.Project.Name) {
		v.add("project.name", "%q is not a valid name (want %s)", c.Project.Name, NameRE)
	}
	if c.Project.MaxSlots < 1 || c.Project.MaxSlots > 999 {
		v.add("project.max_slots", "must be between 1 and 999, got %d", c.Project.MaxSlots)
	}

	if c.Router.Port < 1 || c.Router.Port > 65535 {
		v.add("router.port", "must be a TCP port, got %d", c.Router.Port)
	}
	if c.Router.AdminPort < 1 || c.Router.AdminPort > 65535 {
		v.add("router.admin_port", "must be a TCP port, got %d", c.Router.AdminPort)
	}
	if c.Router.AdminPort == c.Router.Port {
		v.add("router.admin_port", "must differ from router.port (%d)", c.Router.Port)
	}
	if err := validateDomain(c.Router.BaseDomain); err != nil {
		v.add("router.base_domain", "%v", err)
	}

	v.validateServices()
	v.validateEnv()
	v.validateStateful()
	v.validateCaches()
	v.validateLimits()
	v.validateState()
	v.validateDoctor()

	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

func (v *validator) validateServices() {
	c := v.c
	seen := map[string]int{}
	defaults := 0
	for i, s := range c.Service {
		key := fmt.Sprintf("service[%d]", i)
		if s.Name == "" {
			v.add(key+".name", "required: must match a compose service name")
			continue
		}
		if !serviceNameRE.MatchString(s.Name) {
			v.add(key+".name", "%q is not a valid compose service name", s.Name)
		}
		if prev, dup := seen[s.Name]; dup {
			v.add(key+".name", "duplicate service %q (already declared at service[%d])", s.Name, prev)
		}
		seen[s.Name] = i
		if s.Port < 1 || s.Port > 65535 {
			v.add(key+".port", "service %q needs the container port it listens on, got %d", s.Name, s.Port)
		}
		if s.Default && s.TCP {
			v.add(key+".default", "service %q cannot be both default (HTTP) and tcp", s.Name)
		}
		if s.Default {
			defaults++
		}
		for j, p := range s.Paths {
			if !strings.HasPrefix(p, "/") {
				v.add(fmt.Sprintf("%s.paths[%d]", key, j), "path prefix %q must start with /", p)
			}
			if s.TCP {
				v.add(key+".paths", "service %q is tcp and cannot serve HTTP path prefixes", s.Name)
				break
			}
		}
		if s.WebsocketPath != "" && !strings.HasPrefix(s.WebsocketPath, "/") {
			v.add(key+".websocket_path", "%q must start with /", s.WebsocketPath)
		}
	}
	if defaults > 1 {
		v.add("service.default", "%d services are marked default = true; exactly one host gets <env>.<project>.<domain>", defaults)
	}
	if len(c.Service) > 0 && defaults == 0 {
		if _, ok := c.DefaultService(); !ok {
			v.add("service", "no routable service: mark one HTTP service with default = true")
		}
	}
}

func (v *validator) validateEnv() {
	for svc, vars := range v.c.Env {
		if _, ok := v.c.ServiceByName(svc); !ok && !serviceNameRE.MatchString(svc) {
			v.add("env."+svc, "%q is not a valid compose service name", svc)
		}
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" {
				v.add("env."+svc, "empty environment variable name")
				continue
			}
			if _, err := v.c.compileTemplate("env."+svc+"."+k, vars[k]); err != nil {
				v.add("env."+svc+"."+k, "%v", err)
			}
		}
	}
}

func (v *validator) validateStateful() {
	seen := map[string]int{}
	for i, s := range v.c.Stateful {
		key := fmt.Sprintf("stateful[%d]", i)
		if s.Service == "" {
			v.add(key+".service", "required: the compose service whose data volume grove manages")
			continue
		}
		if prev, dup := seen[s.Service]; dup {
			v.add(key+".service", "duplicate stateful service %q (already declared at stateful[%d])", s.Service, prev)
		}
		seen[s.Service] = i
		if s.Volume == "" {
			v.add(key+".volume", "required: the top-level named volume holding %s's data", s.Service)
		}

		for j, in := range s.Inputs {
			if strings.HasPrefix(in, "/") || strings.Contains(in, "..") {
				v.add(fmt.Sprintf("%s.inputs[%d]", key, j), "%q must be a relative path inside the repository", in)
			}
			if _, err := path.Match(strings.ReplaceAll(in, "**", "*"), "x"); err != nil {
				v.add(fmt.Sprintf("%s.inputs[%d]", key, j), "invalid glob %q: %v", in, err)
			}
		}
		if !s.Prepare.Defined() {
			continue
		}
		switch s.Prepare.Mode {
		case PrepareRun, PrepareExec:
			if s.Prepare.Service == "" {
				v.add(key+".prepare.service", "required for mode = %q: the compose service that runs the command", s.Prepare.Mode)
			}
		case PrepareHost:
		default:
			v.add(key+".prepare.mode", "%q is not one of run, exec, host", s.Prepare.Mode)
		}
	}
}

func (v *validator) validateCaches() {
	seen := map[string]int{}
	for i, ch := range v.c.Cache {
		key := fmt.Sprintf("cache[%d]", i)
		if ch.Name == "" {
			v.add(key+".name", "required")
		} else if !NameRE.MatchString(ch.Name) {
			v.add(key+".name", "%q is not a valid name (want %s)", ch.Name, NameRE)
		}
		if prev, dup := seen[ch.Name]; dup {
			v.add(key+".name", "duplicate cache %q (already declared at cache[%d])", ch.Name, prev)
		}
		seen[ch.Name] = i
		if ch.Path == "" || !strings.HasPrefix(ch.Path, "/") {
			v.add(key+".path", "must be an absolute path inside the container, got %q", ch.Path)
		}
		if len(ch.Services) == 0 {
			v.add(key+".services", "required: which services mount this cache")
		}
	}
}

func (v *validator) validateLimits() {
	for svc, l := range v.c.Limits {
		if !serviceNameRE.MatchString(svc) {
			v.add("limits."+svc, "%q is not a valid compose service name", svc)
		}
		if l.CPUs < 0 {
			v.add("limits."+svc+".cpus", "must not be negative, got %v", l.CPUs)
		}
		if l.Memory.Percent > 0 {
			v.add("limits."+svc+".mem", "a percentage is not meaningful for a container limit; use an absolute size like \"512m\"")
		}
	}
}

func (v *validator) validateState() {
	switch v.c.State.Driver {
	case "volume-copy":
	case "pg-template", "reflink", "external":
		v.add("state.driver", "driver %q is planned but not implemented in this version; use \"volume-copy\"", v.c.State.Driver)
	default:
		v.add("state.driver", "unknown driver %q (available: volume-copy)", v.c.State.Driver)
	}
	if v.c.State.KeepGoldens < 1 {
		v.add("state.keep_goldens", "must keep at least one golden per service, got %d", v.c.State.KeepGoldens)
	}
	if v.c.Sleep.StopAfter > 0 && v.c.Sleep.PauseAfter > v.c.Sleep.StopAfter {
		v.add("sleep.pause_after", "must not exceed sleep.stop_after (%s > %s)", v.c.Sleep.PauseAfter, v.c.Sleep.StopAfter)
	}
}

func (v *validator) validateDoctor() {
	for i, h := range v.c.Doctor.HTTP {
		key := fmt.Sprintf("doctor.http[%d]", i)
		if h.Service == "" {
			v.add(key+".service", "required")
		} else if s, ok := v.c.ServiceByName(h.Service); !ok {
			v.add(key+".service", "%q is not declared as a [[service]]", h.Service)
		} else if !s.Routable() {
			v.add(key+".service", "%q is not routable over HTTP", h.Service)
		}
		if !strings.HasPrefix(h.Path, "/") {
			v.add(key+".path", "must start with /, got %q", h.Path)
		}
		if h.ExpectStatus != 0 && (h.ExpectStatus < 100 || h.ExpectStatus > 599) {
			v.add(key+".expect_status", "%d is not an HTTP status code", h.ExpectStatus)
		}
		if h.ExpectBodyContains != "" {
			if _, err := v.c.compileTemplate(key+".expect_body_contains", h.ExpectBodyContains); err != nil {
				v.add(key+".expect_body_contains", "%v", err)
			}
		}
	}
}

func validateDomain(d string) error {
	if d == "" {
		return errors.New("must not be empty")
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" {
			return fmt.Errorf("%q has an empty label", d)
		}
		if len(label) > 63 {
			return fmt.Errorf("label %q is longer than 63 characters", label)
		}
		for _, r := range label {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				return fmt.Errorf("%q must be lowercase letters, digits, dots and dashes", d)
			}
		}
	}
	return nil
}

func decodeError(raw []byte, path string, err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, _ := de.Position()
		return &Error{Key: strings.Join(de.Key(), "."), Line: row, Message: de.Error(), Path: path}
	}
	var se *toml.StrictMissingError
	if errors.As(err, &se) {
		errsOut := make(Errors, 0, len(se.Errors))
		for i := range se.Errors {
			row, _ := se.Errors[i].Position()
			key := strings.Join(se.Errors[i].Key(), ".")
			errsOut = append(errsOut, &Error{
				Key:     key,
				Line:    row,
				Message: "unknown key; check the reference in docs/configuration.md for a typo",
				Path:    path,
			})
		}
		return errsOut
	}
	return &Error{Message: err.Error(), Path: path}
}

func findKeyLine(raw []byte, key string) int {
	if len(raw) == 0 || key == "" {
		return 0
	}
	segs := strings.Split(key, ".")
	leaf := segs[len(segs)-1]

	wantIndex := 0
	tablePath := make([]string, 0, len(segs))
	for _, s := range segs[:len(segs)-1] {
		if i := strings.IndexByte(s, '['); i >= 0 && strings.HasSuffix(s, "]") {
			n, err := strconv.Atoi(s[i+1 : len(s)-1])
			if err == nil {
				wantIndex = n
			}
			s = s[:i]
		}
		tablePath = append(tablePath, s)
	}
	if i := strings.IndexByte(leaf, '['); i >= 0 {
		leaf = leaf[:i]
	}

	wantTable := strings.Join(tablePath, ".")
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	curTable := ""
	seenIndex := map[string]int{}
	tableLine, leafLine := 0, 0
	line := 0
	inTable := false
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[") {
			name := strings.Trim(t, "[] \t")
			if idx := strings.IndexAny(name, "#"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
			isArray := strings.HasPrefix(t, "[[")
			curTable = name
			n := seenIndex[name]
			if isArray {
				seenIndex[name] = n + 1
			}
			inTable = name == wantTable && (!isArray || n == wantIndex)

			if !inTable && strings.HasPrefix(wantTable, name+".") {
				inTable = false
			}
			if inTable && tableLine == 0 {
				tableLine = line
			}
			continue
		}
		if !inTable && wantTable != "" {
			continue
		}
		if wantTable == "" && curTable != "" {
			continue
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			continue
		}
		name := strings.TrimSpace(t[:eq])
		name = strings.Trim(name, "\"'")
		if name == leaf && leafLine == 0 {
			leafLine = line
			if wantTable == "" || inTable {
				return line
			}
		}
	}
	if leafLine > 0 {
		return leafLine
	}
	return tableLine
}
