package doctor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/state"
	"github.com/Muronuch/grove/internal/transform"
)

func (r *Runner) static(ctx context.Context) {
	cfg := r.Project.Config

	if len(cfg.RoutableServices()) == 0 {
		r.add(finding.New(finding.CodeNoRoutable,
			"no [[service]] is reachable over HTTP, so an env would have no URL"))
	}

	model, err := transform.LoadForProject(ctx, cfg, r.Project.Root, "grove-doctor-probe", nil)
	if err != nil {
		r.add(finding.New(finding.CodeConfigInvalid, "the compose project could not be loaded: %v", err).
			WithHint("Run `docker compose config` in %s to see what compose makes of it.", r.Project.Root))
		return
	}

	published := map[string]bool{}
	for _, p := range transform.OriginalPorts(model) {
		if p.Published != "" {
			published[p.Published] = true
		}
		published[strconv.FormatUint(uint64(p.Target), 10)] = true
	}

	res, err := transform.Apply(model, cfg, transform.Context{
		Project:  cfg.Project.Name,
		Slug:     DefaultEnvA,
		Slot:     1,
		Repo:     r.Project.RepoName(),
		Worktree: r.Project.Root,
	})
	if err != nil {
		r.add(finding.New(finding.CodeConfigInvalid, "the compose model could not be transformed: %v", err))
		return
	}
	r.add(res.Findings...)

	r.checkStateful(res)
	r.checkPorts(res)
	r.checkCopyTracked(ctx)
	r.checkWorktreeBuildContext(res)
	r.scanHardcodedURLs(ctx, res, published)
	r.checkMigrationCollisions(ctx)
}

func (r *Runner) checkCopyTracked(ctx context.Context) {
	wt := r.Project.Config.Worktree
	entries := append(append([]string{}, wt.Copy...), wt.Template...)
	if len(entries) == 0 {
		return
	}
	git := r.Project.Git()
	for _, rel := range entries {
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			continue
		}
		files, err := git.TrackedFiles(ctx, filepath.ToSlash(rel))
		if err != nil || len(files) == 0 {
			continue
		}
		r.add(finding.New(finding.CodeCopyTracked,
			"worktree.copy/template lists %q, which git tracks", rel))
	}
}

func (r *Runner) checkWorktreeBuildContext(res *transform.Result) {
	base, err := r.Project.WorktreeBase()
	if err != nil || !underDir(r.Project.Root, base) {
		return
	}
	for _, name := range sortedServiceNames(res.Project.Services) {
		svc := res.Project.Services[name]
		if svc.Build == nil || svc.Build.Context == "" {
			continue
		}
		context := svc.Build.Context
		if !filepath.IsAbs(context) {
			context = filepath.Join(r.Project.Root, context)
		}
		rel, err := filepath.Rel(context, base)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if dockerIgnores(context, filepath.ToSlash(rel)) {
			continue
		}
		r.add(finding.New(finding.CodeWorktreeInBuild,
			"service %q builds from %s, which contains the worktree directory %s",
			name, context, base).WithService(name))
		return
	}
}

func dockerIgnores(context, rel string) bool {
	body, err := os.ReadFile(filepath.Join(context, ".dockerignore"))
	if err != nil {
		return false
	}
	want := strings.Trim(rel, "/")
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSuffix(strings.TrimPrefix(line, "/"), "/")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == want || line == want+"/**" || line == want+"/*" {
			return true
		}
	}
	return false
}

func underDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedServiceNames(services types.Services) []string {
	out := make([]string, 0, len(services))
	for name := range services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *Runner) checkStateful(res *transform.Result) {
	cfg := r.Project.Config
	if len(cfg.Stateful) == 0 {
		for name, svc := range res.Project.Services {
			if !looksLikeDatastore(name, svc) {
				continue
			}
			r.add(finding.New(finding.CodePrepareMissing,
				"service %q looks like a datastore but no [[stateful]] block manages it, so every env will migrate from scratch",
				name).WithService(name))
		}
		return
	}
	for _, st := range cfg.Stateful {
		if len(st.Inputs) == 0 {
			r.add(finding.New(finding.CodeStatefulInputs,
				"[[stateful]] %s has no inputs, so grove cannot tell when its snapshot is stale",
				st.Service).WithService(st.Service))
		}
		if !st.Prepare.Defined() {
			r.add(finding.New(finding.CodePrepareMissing,
				"[[stateful]] %s has no prepare command, so grove cannot migrate or seed it",
				st.Service).WithService(st.Service))
			continue
		}
		if st.Prepare.Service != "" {
			if _, ok := res.Project.Services[st.Prepare.Service]; !ok {
				r.add(finding.New(finding.CodeServiceMissing,
					"[[stateful]] %s runs prepare in service %q, which the compose project does not define",
					st.Service, st.Prepare.Service).WithService(st.Prepare.Service))
			}
		}

		key, err := state.ComputeKey(state.KeyInput{
			Driver: "volume-copy", Worktree: r.Project.Root, Inputs: st.Inputs, Prepare: st.Prepare,
		})
		if err != nil {
			r.add(finding.New(finding.CodeStatefulInputs,
				"[[stateful]] %s: %v", st.Service, err).WithService(st.Service))
			continue
		}
		if len(key.Files) == 0 {
			r.add(finding.New(finding.CodeStatefulInputs,
				"[[stateful]] %s: inputs %v match no files in %s, so the snapshot key would never change",
				st.Service, st.Inputs, r.Project.Root).WithService(st.Service))
		} else {
			r.detail("%s: %d input files decide the snapshot key", st.Service, len(key.Files))
		}
	}
}

