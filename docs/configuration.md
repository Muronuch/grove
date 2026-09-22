# grove.toml

One file at the repository root, committed. `grove init` writes a draft of it.

Every key is optional except `project.name` and at least one `[[service]]`.
Validation errors name the key and the line.

---

## `[project]`

```toml
[project]
name = "myapp"            # required; appears in every hostname
default_branch = "main"   # what a new branch starts from, and what goldens are built from
max_slots = 8             # how many envs may exist at once
```

`name` must match `^[a-z0-9][a-z0-9-]{0,29}$`: it goes into hostnames, Docker
resource names and file paths.

---

## `[compose]`

```toml
[compose]
files = ["docker-compose.yml"]   # default: compose's own discovery, including COMPOSE_FILE
profiles = ["dev"]               # profiles to enable
env_file = [".env.grove"]        # overrides the default .env lookup
shared_volumes = []              # volumes that must NOT be isolated per env
```

A volume named in `shared_volumes` gets one copy for the whole project
(`grove-shared-<project>-<volume>`) instead of one per env. Use it for things
that are expensive and identical everywhere — a model cache, a licence store —
never for application state.

---

## `[worktree]`

```toml
[worktree]
dir      = "worktrees"               # relative to the repository root; one subdirectory per env
copy     = [".env", ".env.local"]    # gitignored files every new worktree needs, verbatim
template = [".env.shared"]           # the same, but rendered per env
```

`dir` defaults to `worktrees` **inside** the repository, so an editor lists each
env as its own source-control entry beside the main checkout — which is how you
review and commit a branch's work without leaving the window. grove keeps the
directory out of git by writing a `.gitignore` containing `*` inside it, so no
file you have committed is ever modified, and `git status`, ripgrep and every
other tool that honours gitignore stay clean.

Point `dir` outside the repository if you prefer; `../{{ .Repo }}.grove` is the
usual shape. `.Repo` and `.Project` are available. One caveat when it stays
inside: a service that builds from the repository root would send every env's
worktree to the Docker daemon. grove's own builds run from the worktree, where
the directory does not exist, so only builds you run by hand are affected —
`grove doctor` reports it as `W_WORKTREE_IN_BUILD_CONTEXT`, and a
`.dockerignore` entry settles it.

`copy` and `template` entries are copied, never symlinked: a symlink breaks once
the directory is bind-mounted into a container. Both are relative paths inside
the repository, both accept directories (`template` takes files only), and both
silently skip what the developer does not have — these lists describe what a
worktree needs *if it exists*.

`template` renders the file with the same data as `[env.<service>]`, which is
what lets one file in the main checkout give every env its own value:

```
DATABASE_NAME=app_{{ .Slug }}
PUBLIC_URL={{ url "web" }}
```

Both lists are for files git does not track. Naming a committed file makes grove
overwrite it in every worktree, so every env starts dirty; `grove doctor` reports
that as `W_COPY_TRACKED_FILE`. A tracked file that must differ per env usually
belongs in `[env.<service>]` instead.

---

## `[router]`

```toml
[router]
port = 80                 # the host port browsers use; falls back to 7080 if it cannot be bound
admin_port = 9180         # loopback control API, bearer-token protected
base_domain = "localhost"
bind = "127.0.0.1"        # never set this to 0.0.0.0 unless you mean to expose every env
self_aliases = false      # see below
```

`base_domain` is the escape hatch for clients that do not resolve `*.localhost`.
Point a wildcard DNS name at 127.0.0.1 and set it here.

`self_aliases` adds each env's hostnames as network aliases of the router *on
that env's network*, so a container can reach its own public URL. It is off by
default because some resolvers short-circuit `.localhost` before DNS, which
makes the behaviour inconsistent across base images.

---

## `[[service]]`

One block per service you want to reach.

```toml
[[service]]
name = "web"                  # the compose service name
port = 5173                   # the port it listens on INSIDE the container
default = true                # this one is served at http://<env>.<project>.localhost
headless = false              # leave it out of agent-only envs
websocket_path = "/"          # doctor checks that an upgrade completes here
websocket_protocol = "vite-hmr"  # a subprotocol the dev server insists on
rewrite_host = "localhost:5173"  # if the dev server validates the Host header

[[service]]
name = "api"
port = 8080
paths = ["/api", "/ws"]       # also served on the default host, which removes CORS entirely

[[service]]
name = "db"
port = 5432
tcp = true                    # an ephemeral 127.0.0.1 port for a GUI client, no hostname
```

`port` is the container port, not a published one. grove never publishes a host
port for an HTTP service.

`paths` is the most useful line in this file: serving the API under the
frontend's hostname means the frontend calls `/api` with no configured URL and
no CORS, which removes the single biggest source of per-environment breakage.

