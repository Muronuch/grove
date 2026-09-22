# Decisions

Every design decision taken while implementing the spec that the spec did not
already make. One entry per decision: context, decision, alternatives.

---

## D1 — Docker client module: `github.com/moby/moby/client`

**Context.** The spec asks to verify the currently recommended Engine API client
module, noting that it has been moving out of `github.com/docker/docker/client`.

**Decision.** Use `github.com/moby/moby/client` v0.6.0. It is a properly
versioned Go module (`github.com/docker/docker` is still published only as
`vX.Y.Z+incompatible`), it is the path the Moby project now documents, and its
API is option-struct based, which ages better than the long positional argument
lists of the old client.

**Alternatives.** `github.com/docker/docker/client` still works and is what most
tooling uses today, but pulling in a `+incompatible` module for a project that
otherwise has a clean dependency graph is a cost with no benefit. The client is
used through the small `engine.Runtime` interface, so swapping it later is a
contained change.

---

## D2 — A `finding` package of its own

**Context.** The spec's package layout puts the diagnostics catalogue in
`internal/doctor`. But the compose transformer also produces findings (rules
T4, 5.3), and doctor's static checks need to run the transformer. Putting the
type in `doctor` would make `transform` import `doctor` and `doctor` import
`transform`.

**Decision.** `internal/finding` holds the `Finding` type, the code constants
and the catalogue of meanings and fix hints. `transform`, `doctor`, `envctl`
and `cli` all import it; it imports only `meta`.

**Alternatives.** Duplicating a small issue type in `transform` and converting
in `doctor` — rejected because the fix hints would then live in two places and
drift. Passing findings as plain errors — rejected because the JSON contract in
spec 12.2 needs structure.

`docs/doctor-codes.md` is generated from the catalogue (`go generate
./internal/finding`), so the hints an agent reads and the hints grove prints are
the same strings. CI checks the file is current.

---

## D3 — An `envctl` orchestration package

**Context.** The spec's layout has `internal/cli` calling the lower packages
directly. But the same orchestration — create, up, down, reconcile, status,
prepare — is needed by the MCP server, the Claude Code hook handlers and the
scheduler daemon, none of which go through cobra.

**Decision.** `internal/envctl` holds the env lifecycle. `cli`, `mcp`, `hooks`
and `sched` are thin adapters over it. It owns the reconcile step that spec 4.4
requires of every listing or mutating command.

**Alternatives.** Duplicating the sequences in each entry point — rejected as
the surest way to end up with three subtly different `up` implementations.

---

## D4 — Config validation versus doctor completeness checks

**Context.** `grove init` deliberately leaves `[stateful.prepare].command` and
sometimes `inputs` empty, because neither can be guessed reliably. If the parser
rejected them, the freshly drafted file would not load at all and `grove doctor`
could not report anything else about it.

**Decision.** The parser enforces syntax and internal consistency (a mode is one
of three words, a path prefix starts with `/`, a template names a declared
service). Completeness for a particular project — is there a prepare command, are
there inputs — is a doctor finding (`E_PREPARE_MISSING`, `E_STATEFUL_INPUTS`)
with a fix recipe. The config always loads, and doctor reports every problem in
one pass, which is the loop the spec asks for in section 12.

**Alternatives.** Requiring them at parse time and having `init` emit the whole
`[[stateful]]` block commented out — rejected because doctor then cannot tell
the user that their Postgres service is unmanaged.

---

## D5 — Validation errors carry a line by scanning the source

**Context.** Spec 13 requires that "validation errors must name the TOML key and
line". `go-toml/v2` reports positions for decode errors but not for semantic
rules that run after decoding.

**Decision.** `config.findKeyLine` scans the raw TOML for the key, understanding
the `table[i].field` form used for arrays of tables. A miss degrades to an error
without a line rather than a wrong line.

**Alternatives.** Decoding into a position-aware AST and carrying positions
through the config structs — a large change to every struct for a small gain.

---

## D6 — One resolved compose file, never an override file

This one is in the spec (5.1) but is worth restating as a decision because it is
load-bearing: compose *appends* `ports` entries when merging files, so removing
a published port through an override needs `!reset`/`!override` tags, and those
tags are dropped when a merged config is re-serialised. grove therefore loads
the project's files, transforms the model in memory, and writes a single fully
resolved file to `<state>/envs/<project>/<slug>/compose.yaml`, which it runs
with `--project-directory <worktree>` so relative paths still resolve.

---

## D7 — Slug collision suffix is derived from the branch name

