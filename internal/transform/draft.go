package transform

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/meta"
)

type DraftOptions struct {
	ProjectName   string
	Repo          string
	DefaultBranch string
	ComposeFiles  []string
	Profiles      []string
}

var statefulImages = regexp.MustCompile(`(?i)(^|/)(postgres|postgis|timescale|mysql|mariadb|percona|mongo|redis|valkey|keydb|clickhouse|minio|elasticsearch|opensearch|cassandra|couchdb|neo4j|influxdb|surrealdb)`)

var tcpPorts = map[uint32]bool{
	5432: true, 3306: true, 27017: true, 6379: true, 9000: true, 9042: true,
	5984: true, 7687: true, 8086: true, 1433: true, 11211: true, 5672: true,
}

var httpRank = map[uint32]int{
	3000: 0, 5173: 0, 4200: 0, 8000: 1, 80: 1, 8080: 2, 8081: 3, 5000: 3, 9090: 4,
}

var migrationDirs = []string{"migrations", "migration", "migrate", "schema", "seeds", "seed", "fixtures", "sql"}

func Draft(p *types.Project, o DraftOptions) []byte {
	var b strings.Builder
	name := o.ProjectName
	if !config.NameRE.MatchString(name) {
		name = sanitizeName(name)
	}

	fmt.Fprintf(&b, "# %s configuration, drafted by `%s init` from %s.\n",
		meta.Name, meta.Name, strings.Join(displayFiles(o.ComposeFiles), ", "))
	fmt.Fprintf(&b, "# Everything below is a guess except where a TODO says otherwise.\n")
	fmt.Fprintf(&b, "# Run `%s doctor` to check it against two real environments.\n\n", meta.Name)

	fmt.Fprintf(&b, "[project]\n")
	fmt.Fprintf(&b, "name = %q            # appears in every hostname: <env>.%s.localhost\n", name, name)
	if o.DefaultBranch != "" {
		fmt.Fprintf(&b, "default_branch = %q\n", o.DefaultBranch)
	}
	fmt.Fprintf(&b, "max_slots = %d\n\n", config.DefaultMaxSlots)

	if len(o.ComposeFiles) > 0 || len(o.Profiles) > 0 {
		fmt.Fprintf(&b, "[compose]\n")
		if len(o.ComposeFiles) > 0 {
			fmt.Fprintf(&b, "files = [%s]\n", quoteList(displayFiles(o.ComposeFiles)))
		}
		if len(o.Profiles) > 0 {
			fmt.Fprintf(&b, "profiles = [%s]\n", quoteList(o.Profiles))
		}
		fmt.Fprintf(&b, "# shared_volumes = []   # volumes that must NOT be isolated per env\n\n")
	}

	fmt.Fprintf(&b, "[worktree]\n")
	fmt.Fprintf(&b, "# Inside the repository, so an editor lists every env beside the main\n")
	fmt.Fprintf(&b, "# checkout. %s keeps the directory out of git by writing its own\n", meta.Name)
	fmt.Fprintf(&b, "# .gitignore, so nothing you have committed needs changing.\n")
	fmt.Fprintf(&b, "dir = %q\n", config.DefaultWorktreeDir)
	fmt.Fprintf(&b, "copy = [%s]   # gitignored files each new worktree needs, verbatim\n", quoteList(guessCopyFiles(o.Repo)))
	fmt.Fprintf(&b, "# template = []   # the same, but rendered: {{ .Slug }}, {{ .Slot }}, {{ url \"web\" }}\n\n")

	routable, tcp := classifyServices(p)
	if len(routable) == 0 && len(tcp) == 0 {
		fmt.Fprintf(&b, "# TODO: no service publishes a port, so grove could not guess which service\n")
		fmt.Fprintf(&b, "# you open in a browser. Declare it here with the port it listens on\n")
		fmt.Fprintf(&b, "# *inside* the container:\n")
		fmt.Fprintf(&b, "# [[service]]\n# name = \"web\"\n# port = 3000\n# default = true\n\n")
	}
	for i, s := range routable {
		svc := p.Services[s.name]
		fmt.Fprintf(&b, "[[service]]\n")
		fmt.Fprintf(&b, "name = %q\n", s.name)
		fmt.Fprintf(&b, "port = %d\n", s.port)
		if i == 0 {
			fmt.Fprintf(&b, "default = true       # served on http://<env>.%s.localhost\n", name)
		}
		if looksLikeFrontend(s.name, svc) {
			fmt.Fprintf(&b, "headless = false     # a dev server is pointless in an agent-only env\n")
			fmt.Fprintf(&b, "# websocket_path = \"/\"   # HMR socket, checked by `%s doctor`\n", meta.Name)
			if host := devServerHost(svc, s.port); host != "" {
				fmt.Fprintf(&b, "# rewrite_host = %q  # if the dev server rejects the env hostname\n", host)
			}
		} else if i > 0 {
			fmt.Fprintf(&b, "# paths = [\"/api\"]   # also serve on the default host, which removes CORS\n")
		}
		b.WriteString("\n")
	}
	for _, s := range tcp {
		fmt.Fprintf(&b, "[[service]]\n")
		fmt.Fprintf(&b, "name = %q\n", s.name)
		fmt.Fprintf(&b, "port = %d\n", s.port)
		fmt.Fprintf(&b, "tcp = true           # ephemeral 127.0.0.1 port for a GUI client\n\n")
	}

	if len(routable) > 1 {
		fmt.Fprintf(&b, "# Templated environment injection. `url \"<service>\"` resolves to that\n")
		fmt.Fprintf(&b, "# service's URL in *this* env, so nothing has to hardcode a port.\n")
		api, web := routable[len(routable)-1].name, routable[0].name
		fmt.Fprintf(&b, "# [env.%s]\n", web)
		fmt.Fprintf(&b, "# VITE_API_URL = \"{{ url %s }}\"\n", tomlQuoted(api))
		fmt.Fprintf(&b, "# [env.%s]\n", api)
		fmt.Fprintf(&b, "# CORS_ORIGINS = \"{{ url %s }},{{ slotUrl %s }}\"\n\n", tomlQuoted(web), tomlQuoted(web))
	}

	inputs := guessMigrationInputs(o.Repo)
	for _, st := range statefulCandidates(p) {
		fmt.Fprintf(&b, "[[stateful]]\n")
		fmt.Fprintf(&b, "service = %q\n", st.service)
		fmt.Fprintf(&b, "volume  = %q\n", st.volume)
		if len(inputs) > 0 {
			fmt.Fprintf(&b, "inputs  = [%s]\n", quoteList(inputs))
			fmt.Fprintf(&b, "# A change to any of these files builds a new golden snapshot.\n")
		} else {
			fmt.Fprintf(&b, "inputs  = []   # TODO: globs whose content decides the schema, e.g. [\"migrations/**\"]\n")
		}
		fmt.Fprintf(&b, "\n  [stateful.prepare]\n")
		fmt.Fprintf(&b, "  # TODO: the command that migrates and seeds the database.\n")
		fmt.Fprintf(&b, "  # It MUST be idempotent: grove clones a prepared database and runs it\n")
		fmt.Fprintf(&b, "  # again so that only this branch's new migrations are applied.\n")
		fmt.Fprintf(&b, "  mode    = \"run\"        # run | exec | host\n")
		if svc := guessMigrationService(p, st.service); svc != "" {
			fmt.Fprintf(&b, "  service = %q\n", svc)
		} else {
			fmt.Fprintf(&b, "  service = \"\"           # TODO: the service that owns the migration tool\n")
		}
		fmt.Fprintf(&b, "  command = []           # TODO: e.g. [\"./bin/migrate\", \"up\", \"--seed\"]\n\n")
	}

	for _, c := range guessCaches(p) {
		fmt.Fprintf(&b, "[[cache]]\n")
		fmt.Fprintf(&b, "name = %q\n", c.name)
		fmt.Fprintf(&b, "path = %q\n", c.path)
		fmt.Fprintf(&b, "services = [%s]   # shared across envs on purpose\n\n", quoteList(c.services))
	}

	fmt.Fprintf(&b, "[hooks]\n")
	fmt.Fprintf(&b, "# Shell commands run on the host, in the new worktree. They are printed and\n")
	fmt.Fprintf(&b, "# confirmed the first time grove sees them for a project.\n")
	fmt.Fprintf(&b, "post_create = []\n")
	fmt.Fprintf(&b, "post_up = []\n\n")

	fmt.Fprintf(&b, "[state]\n")
	fmt.Fprintf(&b, "driver = %q\n", config.DefaultDriver)
	fmt.Fprintf(&b, "cache_branch_snapshots = true\n")
	fmt.Fprintf(&b, "gc_after = \"14d\"\n")
	fmt.Fprintf(&b, "keep_goldens = %d\n\n", config.DefaultKeepGoldens)

	fmt.Fprintf(&b, "[sleep]\n")
	fmt.Fprintf(&b, "pause_after = \"15m\"     # freeze: memory kept, wake is instant\n")
	fmt.Fprintf(&b, "stop_after  = \"2h\"      # stop: memory freed, wake is a cold start\n")
	fmt.Fprintf(&b, "memory_budget = \"50%%\"   # of the engine's memory, across all envs\n\n")

	if len(routable) > 0 {
		fmt.Fprintf(&b, "# A check that proves each env's frontend talks to its own backend.\n")
		fmt.Fprintf(&b, "# [[doctor.http]]\n")
		fmt.Fprintf(&b, "# service = %q\n", routable[len(routable)-1].name)
		fmt.Fprintf(&b, "# path = \"/healthz\"\n")
		fmt.Fprintf(&b, "# expect_status = 200\n")
	}
	return []byte(b.String())
}