---

## `[env.<service>]`

Environment variables injected into one service, with templates.

```toml
[env.web]
VITE_API_URL = "{{ url \"api\" }}"

[env.api]
PUBLIC_URL   = "{{ url \"web\" }}"
CORS_ORIGINS = "{{ url \"web\" }},{{ slotUrl \"web\" }}"
```

Functions: `url "<service>"`, `slotUrl "<service>"`, `host "<service>"`,
`slotHost "<service>"`. Fields: `.Env`, `.Slot`, `.Project`, `.Repo`,
`.Service`, `.Domain`.

`slotUrl` is the stable per-slot alias (`s3.myapp.localhost`). It does not change
when branches come and go, which is what makes it usable in an OAuth redirect
list or a CAPTCHA allowlist.

grove also injects, into every service:

```
GROVE_ENV, GROVE_PROJECT, GROVE_SLOT, GROVE_SERVICE, GROVE_DOMAIN
GROVE_URL_<SERVICE>, GROVE_HOST_<SERVICE>   for each routable service
```

---

## `[[stateful]]`

A service whose data volume grove snapshots.

```toml
[[stateful]]
service = "db"
volume  = "pgdata"                  # a TOP-LEVEL named volume in the compose file
inputs  = ["api/migrations/**", "api/seed/**"]
stop_grace = "60s"                  # how long it gets to shut down cleanly

  [stateful.prepare]
  mode    = "run"                   # run | exec | host
  service = "api"                   # which service runs the command
  command = ["./bin/migrate", "up", "--seed"]
  timeout = "10m"
```

`inputs` decides the snapshot key: grove hashes those files (from the working
tree, so uncommitted migrations count) together with the database image and the
prepare command. Change any of them and the next env builds a new golden.

**`prepare` must be idempotent.** grove clones a database that has already been
prepared and runs the command again so that only the branch's own new migrations
apply. `grove doctor` verifies this (`E_PREPARE_NOT_IDEMPOTENT`).

`mode = "host"` runs the command on the host in the worktree, for projects whose
migration tool is not in any image. It receives `GROVE_TCP_<SERVICE>` for each
service declared `tcp = true`.

---

## `[[cache]]`

Package caches shared across every env of the project, on purpose.

```toml
[[cache]]
name = "gomod"
path = "/go/pkg/mod"
services = ["api"]
```

They survive `grove down`, which is the point: a new env should not re-download
the module cache.

---

## `[limits.<service>]`

```toml
[limits.api]
mem  = "512m"
cpus = 1.5
```

---

## `[hooks]`

Shell commands run **on the host**, in the new worktree.

```toml
[hooks]
post_create = ["pnpm --dir web install --frozen-lockfile"]
post_up     = []
pre_down    = []
```

These come from a committed file, so grove prints them and asks the first time it
sees a given set for a project, and remembers the answer by content hash.
`--trust` skips the question.

---

## `[state]`

```toml
[state]
driver = "volume-copy"           # the only driver in this version
cache_branch_snapshots = true    # cache an env's own prepared state after `new` and `db reset`
gc_after = "14d"                 # remove snapshots unused for longer
keep_goldens = 3                 # always keep this many newest goldens per service
helper_image = "busybox:1.37.0"  # pinned: it is part of the snapshot key
warn_size = "5g"                 # warn above this, because copying costs O(size)
```

---

## `[sleep]`

```toml
[sleep]
enabled = true
pause_after = "15m"      # freeze: memory kept, wake is instant
stop_after  = "2h"       # stop: memory freed, wake is a cold start
memory_budget = "50%"    # of the engine's memory, across all envs
cpu_floor = 5            # above this much CPU an env counts as busy
interval = "30s"         # how often the policy is evaluated
```

An env is never slept while it is pinned, while a hold is unexpired, while its
lock is held by an operation, or while it is using CPU. `grove daemon status`
shows the current reasoning for every env.

---

## `[agent]`

```toml
[agent]
command = ["claude"]
```

What `grove new` starts in the worktree. `--no-agent` skips it.

---

## `[up]`

```toml
[up]
timeout = "5m"    # how long services get to become healthy
build = true      # pass --build to compose up
```

---

## `[[doctor.http]]`

Checks of your own, run against both throwaway envs.

```toml
[[doctor.http]]
service = "api"
path = "/healthz"
expect_status = 200

[[doctor.http]]
service = "api"
path = "/api/whoami"
expect_body_contains = "\"env\":\"{{ .Env }}\""
```

The second form is the valuable one: it proves that the env answering is the env
you asked for, which is the failure mode that isolation bugs actually produce.