**Context.** Spec 2 says a colliding slug gets "`-` + 4 hex chars of the
branch-name hash" but does not say what happens if that also collides.

**Decision.** The suffix is `sha256(branch)[:4]`, so it is stable across runs
and across machines — the same branch always gets the same slug, which matters
because the slug appears in URLs. On the (vanishingly unlikely) second
collision the suffix widens deterministically to 6, 8, ... hex characters. The
base is truncated so that the total stays within 30 characters.

**Alternatives.** A counter (`-2`, `-3`) — rejected because it depends on
creation order, so the same branch could get a different URL on a different
machine.

---

## D8 — `restart: "no"` also clears `deploy.restart_policy`

**Context.** Rule T5 sets `restart: "no"` so slept envs do not resurrect when
the engine restarts. A compose file that sets `deploy.restart_policy` instead
would override it.

**Decision.** T5 sets both: `restart: "no"` and, when present,
`deploy.restart_policy.condition: none`.

---

## D9 — Service profiles are cleared in the emitted file

**Context.** Profiles are resolved when the model is loaded. If the emitted file
kept `profiles:` on services, every later `docker compose` call against that
file would need the same `--profile` flags or would silently drop services.

**Decision.** `transform` clears `Profiles` on every surviving service. The set
of services in the emitted file is exactly the set the env runs.

---

## D10 — Cache volumes are keyed `grove-cache-<name>` inside the model

**Context.** A `[[cache]]` called `gomod` must not collide with a project volume
called `gomod`.

**Decision.** In the compose model the cache volume takes the key
`grove-cache-<name>` and is declared `external: true` with the real name
`grove-cache-<project>-<name>`. Project volumes keep their own keys.

---

## D11 — `ContainerWait` uses "next-exit", not "not-running"

**Context.** The volume-copy driver runs a helper container and waits for it.
The obvious condition, `not-running`, returns *immediately* for a container
that has been created but not yet started — which is exactly the state the
container is in when the wait is registered before the start, as the API
documentation recommends.

**Decision.** Wait with `next-exit`, and afterwards confirm the exit code by
inspecting the container rather than trusting the stream alone. A closed error
channel is not treated as a failure; the wait continues for the result.

**Why it matters.** With `not-running`, `Run` returned as soon as it started
the container, the deferred removal killed the helper mid-copy, and grove saved
a **partially copied golden snapshot** that every later env would have cloned.
It surfaced as a 46 MB Postgres data directory arriving as 9 MB containing only
`base/`. This is the kind of bug that has to be found once and then made
impossible; the clean-exit assertion in `buildGolden` is the second line of
defence.

---

## D12 — An env is marked `creating` before its routes are pushed

**Context.** grove's health probe goes through the router, because the router is
the only component attached to the env's network and therefore the only one
that can tell "listening" from "not listening yet". The router refuses to proxy
to a `paused` or `stopped` env — it offers to wake it instead.

**Decision.** `up` moves the env to `creating` before pushing any route, and the
router proxies to `creating` envs (serving a self-refreshing "starting" page if
the dial fails, instead of a bare 502).

**Alternatives.** Probing containers directly from the host — impossible,
because env services publish no host ports, which is the whole point. Running a
helper container per poll — far too slow.

---

## D13 — `db reset` tears the whole stack down

**Context.** Replacing a data volume requires that nothing references it.
Stopping only the database and its dependents left the network in place with
the router attached, and compose then fought the router over recreating it.

**Decision.** `db reset` detaches the router, runs `compose down` (keeping
volumes), removes the stateful volumes, and brings the stack back up. A reset
restarts everything anyway, and this way there is no partial state for compose
and the router to disagree about.

---

## D14 — `GROVE_HOME` must be absolute

A relative value would resolve against the working directory, so the same
machine would have a different registry per worktree — silently. grove rejects
it with a message instead.

---

## D15 — Atomic writes retry the rename

On POSIX, `rename` over an existing file is atomic and unaffected by readers.
On Windows it fails while another process holds the destination open, and
read-only commands deliberately read the registry without the lock. The write
therefore retries the rename for about a second. The lock still guarantees a
single writer; the retry only covers the reader window.

---

## D16 — `grove prepare` does not cache a snapshot, `new` and `db reset` do

Spec 7.6 caches a branch snapshot after a successful prepare in the
*provisioning* flow. Doing it on every bare `grove db prepare` would stop the
database and copy its volume each time an agent iterates on a migration, which
is a surprising pause for no benefit: the next `db reset` recomputes the key and
caches then anyway.

---

