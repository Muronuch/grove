package transform

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/meta"
)

type Context struct {
	Project  string
	Slug     string
	Slot     int
	Repo     string
	Worktree string
	Headless bool
	Only     []string
}

func (c Context) Identity() config.EnvIdentity {
	return config.EnvIdentity{Project: c.Project, Slug: c.Slug, Slot: c.Slot, Repo: c.Repo}
}

func (c Context) ComposeProject() string { return meta.ComposeProject(c.Project, c.Slug) }

type Result struct {
	Project         *types.Project
	Findings        finding.List
	TCPServices     map[string]uint32
	Upstreams       map[string]string
	Volumes         map[string]string
	ExternalVolumes []string
	Networks        []string
	DefaultNetwork  string
	Omitted         []string
}

func Apply(p *types.Project, cfg *config.Config, ec Context) (*Result, error) {
	res := &Result{
		Project:     p,
		TCPServices: map[string]uint32{},
		Upstreams:   map[string]string{},
		Volumes:     map[string]string{},
	}
	composeProject := ec.ComposeProject()

	loadedAs := p.Name

	p.Name = composeProject

	if err := selectServices(p, cfg, ec, res); err != nil {
		return nil, err
	}
	checkUnsupported(p, ec, res)

	hosts := cfg.Hosts(ec.Identity())
	names := serviceNames(p)

	for _, name := range names {
		s := p.Services[name]
		svcCfg, declared := cfg.ServiceByName(name)

		s.Ports = nil
		if declared && svcCfg.TCP && svcCfg.Port > 0 {
			target := uint32(svcCfg.Port)
			s.Ports = []types.ServicePortConfig{{
				HostIP:   "127.0.0.1",
				Target:   target,
				Protocol: "tcp",
				Mode:     "ingress",
			}}
			res.TCPServices[name] = target
		}

		if s.ContainerName != "" {
			res.Findings.Add(finding.New(finding.CodeContainerName,
				"service %q sets container_name: %q; grove removes it so a second env can run",
				name, s.ContainerName).WithService(name))
			s.ContainerName = ""
		}

		s.Restart = "no"
		if s.Deploy != nil && s.Deploy.RestartPolicy != nil {
			s.Deploy.RestartPolicy.Condition = "none"
		}

		s.Profiles = nil

		s.Labels = withLabels(s.Labels, ec, name)
		if s.CustomLabels == nil {
			s.CustomLabels = types.Labels{}
		}

		env, err := injectedEnv(cfg, ec, hosts, name)
		if err != nil {
			return nil, err
		}
		s.Environment = mergeEnv(s.Environment, env)

		for _, ch := range cfg.CachesFor(name) {
			vol := meta.CacheVolume(ec.Project, ch.Name)
			s.Volumes = append(s.Volumes, types.ServiceVolumeConfig{
				Type:   types.VolumeTypeVolume,
				Source: cacheVolumeKey(ch.Name),
				Target: ch.Path,
			})
			res.Volumes[cacheVolumeKey(ch.Name)] = vol
		}

		if lim, ok := cfg.Limits[name]; ok {
			if lim.Memory.Bytes > 0 {
				s.MemLimit = types.UnitBytes(lim.Memory.Bytes)
			}
			if lim.CPUs > 0 {
				s.CPUS = float32(lim.CPUs)
			}
		}

		if declared && svcCfg.Routable() {
			replicas := 1
			if s.Deploy != nil && s.Deploy.Replicas != nil {
				replicas = *s.Deploy.Replicas
			}
			if s.Scale != nil && *s.Scale > 1 {
				replicas = *s.Scale
			}
			if replicas > 1 {
				res.Findings.Add(finding.New(finding.CodeScale,
					"service %q runs %d replicas; the router sends every request to replica 1",
					name, replicas).WithService(name))
			}
			res.Upstreams[name] = fmt.Sprintf("%s:%d",
				meta.ContainerName(composeProject, name, 1), svcCfg.Port)
		}

		p.Services[name] = s
	}

	applyVolumes(p, cfg, ec, res, loadedAs)
	applyNetworks(p, ec, res)
	checkDeclaredServices(p, cfg, res)

	return res, nil
}

func selectServices(p *types.Project, cfg *config.Config, ec Context, res *Result) error {
	drop := map[string]bool{}
	if ec.Headless {
		for _, s := range cfg.Service {
			if !s.InHeadless() {
				if _, ok := p.Services[s.Name]; ok {
					drop[s.Name] = true
				}
			}
		}
	}
	if len(ec.Only) > 0 {
		keep, err := withDependencies(p, ec.Only)
		if err != nil {
			return err
		}
		for name := range p.Services {
			if !keep[name] {
				drop[name] = true
			}
		}
	}
	if len(drop) == 0 {
		return nil
	}
	for name := range drop {
		delete(p.Services, name)
		res.Omitted = append(res.Omitted, name)
	}
	sort.Strings(res.Omitted)

	for name, s := range p.Services {
		if len(s.DependsOn) == 0 {
			continue
		}
		for dep := range s.DependsOn {
			if drop[dep] {
				delete(s.DependsOn, dep)
			}
		}
		p.Services[name] = s
	}
	return nil
}