type portCandidate struct {
	name string
	port uint32
	rank int
}

func classifyServices(p *types.Project) (routable, tcp []portCandidate) {
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		seen := map[uint32]bool{}
		candidates := make([]uint32, 0, len(s.Ports)+len(s.Expose))
		for _, port := range s.Ports {
			if port.Target > 0 && !seen[port.Target] {
				seen[port.Target] = true
				candidates = append(candidates, port.Target)
			}
		}
		if len(candidates) == 0 {
			for _, e := range s.Expose {
				if n, err := strconv.ParseUint(strings.SplitN(e, "/", 2)[0], 10, 32); err == nil && !seen[uint32(n)] {
					seen[uint32(n)] = true
					candidates = append(candidates, uint32(n))
				}
			}
		}
		for _, port := range candidates {
			if tcpPorts[port] || statefulImages.MatchString(s.Image) {
				tcp = append(tcp, portCandidate{name: name, port: port})
				break
			}
			rank, known := httpRank[port]
			if !known {
				rank = 5
			}
			routable = append(routable, portCandidate{name: name, port: port, rank: rank})
			break
		}
	}
	sort.SliceStable(routable, func(i, j int) bool {
		if routable[i].rank != routable[j].rank {
			return routable[i].rank < routable[j].rank
		}
		return routable[i].name < routable[j].name
	})
	return routable, tcp
}

