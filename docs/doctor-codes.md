# doctor codes

Every finding `grove doctor` emits carries one of these codes.
Errors fail the run (exit code 6); warnings do not.

This file is generated from `internal/finding/catalogue.go`.
Regenerate it with `go generate ./internal/finding`.

## Errors

### `E_CONFIG`

**Meaning.** grove.toml is invalid.

**Typical cause.** A typo or a missing required key.

**Fix.** The message names the key and line. See docs/configuration.md for the reference.

### `E_CROSS_ENV_DNS`

**Meaning.** A service name inside one env resolves to a container of another env.

**Typical cause.** Both envs are attached to a shared network. Compose registers every service name as a DNS alias on every network the service joins.

**Fix.** Remove the shared external network from the dev compose file. grove attaches only the router to each env network, in one direction, so the envs never see one another.

### `E_DEPENDS_ON_MISSING`

**Meaning.** A service depends on a service that is not part of the enabled profiles.

**Typical cause.** A profile listed in grove.toml does not pull in everything the stack needs.

**Fix.** Add the missing profile to `compose.profiles`, or remove the dangling `depends_on` entry.

### `E_DOCKER_UNAVAILABLE`

**Meaning.** grove cannot reach a Docker-compatible engine.

**Typical cause.** The engine is not running, or DOCKER_HOST points somewhere else.

**Fix.** Start Docker Desktop, OrbStack, Colima or the Podman compat socket, then re-run. Check `docker version` and the DOCKER_HOST variable.

### `E_EXTERNAL_NETWORK`

**Meaning.** A service joins an external network with a fixed name, so all envs would share it.

**Typical cause.** A stack that is wired to another compose project, e.g. a shared monitoring or proxy network.

**Fix.** Either drop the external network from the dev compose file, or, if the envs really must share it, add the network's services to `compose.shared_volumes`' network equivalent by keeping the network external and accepting the sharing — then declare it in grove.toml under [compose] and re-run doctor. Traffic on a shared network crosses envs because compose registers each service name as a DNS alias on every network it joins.

### `E_HOST_NETWORK`

**Meaning.** A service uses network_mode: host, which puts it directly on the host network.

**Typical cause.** A dev stack that was written for a single environment and wants to reach host processes.

**Fix.** Remove `network_mode: host` from the service and let it use the project network. If it needs to reach a process on the host, use `host.docker.internal` and add `extra_hosts: ["host.docker.internal:host-gateway"]`. Host networking cannot be isolated: every env would share the same ports.

### `E_HTTP_CHECK`

**Meaning.** A user-defined [[doctor.http]] check failed.

**Typical cause.** The endpoint returned the wrong status or body, often because the frontend of one env talks to the backend of another.

**Fix.** The evidence holds the request, the status and a body excerpt. Use it to confirm each env answers with its own identity.

### `E_NO_ROUTABLE_SERVICE`

**Meaning.** No [[service]] can be reached over HTTP, so the env has no URL.

**Typical cause.** Every declared service is tcp = true, or none is declared.

**Fix.** Declare the HTTP service your browser opens, with its container port and default = true, e.g. [[service]] name = "web", port = 5173, default = true.

### `E_PORT_PUBLISHED`

**Meaning.** A container publishes a host port that grove did not ask for, so a second env will collide.

**Typical cause.** A `ports:` entry that survived transformation, usually added by a compose file grove did not load (check COMPOSE_FILE and compose.files).

**Fix.** Remove the host port from the compose file: grove reaches services through the router, so publishing is never needed. If you want a GUI client to connect directly, declare the service with tcp = true in grove.toml and grove will publish an ephemeral 127.0.0.1 port instead.

### `E_PREPARE_MISSING`

**Meaning.** A stateful service has no prepare command, so grove cannot migrate or seed it.

**Typical cause.** `grove init` left the command as a TODO because it cannot be guessed.

