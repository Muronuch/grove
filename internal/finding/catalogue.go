package finding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Muronuch/grove/internal/meta"
)

type Entry struct {
	Code     Code
	Severity Severity
	Meaning  string
	Cause    string
	Hint     string
}

var catalogue = []Entry{
	{
		Code:     CodeHostNetwork,
		Severity: Error,
		Meaning:  "A service uses network_mode: host, which puts it directly on the host network.",
		Cause:    "A dev stack that was written for a single environment and wants to reach host processes.",
		Hint:     "Remove `network_mode: host` from the service and let it use the project network. If it needs to reach a process on the host, use `host.docker.internal` and add `extra_hosts: [\"host.docker.internal:host-gateway\"]`. Host networking cannot be isolated: every env would share the same ports.",
	},
	{
		Code:     CodeExternalNetwork,
		Severity: Error,
		Meaning:  "A service joins an external network with a fixed name, so all envs would share it.",
		Cause:    "A stack that is wired to another compose project, e.g. a shared monitoring or proxy network.",
		Hint:     "Either drop the external network from the dev compose file, or, if the envs really must share it, add the network's services to `compose.shared_volumes`' network equivalent by keeping the network external and accepting the sharing — then declare it in grove.toml under [compose] and re-run doctor. Traffic on a shared network crosses envs because compose registers each service name as a DNS alias on every network it joins.",
	},
	{
		Code:     CodeAbsoluteBind,
		Severity: Warning,
		Meaning:  "A bind mount points at an absolute host path outside the worktree, so every env shares it.",
		Cause:    "A mount such as /var/run/docker.sock, a shared data directory, or a path made absolute by an env var.",
		Hint:     "If the path holds state that must differ per env, move it under the repository so each worktree gets its own copy, or turn it into a named volume (grove isolates named volumes automatically). If sharing is intended, nothing to do — this warning records it.",
	},
	{
		Code:     CodeContainerName,
		Severity: Warning,
		Meaning:  "A service sets container_name, which is unique per engine and makes a second env impossible.",
		Cause:    "A compose file written before parallel environments were a requirement.",
		Hint:     "Delete the `container_name:` line from the service. grove strips it anyway, so anything that referred to the container by that fixed name (scripts, `docker exec`) must use the compose service name instead: `" + meta.Name + " exec <env> <service> -- ...`.",
	},
	{
		Code:     CodeCopyTracked,
		Severity: Warning,
		Meaning:  "A worktree.copy or worktree.template entry is a file git tracks, so every worktree starts dirty.",
		Cause:    "The entry names a committed file rather than a gitignored one such as .env.",
		Hint:     "These lists exist for files git does not have. Either gitignore the file, or, if it is meant to be committed, stop listing it — the worktree already gets the committed version. A tracked file that must differ per env is a sign the value belongs in [env.<service>] instead.",
	},
	{
		Code:     CodeWorktreeInBuild,
		Severity: Warning,
		Meaning:  "worktree.dir sits inside a service's build context, so every build would ship every env's worktree.",
		Cause:    "worktree.dir defaults to a directory inside the repository, and a service builds from the repository root.",
		Hint:     "Add the worktree directory to .dockerignore. " + meta.Name + " builds from the worktree, where the directory does not exist, so this only affects builds you run yourself — but those would send every env's checkout to the daemon. Moving worktree.dir outside the repository also fixes it.",
	},
	{
		Code:     CodeHardcodedURL,
		Severity: Warning,
		Meaning:  "A tracked file hardcodes a localhost URL with a published port.",
		Cause:    "A frontend or client that was written against one fixed dev stack.",
		Hint:     "Read the URL from an environment variable and set it in grove.toml, e.g. [env.web] VITE_API_URL = \"{{ url \\\"api\\\" }}\" — or serve the API under the same hostname by adding paths = [\"/api\"] to the api [[service]], which also removes the need for CORS.",
	},
	{
		Code:     CodeServiceMissing,
		Severity: Error,
		Meaning:  "grove.toml names a compose service that the compose file does not define.",
		Cause:    "A renamed service, or a service that only exists under a profile that is not enabled.",
		Hint:     "Fix the name in grove.toml, or add the profile that defines the service to `compose.profiles`.",
	},
	{
		Code:     CodeVolumeMissing,
		Severity: Error,
		Meaning:  "A [[stateful]] entry names a top-level volume the compose file does not declare.",
		Cause:    "The database writes to a bind mount or an anonymous volume instead of a named volume.",
		Hint:     "Declare a named volume in the compose file and mount it at the database's data directory, then set `volume` in the [[stateful]] block to that name. grove can only snapshot named volumes; a bind mount lives in the worktree and an anonymous volume cannot be cloned.",
	},
	{
		Code:     CodePrepareMissing,
		Severity: Error,
		Meaning:  "A stateful service has no prepare command, so grove cannot migrate or seed it.",
		Cause:    "`grove init` left the command as a TODO because it cannot be guessed.",
		Hint:     "Fill in [stateful.prepare] with the command that migrates and seeds the database, e.g. mode = \"run\", service = \"api\", command = [\"./bin/migrate\", \"up\", \"--seed\"]. It must be idempotent: running it twice must be a no-op.",
	},
	{
		Code:     CodeStatefulInputs,
		Severity: Error,
		Meaning:  "A [[stateful]] entry has no inputs, so grove cannot tell when its snapshot is stale.",
		Cause:    "`grove init` could not find a migrations directory to guess from.",
		Hint:     "Set `inputs` to the globs whose content decides the database's shape, e.g. inputs = [\"backend/migrations/**\", \"backend/seed/**\"]. grove hashes those files; a change to any of them builds a new golden snapshot.",
	},
	{
		Code:     CodeScale,
		Severity: Warning,
		Meaning:  "A routable service declares more than one replica; only replica 1 is routed.",
		Cause:    "`deploy.replicas` or `scale` greater than 1 in the compose file.",
		Hint:     "Set the replica count to 1 for dev environments, or accept that the router sends every request to replica 1.",
	},
	{
		Code:     CodeSharedVolume,
		Severity: Warning,
		Meaning:  "A volume is shared between envs because it is listed in compose.shared_volumes.",
		Cause:    "Deliberate configuration.",
		Hint:     "Nothing to do if this is intended. If the volume holds per-branch state, remove it from `compose.shared_volumes` so grove isolates it.",
	},
	{
		Code:     CodeVolumeIsolated,
		Severity: Warning,
		Meaning:  "A volume declared with a fixed name or external: true was scoped to this env instead.",
		Cause:    "A compose file written for a single environment, where pinning the volume name was harmless.",
		Hint:     "Nothing to do if you want per-env state — that is what grove did. If this volume is meant to be shared by every env (a model cache, a licence store), list it in `compose.shared_volumes` and grove will keep one copy for the whole project.",
	},
	{
		Code:     CodeMigrationClash,
		Severity: Warning,
		Meaning:  "Two live envs add a migration file with the same sequence prefix.",
		Cause:    "Two branches created a migration from the same starting number.",
		Hint:     "Renumber one of the migrations before merging. grove cannot resolve this: it is a git and process problem, visible here only because both branches are checked out at once.",
	},
	{
		Code:     CodeConfigInvalid,
		Severity: Error,
		Meaning:  "grove.toml is invalid.",
		Cause:    "A typo or a missing required key.",
		Hint:     "The message names the key and line. See docs/configuration.md for the reference.",
	},
	{
		Code:     CodeNoRoutable,
		Severity: Error,
		Meaning:  "No [[service]] can be reached over HTTP, so the env has no URL.",
		Cause:    "Every declared service is tcp = true, or none is declared.",
		Hint:     "Declare the HTTP service your browser opens, with its container port and default = true, e.g. [[service]] name = \"web\", port = 5173, default = true.",
	},
	{
		Code:     CodeDependsOnMissing,
		Severity: Error,
		Meaning:  "A service depends on a service that is not part of the enabled profiles.",
		Cause:    "A profile listed in grove.toml does not pull in everything the stack needs.",
		Hint:     "Add the missing profile to `compose.profiles`, or remove the dangling `depends_on` entry.",
	},
	{
		Code:     CodeDockerUnavailable,
		Severity: Error,
		Meaning:  "grove cannot reach a Docker-compatible engine.",
		Cause:    "The engine is not running, or DOCKER_HOST points somewhere else.",
		Hint:     "Start Docker Desktop, OrbStack, Colima or the Podman compat socket, then re-run. Check `docker version` and the DOCKER_HOST variable.",
	},
	{
		Code:     CodeUnhealthy,
		Severity: Error,
		Meaning:  "A service did not become healthy within the timeout.",
		Cause:    "A crash loop, a missing migration, or a healthcheck that is stricter than the service.",
		Hint:     "The evidence holds the last log lines. Reproduce with `" + meta.Name + " logs <env> <service>`, fix the service, then re-run. Raise `up.timeout` in grove.toml if the stack is simply slow to boot.",
	},
	{
		Code:     CodePortPublished,
		Severity: Error,
		Meaning:  "A container publishes a host port that grove did not ask for, so a second env will collide.",
		Cause:    "A `ports:` entry that survived transformation, usually added by a compose file grove did not load (check COMPOSE_FILE and compose.files).",
		Hint:     "Remove the host port from the compose file: grove reaches services through the router, so publishing is never needed. If you want a GUI client to connect directly, declare the service with tcp = true in grove.toml and grove will publish an ephemeral 127.0.0.1 port instead.",
	},
	{
		Code:     CodeStateShared,
		Severity: Error,
		Meaning:  "Two envs write to the same data volume, so their databases are not isolated.",
		Cause:    "A top-level volume declared with an explicit `name:` or `external: true`, or a bind mount to a shared host path.",
		Hint:     "Remove the `name:` and `external: true` keys from the volume in the compose file so compose prefixes it with the project name, or add the volume to `compose.shared_volumes` if the sharing is deliberate. A bind-mounted data directory must become a named volume.",
	},
	{
		Code:     CodeCrossEnvDNS,
		Severity: Error,
		Meaning:  "A service name inside one env resolves to a container of another env.",
		Cause:    "Both envs are attached to a shared network. Compose registers every service name as a DNS alias on every network the service joins.",
		Hint:     "Remove the shared external network from the dev compose file. grove attaches only the router to each env network, in one direction, so the envs never see one another.",
	},
	{
		Code:     CodeRouteFailed,
		Severity: Error,
		Meaning:  "A routable URL did not answer through the router.",
		Cause:    "A wrong `port` in grove.toml, a service bound to 127.0.0.1 inside its container, or a service that never started.",
		Hint:     "Check that `port` in the [[service]] block is the port the process listens on *inside* the container, and that the process binds 0.0.0.0 and not 127.0.0.1. A dev server usually needs --host 0.0.0.0.",
	},
	{
		Code:     CodeWebsocket,
		Severity: Error,
		Meaning:  "A WebSocket upgrade did not complete through the router.",
		Cause:    "A dev server that rejects the forwarded Host header, or a wrong `websocket_path`.",
		Hint:     "Set rewrite_host on the [[service]] (e.g. rewrite_host = \"localhost:5173\") so the dev server sees the Host it expects, or configure the dev server to allow the env hostname (Vite: server.allowedHosts).",
	},
	{
		Code:     CodePrepareNotIdem,
		Severity: Error,
		Meaning:  "Running prepare twice failed, so grove cannot apply a branch's migrations on top of a golden snapshot.",
		Cause:    "A seed step that inserts rows unconditionally, or a migration tool invoked in a way that re-applies everything.",
		Hint:     "Make the prepare command a no-op on an already prepared database: use the migration tool's `up` (not `reset`), and make seeds upsert rather than insert. grove clones a prepared database and then runs prepare again to add only the branch's own migrations.",
	},
	{
		Code:     CodeHTTPCheck,
		Severity: Error,
		Meaning:  "A user-defined [[doctor.http]] check failed.",
		Cause:    "The endpoint returned the wrong status or body, often because the frontend of one env talks to the backend of another.",
		Hint:     "The evidence holds the request, the status and a body excerpt. Use it to confirm each env answers with its own identity.",
	},
	{
		Code:     CodeLocalhostResolve,
		Severity: Warning,
		Meaning:  "A generated hostname does not resolve to the loopback address through the system resolver.",
		Cause:    "*.localhost is resolved by browsers but not always by other clients and OS resolvers.",
		Hint:     "Browsers will still work. For CLI clients, point a wildcard DNS name at 127.0.0.1 (e.g. with dnsmasq or a public wildcard domain) and set router.base_domain in grove.toml to it.",
	},
	{
		Code:     CodeLargeSnapshot,
		Severity: Warning,
		Meaning:  "A snapshot is large enough that copying it will be slow.",
		Cause:    "A seeded database bigger than state.warn_size.",
		Hint:     "The volume-copy driver costs O(size) per env. Trim the seed data, or wait for the reflink and pg-template drivers, which clone in constant time.",
	},
	{
		Code:     CodeTiming,
		Severity: Info,
		Meaning:  "A measured duration, reported so regressions are visible.",
		Cause:    "",
		Hint:     "",
	},
}