func (r *Runner) checkPorts(res *transform.Result) {
	cfg := r.Project.Config
	for _, s := range cfg.Service {
		svc, ok := res.Project.Services[s.Name]
		if !ok {
			continue
		}
		if portDeclared(svc, uint32(s.Port)) {
			continue
		}

		r.detail("%s: the compose file does not mention port %d (grove will still route to it)", s.Name, s.Port)
	}
}

func portDeclared(svc types.ServiceConfig, port uint32) bool {
	for _, p := range svc.Ports {
		if p.Target == port {
			return true
		}
	}
	for _, e := range svc.Expose {
		if n, err := strconv.ParseUint(strings.SplitN(e, "/", 2)[0], 10, 32); err == nil && uint32(n) == port {
			return true
		}
	}
	return len(svc.Ports) == 0 && len(svc.Expose) == 0
}

var localhostURL = regexp.MustCompile(`(?i)\b(?:https?://)?(?:localhost|127\.0\.0\.1|0\.0\.0\.0)(?::(\d{2,5}))\b`)

func (r *Runner) scanHardcodedURLs(ctx context.Context, res *transform.Result, published map[string]bool) {
	if len(published) == 0 {
		return
	}

	git := r.Project.Git()
	seen := map[string]bool{}
	for _, s := range r.Project.Config.RoutableServices() {
		svc, ok := res.Project.Services[s.Name]
		if !ok {
			continue
		}
		var roots []string
		if svc.Build != nil && svc.Build.Context != "" {
			roots = append(roots, svc.Build.Context)
		}
		for _, ef := range svc.EnvFiles {
			roots = append(roots, ef.Path)
		}
		for _, root := range roots {
			rel, err := filepath.Rel(r.Project.Root, root)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			files, err := git.TrackedFiles(ctx, filepath.ToSlash(rel))
			if err != nil {
				continue
			}
			for _, f := range files {
				if seen[f] || !scannable(f) {
					continue
				}
				seen[f] = true
				r.scanFile(filepath.Join(r.Project.Root, filepath.FromSlash(f)), f, s.Name, published)
			}
		}
	}
}

func scannable(rel string) bool {
	lower := strings.ToLower(rel)
	for _, skip := range []string{"node_modules/", "vendor/", "dist/", "build/", ".min.", "-lock.", "lock.json", "go.sum"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	switch filepath.Ext(lower) {
	case ".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".woff", ".woff2", ".ttf", ".pdf", ".zip", ".gz":
		return false
	}
	return true
}

func (r *Runner) scanFile(abs, rel, service string, published map[string]bool) {
	f, err := os.Open(abs)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > 1<<20 {
		return
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		m := localhostURL.FindStringSubmatch(text)
		if m == nil || !published[m[1]] {
			continue
		}
		r.add(finding.New(finding.CodeHardcodedURL,
			"%s hardcodes %s, which only works while exactly one environment exists",
			rel, strings.TrimSpace(m[0])).
			WithService(service).
			WithEvidence("file", rel, "line", line, "match", strings.TrimSpace(m[0])).
			WithHint("Read it from an environment variable and set that variable in %s:\n"+
				"    [env.%s]\n    API_URL = \"{{ url \\\"<service>\\\" }}\"\n"+
				"Or serve the other service under the same hostname by adding paths = [\"/api\"] to its [[service]] block, which also removes the need for CORS.",
				meta.Name+".toml", service))

		return
	}
}

func (r *Runner) checkMigrationCollisions(ctx context.Context) {
	cfg := r.Project.Config
	if len(cfg.Stateful) == 0 {
		return
	}
	reg, err := r.Ctl.Store.Read()
	if err != nil {
		return
	}
	ps, ok := reg.LookupProject(cfg.Project.Name)
	if !ok {
		return
	}

	byPrefix := map[string]map[string]string{}
	for _, e := range ps.List() {
		if e.Worktree == "" || strings.HasPrefix(e.Slug, "doctor-") {
			continue
		}
		for _, st := range cfg.Stateful {
			key, err := state.ComputeKey(state.KeyInput{
				Driver: "volume-copy", Worktree: e.Worktree, Inputs: st.Inputs, Prepare: st.Prepare,
			})
			if err != nil {
				continue
			}
			for _, f := range key.Files {
				p := sequencePrefix(filepath.Base(f))
				if p == "" {
					continue
				}
				if byPrefix[p] == nil {
					byPrefix[p] = map[string]string{}
				}
				byPrefix[p][e.Slug] = f
			}
		}
	}

	for prefix, envs := range byPrefix {
		if len(envs) < 2 {
			continue
		}
		names := map[string]bool{}
		for _, f := range envs {
			names[filepath.Base(f)] = true
		}
		if len(names) < 2 {
			continue
		}
		var parts []string
		for env, f := range envs {
			parts = append(parts, fmt.Sprintf("%s has %s", env, filepath.Base(f)))
		}
		r.add(finding.New(finding.CodeMigrationClash,
			"two envs add a migration numbered %s: %s", prefix, strings.Join(parts, ", ")).
			WithEvidence("prefix", prefix, "envs", envs))
	}
}

func sequencePrefix(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i < 3 {
		return ""
	}
	return name[:i]
}

func looksLikeDatastore(name string, svc types.ServiceConfig) bool {
	hay := strings.ToLower(name + " " + svc.Image)
	for _, m := range []string{"postgres", "mysql", "mariadb", "mongo", "redis", "clickhouse", "elasticsearch", "cassandra", "valkey"} {
		if strings.Contains(hay, m) {
			for _, v := range svc.Volumes {
				if v.Type == types.VolumeTypeVolume {
					return true
				}
			}
		}
	}
	return false
}
