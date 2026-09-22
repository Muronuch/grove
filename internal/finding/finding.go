package finding

import (
	"fmt"
	"sort"
	"strings"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
	Info    Severity = "info"
)

type Code string

const (
	CodeHostNetwork      Code = "E_HOST_NETWORK"
	CodeExternalNetwork  Code = "E_EXTERNAL_NETWORK"
	CodeAbsoluteBind     Code = "W_ABSOLUTE_BIND"
	CodeContainerName    Code = "W_CONTAINER_NAME"
	CodeHardcodedURL     Code = "W_HARDCODED_URL"
	CodeServiceMissing   Code = "E_SERVICE_MISSING"
	CodeVolumeMissing    Code = "E_VOLUME_MISSING"
	CodePrepareMissing   Code = "E_PREPARE_MISSING"
	CodeStatefulInputs   Code = "E_STATEFUL_INPUTS"
	CodeScale            Code = "W_SCALE"
	CodeSharedVolume     Code = "W_SHARED_VOLUME"
	CodeVolumeIsolated   Code = "W_VOLUME_ISOLATED"
	CodeMigrationClash   Code = "W_MIGRATION_COLLISION"
	CodeConfigInvalid    Code = "E_CONFIG"
	CodeNoRoutable       Code = "E_NO_ROUTABLE_SERVICE"
	CodeDependsOnMissing Code = "E_DEPENDS_ON_MISSING"
	CodeWorktreeInBuild  Code = "W_WORKTREE_IN_BUILD_CONTEXT"
	CodeCopyTracked      Code = "W_COPY_TRACKED_FILE"
)

const (
	CodeDockerUnavailable Code = "E_DOCKER_UNAVAILABLE"
	CodeUnhealthy         Code = "E_UNHEALTHY"
	CodePortPublished     Code = "E_PORT_PUBLISHED"
	CodeStateShared       Code = "E_STATE_SHARED"
	CodeCrossEnvDNS       Code = "E_CROSS_ENV_DNS"
	CodeRouteFailed       Code = "E_ROUTE_FAILED"
	CodeWebsocket         Code = "E_WEBSOCKET"
	CodePrepareNotIdem    Code = "E_PREPARE_NOT_IDEMPOTENT"
	CodeHTTPCheck         Code = "E_HTTP_CHECK"
	CodeLocalhostResolve  Code = "W_LOCALHOST_RESOLUTION"
	CodeLargeSnapshot     Code = "W_LARGE_SNAPSHOT"
	CodeTiming            Code = "I_TIMING"
)

type Finding struct {
	Code     Code           `json:"code"`
	Severity Severity       `json:"severity"`
	Service  string         `json:"service,omitempty"`
	Env      string         `json:"env,omitempty"`
	Message  string         `json:"message"`
	Evidence map[string]any `json:"evidence,omitempty"`
	Hint     string         `json:"hint,omitempty"`
}

func (f Finding) Error() string { return string(f.Code) + ": " + f.Message }

func New(code Code, format string, args ...any) Finding {
	e := Lookup(code)
	return Finding{
		Code:     code,
		Severity: e.Severity,
		Message:  fmt.Sprintf(format, args...),
		Hint:     e.Hint,
	}
}

func (f Finding) WithService(s string) Finding { f.Service = s; return f }

func (f Finding) WithEnv(s string) Finding { f.Env = s; return f }

func (f Finding) WithHint(format string, args ...any) Finding {
	f.Hint = fmt.Sprintf(format, args...)
	return f
}

func (f Finding) WithEvidence(kv ...any) Finding {
	if len(kv)%2 != 0 {
		return f
	}
	if f.Evidence == nil {
		f.Evidence = map[string]any{}
	}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		f.Evidence[k] = kv[i+1]
	}
	return f
}

type List []Finding

func (l *List) Add(f ...Finding) { *l = append(*l, f...) }

func (l List) HasErrors() bool {
	for _, f := range l {
		if f.Severity == Error {
			return true
		}
	}
	return false
}

func (l List) Errors() List {
	var out List
	for _, f := range l {
		if f.Severity == Error {
			out = append(out, f)
		}
	}
	return out
}

func (l List) Codes() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range l {
		if !seen[string(f.Code)] {
			seen[string(f.Code)] = true
			out = append(out, string(f.Code))
		}
	}
	sort.Strings(out)
	return out
}

func (l List) Sorted() List {
	out := make(List, len(l))
	copy(out, l)
	rank := map[Severity]int{Error: 0, Warning: 1, Info: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Service < out[j].Service
	})
	return out
}

func (l List) Error() string {
	errs := l.Errors()
	if len(errs) == 0 {
		return "no errors"
	}
	var b strings.Builder
	for i, f := range errs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: %s", f.Code, f.Message)
		if f.Hint != "" {
			fmt.Fprintf(&b, "\n  fix: %s", f.Hint)
		}
	}
	return b.String()
}
