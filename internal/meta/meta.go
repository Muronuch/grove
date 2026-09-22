package meta

import (
	"fmt"
	"runtime/debug"
	"strings"
)

const Name = "grove"

var Version = ""

const LabelPrefix = Name + "."

const (
	LabelManaged = LabelPrefix + "managed"
	LabelProject = LabelPrefix + "project"
	LabelEnv     = LabelPrefix + "env"
	LabelService = LabelPrefix + "service"
	LabelSlot    = LabelPrefix + "slot"
	LabelRole    = LabelPrefix + "role"
	LabelKey     = LabelPrefix + "key"
	LabelCreated = LabelPrefix + "created"
)

const (
	ComposeLabelProject = "com.docker.compose.project"
	ComposeLabelVolume  = "com.docker.compose.volume"
	ComposeLabelVersion = "com.docker.compose.version"
)

const RouterContainer = Name + "-router"

const RouterDataVolume = Name + "-router-data"

const (
	RouterProxyPort = 80
	RouterAdminPort = 9180
)

const (
	DefaultRouterPort         = 80
	FallbackRouterPort        = 7080
	DefaultRouterAdminPort    = 9180
	FallbackRouterAdminPortHi = 9280
)

func UserAgent() string { return Name + "/" + VersionString() }

func VersionString() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return "dev+" + rev
		}
	}
	return "dev"
}

func ComposeProject(project, slug string) string {
	return fmt.Sprintf("%s-%s-%s", Name, project, slug)
}

func ComposeVolume(composeProject, volume string) string {
	return composeProject + "_" + volume
}

func ComposeNetwork(composeProject, network string) string {
	return composeProject + "_" + network
}

func ContainerName(composeProject, service string, index int) string {
	return fmt.Sprintf("%s-%s-%d", composeProject, service, index)
}

func SnapshotVolume(project, service, key string) string {
	return fmt.Sprintf("%s-snap-%s-%s-%s", Name, project, service, key)
}

func CacheVolume(project, name string) string {
	return fmt.Sprintf("%s-cache-%s-%s", Name, project, name)
}

func SnapshotKeyShort(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

func EnvVarName(parts ...string) string {
	out := make([]string, 0, len(parts)+1)
	out = append(out, strings.ToUpper(Name))
	for _, p := range parts {
		out = append(out, envIdent(p))
	}
	return strings.Join(out, "_")
}

func envIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func HeaderName(suffix string) string {
	return "X-" + strings.ToUpper(Name[:1]) + Name[1:] + "-" + suffix
}

func SharedVolume(project, volume string) string {
	return fmt.Sprintf("%s-shared-%s-%s", Name, project, volume)
}