**Fix.** Fill in [stateful.prepare] with the command that migrates and seeds the database, e.g. mode = "run", service = "api", command = ["./bin/migrate", "up", "--seed"]. It must be idempotent: running it twice must be a no-op.

### `E_PREPARE_NOT_IDEMPOTENT`

**Meaning.** Running prepare twice failed, so grove cannot apply a branch's migrations on top of a golden snapshot.

**Typical cause.** A seed step that inserts rows unconditionally, or a migration tool invoked in a way that re-applies everything.

**Fix.** Make the prepare command a no-op on an already prepared database: use the migration tool's `up` (not `reset`), and make seeds upsert rather than insert. grove clones a prepared database and then runs prepare again to add only the branch's own migrations.

### `E_ROUTE_FAILED`

**Meaning.** A routable URL did not answer through the router.

**Typical cause.** A wrong `port` in grove.toml, a service bound to 127.0.0.1 inside its container, or a service that never started.

**Fix.** Check that `port` in the [[service]] block is the port the process listens on *inside* the container, and that the process binds 0.0.0.0 and not 127.0.0.1. A dev server usually needs --host 0.0.0.0.

### `E_SERVICE_MISSING`

**Meaning.** grove.toml names a compose service that the compose file does not define.

**Typical cause.** A renamed service, or a service that only exists under a profile that is not enabled.

**Fix.** Fix the name in grove.toml, or add the profile that defines the service to `compose.profiles`.

### `E_STATEFUL_INPUTS`

**Meaning.** A [[stateful]] entry has no inputs, so grove cannot tell when its snapshot is stale.

**Typical cause.** `grove init` could not find a migrations directory to guess from.

**Fix.** Set `inputs` to the globs whose content decides the database's shape, e.g. inputs = ["backend/migrations/**", "backend/seed/**"]. grove hashes those files; a change to any of them builds a new golden snapshot.

### `E_STATE_SHARED`

**Meaning.** Two envs write to the same data volume, so their databases are not isolated.

**Typical cause.** A top-level volume declared with an explicit `name:` or `external: true`, or a bind mount to a shared host path.

**Fix.** Remove the `name:` and `external: true` keys from the volume in the compose file so compose prefixes it with the project name, or add the volume to `compose.shared_volumes` if the sharing is deliberate. A bind-mounted data directory must become a named volume.

### `E_UNHEALTHY`

**Meaning.** A service did not become healthy within the timeout.

**Typical cause.** A crash loop, a missing migration, or a healthcheck that is stricter than the service.

**Fix.** The evidence holds the last log lines. Reproduce with `grove logs <env> <service>`, fix the service, then re-run. Raise `up.timeout` in grove.toml if the stack is simply slow to boot.

### `E_VOLUME_MISSING`

**Meaning.** A [[stateful]] entry names a top-level volume the compose file does not declare.

**Typical cause.** The database writes to a bind mount or an anonymous volume instead of a named volume.

**Fix.** Declare a named volume in the compose file and mount it at the database's data directory, then set `volume` in the [[stateful]] block to that name. grove can only snapshot named volumes; a bind mount lives in the worktree and an anonymous volume cannot be cloned.

### `E_WEBSOCKET`

**Meaning.** A WebSocket upgrade did not complete through the router.

**Typical cause.** A dev server that rejects the forwarded Host header, or a wrong `websocket_path`.

**Fix.** Set rewrite_host on the [[service]] (e.g. rewrite_host = "localhost:5173") so the dev server sees the Host it expects, or configure the dev server to allow the env hostname (Vite: server.allowedHosts).

## Warnings

### `W_ABSOLUTE_BIND`

**Meaning.** A bind mount points at an absolute host path outside the worktree, so every env shares it.

**Typical cause.** A mount such as /var/run/docker.sock, a shared data directory, or a path made absolute by an env var.

**Fix.** If the path holds state that must differ per env, move it under the repository so each worktree gets its own copy, or turn it into a named volume (grove isolates named volumes automatically). If sharing is intended, nothing to do — this warning records it.