func withDependencies(p *types.Project, roots []string) (map[string]bool, error) {
	keep := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if keep[name] {
			return nil
		}
		s, ok := p.Services[name]
		if !ok {
			return fmt.Errorf("service %q is not part of the compose project (services: %s)",
				name, strings.Join(serviceNames(p), ", "))
		}
		keep[name] = true
		for dep := range s.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := visit(r); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

func checkUnsupported(p *types.Project, ec Context, res *Result) {
	worktree := filepath.Clean(ec.Worktree)
	for _, name := range serviceNames(p) {
		s := p.Services[name]
		if s.NetworkMode == "host" || s.Net == "host" {
			res.Findings.Add(finding.New(finding.CodeHostNetwork,
				"service %q uses network_mode: host, which cannot be isolated: every env would bind the same host ports",
				name).WithService(name))
		}
		for _, v := range s.Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			src := filepath.Clean(v.Source)
			if worktree == "" || !isAbsolutePath(v.Source) {
				continue
			}
			if under(worktree, src) {
				continue
			}
			res.Findings.Add(finding.New(finding.CodeAbsoluteBind,
				"service %q bind-mounts %s, which is outside the worktree and therefore shared by every env",
				name, v.Source).WithService(name).WithEvidence("source", v.Source, "target", v.Target))
		}
	}
	for name, n := range p.Networks {
		if !bool(n.External) {
			continue
		}
		var users []string
		for _, sname := range serviceNames(p) {
			if _, ok := p.Services[sname].Networks[name]; ok {
				users = append(users, sname)
			}
		}
		if len(users) == 0 {
			continue
		}
		res.Findings.Add(finding.New(finding.CodeExternalNetwork,
			"network %q is external, so %s of every env share it; compose registers each service name as a DNS alias on it and traffic would cross envs",
			name, strings.Join(users, ", ")).WithEvidence("network", name, "services", users))
	}
}

func checkDeclaredServices(p *types.Project, cfg *config.Config, res *Result) {
	omitted := map[string]bool{}
	for _, s := range res.Omitted {
		omitted[s] = true
	}
	for _, s := range cfg.Service {
		if _, ok := p.Services[s.Name]; ok {
			continue
		}

		if omitted[s.Name] {
			continue
		}
		if _, disabled := p.DisabledServices[s.Name]; disabled {
			res.Findings.Add(finding.New(finding.CodeServiceMissing,
				"grove.toml declares [[service]] %q but it is only enabled by a profile that is not in compose.profiles",
				s.Name).WithService(s.Name))
			continue
		}
		res.Findings.Add(finding.New(finding.CodeServiceMissing,
			"grove.toml declares [[service]] %q but the compose project has no such service (services: %s)",
			s.Name, strings.Join(serviceNames(p), ", ")).WithService(s.Name))
	}
	for _, st := range cfg.Stateful {
		if _, ok := p.Services[st.Service]; !ok {
			if omitted[st.Service] {
				continue
			}
			if _, disabled := p.DisabledServices[st.Service]; !disabled {
				res.Findings.Add(finding.New(finding.CodeServiceMissing,
					"grove.toml declares [[stateful]] for %q but the compose project has no such service",
					st.Service).WithService(st.Service))
			}
			continue
		}
		if _, ok := p.Volumes[st.Volume]; !ok {
			res.Findings.Add(finding.New(finding.CodeVolumeMissing,
				"[[stateful]] %s names volume %q, which the compose file does not declare as a top-level volume (declared: %s)",
				st.Service, st.Volume, strings.Join(volumeNames(p), ", ")).WithService(st.Service))
		}
	}
}

func applyVolumes(p *types.Project, cfg *config.Config, ec Context, res *Result, loadedAs string) {
	shared := map[string]bool{}
	for _, v := range cfg.Compose.SharedVolumes {
		shared[v] = true
	}
	if p.Volumes == nil {
		p.Volumes = types.Volumes{}
	}
	for _, name := range volumeNames(p) {
		v := p.Volumes[name]

		pinned := v.Name != "" &&
			v.Name != meta.ComposeVolume(p.Name, name) &&
			v.Name != meta.ComposeVolume(loadedAs, name)

		if shared[name] {
			if !pinned {
				v.Name = meta.SharedVolume(ec.Project, name)
			}
			v.External = true
			res.Findings.Add(finding.New(finding.CodeSharedVolume,
				"volume %q is shared between every env of this project (compose.shared_volumes)", name))
			res.Volumes[name] = v.Name
			res.ExternalVolumes = append(res.ExternalVolumes, v.Name)
			p.Volumes[name] = v
			continue
		}
		if pinned || bool(v.External) {
			res.Findings.Add(finding.New(finding.CodeVolumeIsolated,
				"volume %q pins a fixed name, so every env would share it; grove scopes it to this env instead", name).
				WithEvidence("volume", name, "declared_name", v.Name, "external", bool(v.External)))
		}

		v.Name = ""
		v.External = false
		v.Labels = withLabels(v.Labels, ec, "")
		p.Volumes[name] = v
		res.Volumes[name] = meta.ComposeVolume(p.Name, name)
	}
	for _, ch := range cfg.Cache {
		key := cacheVolumeKey(ch.Name)
		full := meta.CacheVolume(ec.Project, ch.Name)
		if _, used := res.Volumes[key]; !used {
			continue
		}
		p.Volumes[key] = types.VolumeConfig{Name: full, External: true}
		res.Volumes[key] = full
		res.ExternalVolumes = append(res.ExternalVolumes, full)
	}
	sort.Strings(res.ExternalVolumes)
}