type statefulCandidate struct {
	service string
	volume  string
}

func statefulCandidates(p *types.Project) []statefulCandidate {
	var out []statefulCandidate
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		if !statefulImages.MatchString(s.Image) && !statefulImages.MatchString(name) {
			continue
		}
		for _, v := range s.Volumes {
			if v.Type != types.VolumeTypeVolume || v.Source == "" {
				continue
			}
			if _, ok := p.Volumes[v.Source]; !ok {
				continue
			}
			out = append(out, statefulCandidate{service: name, volume: v.Source})
			break
		}
	}
	return out
}

func guessMigrationService(p *types.Project, store string) string {
	var withBuild, withDep []string
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		if name == store {
			continue
		}
		if _, ok := s.DependsOn[store]; !ok {
			continue
		}
		withDep = append(withDep, name)
		if s.Build != nil {
			withBuild = append(withBuild, name)
		}
	}
	if len(withBuild) > 0 {
		return withBuild[0]
	}
	if len(withDep) > 0 {
		return withDep[0]
	}
	return ""
}

type cacheCandidate struct {
	name     string
	path     string
	services []string
}

var knownCaches = []struct {
	name     string
	path     string
	markers  []string
	lockfile string
}{
	{name: "gomod", path: "/go/pkg/mod", markers: []string{"golang"}},
	{name: "gobuild", path: "/root/.cache/go-build", markers: []string{"golang"}},
	{name: "pnpm", path: "/root/.local/share/pnpm/store", markers: []string{"node"}, lockfile: "pnpm-lock.yaml"},
	{name: "yarn", path: "/usr/local/share/.cache/yarn", markers: []string{"node"}, lockfile: "yarn.lock"},
	{name: "npm", path: "/root/.npm", markers: []string{"node"}, lockfile: "package.json"},
	{name: "pip", path: "/root/.cache/pip", markers: []string{"python"}},
	{name: "cargo", path: "/usr/local/cargo/registry", markers: []string{"rust"}},
	{name: "maven", path: "/root/.m2", markers: []string{"maven", "gradle", "openjdk", "eclipse-temurin"}},
}