### `W_CONTAINER_NAME`

**Meaning.** A service sets container_name, which is unique per engine and makes a second env impossible.

**Typical cause.** A compose file written before parallel environments were a requirement.

**Fix.** Delete the `container_name:` line from the service. grove strips it anyway, so anything that referred to the container by that fixed name (scripts, `docker exec`) must use the compose service name instead: `grove exec <env> <service> -- ...`.

### `W_COPY_TRACKED_FILE`

**Meaning.** A worktree.copy or worktree.template entry is a file git tracks, so every worktree starts dirty.

**Typical cause.** The entry names a committed file rather than a gitignored one such as .env.

**Fix.** These lists exist for files git does not have. Either gitignore the file, or, if it is meant to be committed, stop listing it — the worktree already gets the committed version. A tracked file that must differ per env is a sign the value belongs in [env.<service>] instead.

### `W_HARDCODED_URL`

**Meaning.** A tracked file hardcodes a localhost URL with a published port.

**Typical cause.** A frontend or client that was written against one fixed dev stack.

**Fix.** Read the URL from an environment variable and set it in grove.toml, e.g. [env.web] VITE_API_URL = "{{ url \"api\" }}" — or serve the API under the same hostname by adding paths = ["/api"] to the api [[service]], which also removes the need for CORS.

### `W_LARGE_SNAPSHOT`

**Meaning.** A snapshot is large enough that copying it will be slow.

**Typical cause.** A seeded database bigger than state.warn_size.

**Fix.** The volume-copy driver costs O(size) per env. Trim the seed data, or wait for the reflink and pg-template drivers, which clone in constant time.

### `W_LOCALHOST_RESOLUTION`

**Meaning.** A generated hostname does not resolve to the loopback address through the system resolver.

**Typical cause.** *.localhost is resolved by browsers but not always by other clients and OS resolvers.

**Fix.** Browsers will still work. For CLI clients, point a wildcard DNS name at 127.0.0.1 (e.g. with dnsmasq or a public wildcard domain) and set router.base_domain in grove.toml to it.

### `W_MIGRATION_COLLISION`

**Meaning.** Two live envs add a migration file with the same sequence prefix.

**Typical cause.** Two branches created a migration from the same starting number.

**Fix.** Renumber one of the migrations before merging. grove cannot resolve this: it is a git and process problem, visible here only because both branches are checked out at once.

### `W_SCALE`

**Meaning.** A routable service declares more than one replica; only replica 1 is routed.

**Typical cause.** `deploy.replicas` or `scale` greater than 1 in the compose file.

**Fix.** Set the replica count to 1 for dev environments, or accept that the router sends every request to replica 1.

### `W_SHARED_VOLUME`

**Meaning.** A volume is shared between envs because it is listed in compose.shared_volumes.

**Typical cause.** Deliberate configuration.

**Fix.** Nothing to do if this is intended. If the volume holds per-branch state, remove it from `compose.shared_volumes` so grove isolates it.

### `W_VOLUME_ISOLATED`

**Meaning.** A volume declared with a fixed name or external: true was scoped to this env instead.

**Typical cause.** A compose file written for a single environment, where pinning the volume name was harmless.

**Fix.** Nothing to do if you want per-env state — that is what grove did. If this volume is meant to be shared by every env (a model cache, a licence store), list it in `compose.shared_volumes` and grove will keep one copy for the whole project.

### `W_WORKTREE_IN_BUILD_CONTEXT`

**Meaning.** worktree.dir sits inside a service's build context, so every build would ship every env's worktree.

**Typical cause.** worktree.dir defaults to a directory inside the repository, and a service builds from the repository root.

**Fix.** Add the worktree directory to .dockerignore. grove builds from the worktree, where the directory does not exist, so this only affects builds you run yourself — but those would send every env's checkout to the daemon. Moving worktree.dir outside the repository also fixes it.

## Informational

### `I_TIMING`

**Meaning.** A measured duration, reported so regressions are visible.