## D17 — `grove doctor` runs its throwaway envs through full provisioning

**Context.** The two disposable envs doctor boots are meant to look like real
ones. Starting them with a bare `up` left their databases empty, so the
user-defined `[[doctor.http]]` checks failed for a reason no developer would
ever see.

**Decision.** `doctor` provisions state, starts, and prepares, exactly as
`grove new` does — only the worktree is skipped, because both envs run from the
current checkout.

---

## D18 — `websocket_protocol`

**Context.** Vite's HMR socket completes an upgrade only when the client offers
the `vite-hmr` subprotocol. Without it the server accepts the TCP connection and
then never answers, which looks exactly like a routing failure.

**Decision.** `[[service]]` gained `websocket_protocol`. The doctor probe sends
it, and the `go-react-pg` fixture sets it. Without this, doctor reports
`E_WEBSOCKET` against a perfectly working project — a false positive is worse
than a missing check, because it trains people to ignore the tool.

---

## D19 — grove only removes worktrees it created

**Context.** `grove down` deleted the worktree of any env. An *adopted* env, and
the throwaway envs doctor runs from the current checkout, point at a directory
the user made. Running `grove down doctor-a` deleted the repository.

**Decision.** `registry.Env.OwnsWorktree` is set only by `Create`. `down`
removes the directory only when it is true, and says so otherwise. grove removes
what grove created.

---

## D20 — A stale route-table version is adopted, not obeyed

**Context.** The router persists its route table and restores it on restart, and
rejects a table whose version is older than the one it holds. If the registry's
version goes backwards — a second state directory on the same machine, a deleted
`registry.json`, a restored backup — every push is rejected and routing silently
freezes at the old table.