func guessCaches(p *types.Project) []cacheCandidate {
	haystacks := map[string]string{}
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		hay := strings.ToLower(name + " " + s.Image)
		if s.Build != nil {
			hay += " " + strings.ToLower(dockerfileBaseImages(s.Build))
		}
		haystacks[name] = hay
	}
	claimed := map[string]bool{}
	var out []cacheCandidate
	for _, kc := range knownCaches {
		var services []string
		for _, name := range serviceNames(p) {
			matched := false
			for _, m := range kc.markers {
				if strings.Contains(haystacks[name], m) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			if kc.lockfile != "" {
				if claimed[name] || !hasBuildFile(p.Services[name].Build, kc.lockfile) {
					continue
				}
				claimed[name] = true
			}
			services = append(services, name)
		}
		if len(services) > 0 {
			out = append(out, cacheCandidate{name: kc.name, path: kc.path, services: services})
		}
	}
	return out
}

func hasBuildFile(b *types.BuildConfig, name string) bool {
	if b == nil || b.Context == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(b.Context, name))
	return err == nil && !st.IsDir()
}

func dockerfileBaseImages(b *types.BuildConfig) string {
	if b.DockerfileInline != "" {
		return b.DockerfileInline
	}
	name := b.Dockerfile
	if name == "" {
		name = "Dockerfile"
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.Context, name)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var froms []string
	for _, line := range strings.Split(string(body), "\n") {
		t := strings.TrimSpace(line)
		if len(t) > 5 && strings.EqualFold(t[:5], "FROM ") {
			froms = append(froms, t[5:])
		}
	}
	return strings.Join(froms, " ")
}

func guessMigrationInputs(repo string) []string {
	if repo == "" {
		return nil
	}
	var found []string
	skip := map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, "target": true, ".next": true}
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if skip[d.Name()] || strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}

		if strings.Count(rel, string(filepath.Separator)) > 3 {
			return fs.SkipDir
		}
		for _, m := range migrationDirs {
			if strings.EqualFold(d.Name(), m) {
				found = append(found, path.Join(filepath.ToSlash(rel), "**"))
				return fs.SkipDir
			}
		}
		return nil
	})
	sort.Strings(found)
	if len(found) > 4 {
		found = found[:4]
	}
	return found
}

func guessCopyFiles(repo string) []string {
	if repo == "" {
		return []string{".env"}
	}
	var out []string
	for _, name := range []string{".env", ".env.local", ".env.development", ".envrc"} {
		if st, err := os.Stat(filepath.Join(repo, name)); err == nil && !st.IsDir() {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		out = []string{".env"}
	}
	return out
}

func looksLikeFrontend(name string, s types.ServiceConfig) bool {
	hay := strings.ToLower(name + " " + s.Image + " " + strings.Join(s.Command, " "))
	for _, m := range []string{"vite", "next", "nuxt", "webpack", "frontend", "web", "ui", "client", "storybook", "ng serve"} {
		if strings.Contains(hay, m) {
			return true
		}
	}
	return false
}

func devServerHost(s types.ServiceConfig, port uint32) string {
	if s.Build == nil && s.Image == "" {
		return ""
	}
	return "localhost:" + strconv.FormatUint(uint64(port), 10)
}

func displayFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.ToSlash(f))
	}
	if len(out) == 0 {
		out = []string{"the project's compose file"}
	}
	return out
}

func tomlQuoted(s string) string { return `\"` + s + `\"` }

func quoteList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, i := range items {
		parts = append(parts, strconv.Quote(i))
	}
	return strings.Join(parts, ", ")
}

func sanitizeName(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 30 {
		out = strings.TrimRight(out[:30], "-")
	}
	if out == "" {
		return "project"
	}
	return out
}