func applyNetworks(p *types.Project, ec Context, res *Result) {
	if p.Networks == nil {
		p.Networks = types.Networks{}
	}
	if _, ok := p.Networks["default"]; !ok {
		p.Networks["default"] = types.NetworkConfig{}
	}
	names := make([]string, 0, len(p.Networks))
	for name := range p.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := p.Networks[name]
		if bool(n.External) {
			if n.Name != "" {
				res.Networks = append(res.Networks, n.Name)
			} else {
				res.Networks = append(res.Networks, name)
			}
			continue
		}
		n.Labels = withLabels(n.Labels, ec, "")
		full := n.Name
		if full == "" {
			full = meta.ComposeNetwork(p.Name, name)
		}
		p.Networks[name] = n
		res.Networks = append(res.Networks, full)
		if name == "default" {
			res.DefaultNetwork = full
		}
	}
	if res.DefaultNetwork == "" && len(res.Networks) > 0 {
		res.DefaultNetwork = res.Networks[0]
	}
}

func NetworkForService(p *types.Project, res *Result, service string) string {
	s, ok := p.Services[service]
	if !ok {
		return res.DefaultNetwork
	}
	if len(s.Networks) == 0 {
		return res.DefaultNetwork
	}
	if _, onDefault := s.Networks["default"]; onDefault {
		return res.DefaultNetwork
	}
	keys := make([]string, 0, len(s.Networks))
	for k := range s.Networks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n, ok := p.Networks[keys[0]]
	if !ok {
		return res.DefaultNetwork
	}
	if n.Name != "" {
		return n.Name
	}
	return meta.ComposeNetwork(p.Name, keys[0])
}

func RouterNetworks(p *types.Project, cfg *config.Config, res *Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range cfg.RoutableServices() {
		if _, ok := p.Services[s.Name]; !ok {
			continue
		}
		n := NetworkForService(p, res, s.Name)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 && res.DefaultNetwork != "" {
		out = append(out, res.DefaultNetwork)
	}
	sort.Strings(out)
	return out
}

func injectedEnv(cfg *config.Config, ec Context, hosts config.HostSet, service string) (map[string]string, error) {
	out := map[string]string{
		meta.EnvVarName("ENV"):     ec.Slug,
		meta.EnvVarName("PROJECT"): ec.Project,
		meta.EnvVarName("SLOT"):    strconv.Itoa(ec.Slot),
		meta.EnvVarName("SERVICE"): service,
		meta.EnvVarName("DOMAIN"):  hosts.Domain(),
	}
	for _, s := range cfg.RoutableServices() {
		out[meta.EnvVarName("URL", s.Name)] = hosts.URL(s.Name)
		out[meta.EnvVarName("HOST", s.Name)] = hosts.Host(s.Name)
	}
	user, err := cfg.RenderEnvFor(service, ec.Identity())
	if err != nil {
		return nil, err
	}
	for k, v := range user {
		out[k] = v
	}
	return out, nil
}

func mergeEnv(base types.MappingWithEquals, add map[string]string) types.MappingWithEquals {
	if base == nil {
		base = types.MappingWithEquals{}
	}
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := add[k]
		base[k] = &v
	}
	return base
}

func withLabels(l types.Labels, ec Context, service string) types.Labels {
	if l == nil {
		l = types.Labels{}
	}
	l[meta.LabelManaged] = "true"
	l[meta.LabelProject] = ec.Project
	l[meta.LabelEnv] = ec.Slug
	l[meta.LabelSlot] = strconv.Itoa(ec.Slot)
	if service != "" {
		l[meta.LabelService] = service
	}
	return l
}

func cacheVolumeKey(name string) string { return meta.Name + "-cache-" + name }

func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	return filepath.IsAbs(p)
}

func under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !strings.HasPrefix(rel, "../")
}

func serviceNames(p *types.Project) []string {
	out := make([]string, 0, len(p.Services))
	for name := range p.Services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func volumeNames(p *types.Project) []string {
	out := make([]string, 0, len(p.Volumes))
	for name := range p.Volumes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