**Decision.** On a stale rejection, `Sync` reads the router's current version and
republishes above it. The version exists to reject *out-of-order* pushes, and a
deliberate retry with a higher number is the correct answer, not a conflict to
surrender to. The previous behaviour (treat the rejection as "someone else is
newer, nothing to do") was wrong and produced a five-minute health-check timeout
with no explanation.

---

## D21 — The daemon is identified by a held lock, not a pidfile

**Context.** Spec 8 suggests a pidfile and a unix socket. A pidfile alone goes
stale when a daemon is killed; a socket is another listener to secure, and Go's
AF_UNIX support on Windows is uneven.

**Decision.** The daemon holds `locks/daemon.lock` for its lifetime, so any
process can ask "is it running?" race-free by trying the lock. Shutdown is a
`daemon.stop` sentinel file the daemon polls. A pidfile is still written, for
humans and for `daemon status`.

**Trade-off.** Stopping the daemon takes up to half a second longer than a signal
would. That is not a cost anyone notices.

---

## D22 — Windows: `CREATE_NO_WINDOW`, not `DETACHED_PROCESS`

A daemon started with `DETACHED_PROCESS` has no console at all, and a child
process it spawns cannot initialise the Docker CLI — every `docker compose` call
from the daemon died with `0xC0000142`. `CREATE_NO_WINDOW` hides the window and
keeps the console object. Windows is not a supported platform, but a scheduler
that silently cannot run compose is worth one constant to avoid.

---

## D23 — Two state directories, one project name, one set of Docker resources

The project name is the namespace for every Docker resource grove creates. Two
state directories on one machine that both manage a project called `myapp` share
those resources, and each one's `gc` will treat the other's envs as orphans.
This is inherent to using the project name as the namespace, and is the right
trade-off: the alternative is opaque generated prefixes that nobody can read in
`docker ps`. It matters only for tests and for deliberately parallel
installations; both should use distinct `project.name` values.

---

## D24 — Monitoring is a terminal command, not a web page

**Context.** Spec 16 sketches a dashboard at `<name>.localhost` with reset,
sleep and wake buttons.

**Decision.** There is no dashboard. `grove monitor` redraws the same table as
`grove ls` every couple of seconds with the memory, CPU, idle time, request
count and scheduler reasoning for every env. The router serves exactly two
pages: the "waking up" page, and a 404 that lists the envs that do exist.

**Why.** The buttons cannot be built safely. Control flows host → router only,
and the router has no Docker socket by design (spec 4.2) — it terminates
browser traffic, and socket access is root-equivalent. A button would need the
router to hand work back to the daemon, which means an action queue the router
accepts from unauthenticated page visitors. Any local web page can post to a
loopback port, so that queue would be a way for a visited site to stop a
developer's environments.

Without buttons the page is only a view, and it is the worse view. The router
sees a request when it happens to proxy one; it cannot see memory, CPU, or why
the scheduler is about to stop something, because it has no engine access and is
not supposed to. `monitor` runs on the host, so it reads all three — Docker
stats, the scheduler's own evaluation, and the router's counters — and it lands
where the work already is, next to the agent and the tests. It is also pipeable:
`--json` emits one frame, `--once` prints one table.

Waking never needed a page: opening a URL does it.

If a browser UI is wanted later, the honest shape is an authenticated origin
separate from the proxy, not a control path on the port that serves arbitrary
app code.

---

## D25 — A snapshot save waits for the database to come back

**Context.** Caching a branch snapshot stops the stateful service, copies its
volume and starts it again. `compose start` returns as soon as the container is
running, not when the database accepts connections.

**Decision.** `saveSnapshot` waits for health after restarting the service.

**Why it matters.** Without the wait, `grove new` returned "ready" while MySQL
was still recovering a freshly copied 196 MB data directory, and the very next
request got `ECONNREFUSED`. Postgres happened to recover fast enough to hide the
bug; MySQL did not. The integration test against a second database engine is
what found it, which is the reason the spec asks for one.

---

## D26 — Recreating the router is serialised, and its dependents are restarted
    after a snapshot

Two concurrency defects that only a second engine and a concurrent test found:

**The router is one container for the whole machine**, so recreating it needs a
lock. Without one, three concurrent `grove new` calls all decided to replace it
and two failed with *removal of container grove-router is already in progress*.
`Ensure` now checks without mutating, and takes `locks/router.lock` before it
changes anything, re-checking inside the lock in case another process got there
first.

**A snapshot save stops the database**, which leaves every service that holds a
connection holding a dead one. The env looked healthy — the API answered HTTP —
while its next database query failed with `driver: bad connection`. grove now
restarts the stateful service's dependents after a snapshot and waits for health
again. Applications should of course reconnect, and the fixtures do, but an env
that reports itself ready must actually be ready.

---

## D27 — Worktrees live inside the repository, in a directory that ignores itself

**Context.** Spec 5 leaves `worktree.dir` to the implementation. The first
default was `../<repo>.grove`, a sibling directory, on the reasoning that a
checkout nested in the repository would be walked by every recursive tool.

**Decision.** The default is `worktrees/` inside the repository. When the
directory is inside the repo, grove creates it with a `.gitignore` of its own
containing `*`.

**Why.** The sibling directory was solving the wrong problem. What a developer
actually wants from parallel branches is to see and commit their work, and
editors do that by discovering each git repository under the open folder and
giving it its own source-control entry. A sibling directory is outside the open
folder, so none of the envs appear and every review means opening another window.

The recursion argument turned out to be weak once measured. With the directory
gitignored, `git status` is clean, ripgrep skips it, and `go list ./...` never
saw it in the first place. The self-ignoring `.gitignore` is what makes this
work in *any* repository: grove never edits a file the user has committed, needs
no cooperation from their `.gitignore`, and leaves nothing behind but the
directory it created.

What the move does cost is Docker build context. A service that builds from the
repository root would now tar up every env's worktree. grove's own builds use
the worktree as the project directory, where the directory does not exist, so
only builds the developer runs by hand are affected — reported as
`W_WORKTREE_IN_BUILD_CONTEXT` rather than silently fixed, because the fix is a
`.dockerignore` line in a file that belongs to the user.

`git worktree add` has no default path of its own, so there was no convention to
follow either way.

---

## D28 — Copied files can be templates, and tracked files are reported

**Context.** `worktree.copy` puts gitignored files such as `.env` into each new
worktree. Every env gets a byte-identical copy, which is right for an API key
and wrong for anything that must differ per branch — a database name, a port, a
URL. Inside containers `[env.<service>]` already solves this; a process the
developer runs on the host reads the copied file instead.

**Decision.** `worktree.template` lists files rendered with the same data and
functions as `[env.<service>]` before being written. `worktree.copy` stays
byte-exact. Listing a file git tracks is reported as `W_COPY_TRACKED_FILE`.

**Why.** Rendering every copied file would have been the smaller change and the
wrong one: a `.env` may legitimately contain `{{ }}`, and silently rewriting a
developer's secrets file is not a thing to do by inference. Two lists make the
intent explicit at the point where it is declared.

The tracked-file warning exists because the failure is quiet and permanent.
Copying over a committed file leaves every worktree with a modified file nobody
edited, and a rendered one can never be committed back without its template
markers. Both lists are for files git does not have; saying so in a finding is
cheaper than explaining it after the fact.