var byCode = func() map[Code]Entry {
	m := make(map[Code]Entry, len(catalogue))
	for _, e := range catalogue {
		m[e.Code] = e
	}
	return m
}()

func Lookup(c Code) Entry {
	if e, ok := byCode[c]; ok {
		return e
	}
	sev := Error
	if strings.HasPrefix(string(c), "W_") {
		sev = Warning
	} else if strings.HasPrefix(string(c), "I_") {
		sev = Info
	}
	return Entry{Code: c, Severity: sev}
}

func Catalogue() []Entry {
	out := make([]Entry, len(catalogue))
	copy(out, catalogue)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func Markdown() string {
	var b strings.Builder
	b.WriteString("# doctor codes\n\n")
	b.WriteString("Every finding `" + meta.Name + " doctor` emits carries one of these codes.\n")
	b.WriteString("Errors fail the run (exit code 6); warnings do not.\n\n")
	b.WriteString("This file is generated from `internal/finding/catalogue.go`.\n")
	b.WriteString("Regenerate it with `go generate ./internal/finding`.\n\n")

	var errs, warns, infos []Entry
	for _, e := range Catalogue() {
		switch e.Severity {
		case Error:
			errs = append(errs, e)
		case Warning:
			warns = append(warns, e)
		default:
			infos = append(infos, e)
		}
	}
	section := func(title string, entries []Entry) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n\n", title)
		for _, e := range entries {
			fmt.Fprintf(&b, "### `%s`\n\n", e.Code)
			fmt.Fprintf(&b, "**Meaning.** %s\n\n", e.Meaning)
			if e.Cause != "" {
				fmt.Fprintf(&b, "**Typical cause.** %s\n\n", e.Cause)
			}
			if e.Hint != "" {
				fmt.Fprintf(&b, "**Fix.** %s\n\n", e.Hint)
			}
		}
	}
	section("Errors", errs)
	section("Warnings", warns)
	section("Informational", infos)
	return b.String()
}
