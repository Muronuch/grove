# grove

Parallel per-branch development environments, for when several coding agents
are working at once.

Each branch gets a git worktree, its own containers, its own database, and a
stable URL. All of them run at the same time, on one machine, without colliding.

```
$ grove new feat/invoices
:: creating worktree worktrees/feat-invoices
:: cloning the golden snapshot of db (9e6104d45996)
:: starting 3 services
:: preparing db: api migrate
:: ready in 10s
web        http://feat-invoices.myapp.localhost
api        http://api.feat-invoices.myapp.localhost
db         127.0.0.1:55012
```

## The problem

Running one agent per branch in its own worktree isolates the *code*. It does
not isolate the *runtime*:

- every worktree's compose file publishes the same host ports;
- branches carry different migrations, so one shared database gets corrupted and
  one database per branch is slow to build;
- switching branches means stopping a stack, checking out, migrating, restarting;
- N full stacks idling in RAM is expensive.

grove fixes each of those, and tries hard not to ask you to change your project.

## How it works

**Ports.** grove strips every `ports:` entry and puts one small router container
in front instead. Each env is reachable at `<branch>.<project>.localhost`, and
each service at `<service>.<branch>.<project>.localhost`. Nothing binds a host
port, so nothing can collide.

The router is attached *to* each env's network, never the other way round: a
network shared between envs would make compose's service-name DNS resolve `api`
to every env's `api` at once.

**State.** A database is treated like a build cache. You declare how it is
prepared (`migrate`, `seed`) and which files that depends on. grove hashes those
files, builds one **golden snapshot**, and clones it for every new env — then
runs your prepare command again, which applies only that branch's own new
migrations. Cloning a prepared 46 MB Postgres takes under a second;
`grove db reset` puts an env back to that state in seconds.

**Sleep.** A background scheduler pauses environments nobody has touched, and
stops them when they have been idle longer. Opening the URL wakes the env
transparently — the request is held for a couple of seconds and then answered.

**Agents.** `grove mcp` exposes one environment over MCP, scoped to the working
directory it was started in. There is no tool that addresses another env or runs
anything on the host.

## Getting started

```
grove init          # read the compose file, write a grove.toml draft
grove doctor        # boot two throwaway envs and prove they are isolated
grove new <branch>  # worktree + cloned database + running stack + agent
```

`grove init` fills in everything it can infer and leaves a TODO wherever it
cannot — above all the command that migrates and seeds your database, which no
heuristic guesses reliably.

`grove doctor` is the loop that makes "every project is different" tractable. It
boots two disposable environments and checks that they cannot see one another,
reporting every problem with a stable code and a fix recipe:

```
W_HARDCODED_URL (web) web/src/api.js hardcodes http://localhost:8080, which only
works while exactly one environment exists
  fix: Read it from an environment variable and set that variable in grove.toml:
      [env.web]
      API_URL = "{{ url \"api\" }}"
  fix: Or serve the other service under the same hostname by adding
      paths = ["/api"] to its [[service]] block, which also removes the need for CORS.
```

Run it, apply the fixes, run it again. Exit code 0 means your project can be
run N times over.

## Commands

```
grove new <branch>            worktree + database + stack + agent
grove ls                      every env, its URL, memory and idle time
grove monitor                 the same table, live, with CPU, traffic and what sleeps next
grove status [env]            one env in detail
grove url [env] [service]     one URL, for piping into another tool
grove logs [env] <service>    recent output
grove exec [env] <svc> -- ..  run a command in a container
grove db prepare              apply migrations you just wrote
grove db reset                back to the snapshot plus this branch's migrations
grove db save <name>          checkpoint before something risky
grove down [env]              remove the env; the branch stays
grove doctor                  prove the project can be isolated
grove gc                      drop unused snapshots and orphaned resources
```

Every command infers the env from the current directory when you leave it out,
supports `--json` with a stable schema, and exits with a meaningful code
(3 unknown env, 4 no Docker, 5 unhealthy, 6 doctor findings, 7 lock busy).

## Configuration

One file, `grove.toml`, at the repository root. The full reference is in
[docs/configuration.md](docs/configuration.md); the short version:

```toml
[project]
name = "myapp"

[[service]]
name = "web"
port = 5173
default = true      # served at http://<env>.myapp.localhost
headless = false    # a dev server is pointless in an agent-only env

[[service]]
name = "api"
port = 8080
paths = ["/api"]    # also served on the web host, so no CORS

[[stateful]]
service = "db"
volume = "pgdata"
inputs = ["api/migrations/**", "api/seed/**"]

  [stateful.prepare]
  mode = "run"
  service = "api"
  command = ["./bin/migrate", "up", "--seed"]
```

## Requirements

A Docker-compatible engine (Docker Desktop, OrbStack, Colima, or Podman's compat
socket) and git. macOS and Linux are the supported platforms; Windows works
through WSL2.

## Status

Milestones M0 through M5 of the specification are implemented and verified
against two fixtures (Go + React + Postgres, Node + MySQL) plus a deliberately
hostile one. See [docs/DECISIONS.md](docs/DECISIONS.md) for the design decisions
taken along the way, and [docs/doctor-codes.md](docs/doctor-codes.md) for the
diagnostic catalogue.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
