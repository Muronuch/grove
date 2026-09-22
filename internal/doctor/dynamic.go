package doctor

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/meta"
)

func (r *Runner) helperImage() string {
	if img := r.Project.Config.State.HelperImage; img != "" {
		return img
	}
	return "busybox:1.37.0"
}

func (r *Runner) dynamic(ctx context.Context, o Options) error {
	a, err := r.bring(ctx, o, o.EnvA)
	if err != nil {
		return err
	}
	b, err := r.bring(ctx, o, o.EnvB)
	if err != nil {
		if !o.Keep {
			r.teardown(ctx, a)
		}
		return err
	}
	if !o.Keep {
		defer func() {
			r.teardown(ctx, a)
			r.teardown(ctx, b)
		}()
	} else {
		r.detail("keeping %s and %s running (--keep)", o.EnvA, o.EnvB)
	}

	if a == nil || b == nil {
		return nil
	}

	r.checkNoPublishedPorts(ctx, a, b)
	r.checkVolumeIsolation(ctx, a, b)
	r.checkNetworkIsolation(ctx, a)
	r.checkRoutes(ctx, a)
	r.checkRoutes(ctx, b)
	r.checkWebSockets(ctx, a)
	r.checkUserHTTP(ctx, a)
	r.checkUserHTTP(ctx, b)
	r.checkPrepareIdempotent(ctx, a)
	r.checkHostnameResolution(a)
	return nil
}

func (r *Runner) bring(ctx context.Context, o Options, slug string) (*envctl.Target, error) {
	r.step("starting throwaway env %s", slug)
	t, err := r.Ctl.Ephemeral(ctx, r.Project, slug, r.Project.Root)
	if err != nil {
		return nil, err
	}

	err = r.timed("up:"+slug, func() error {
		if err := r.Ctl.ProvisionState(ctx, t); err != nil {
			return err
		}
		if err := r.Ctl.Up(ctx, t, envctl.UpOptions{Timeout: o.Timeout}); err != nil {
			return err
		}
		return r.Ctl.PrepareState(ctx, t)
	})
	if err != nil {
		r.add(finding.New(finding.CodeUnhealthy, "%v", err).WithEnv(slug))
		return nil, nil
	}
	return t, nil
}

func (r *Runner) teardown(ctx context.Context, t *envctl.Target) {
	if t == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	r.step("removing %s", t.Entry.Slug)
	if err := r.Ctl.Down(cleanup, t, envctl.DownOptions{KeepWorktree: true, Force: true}); err != nil {
		r.detail("could not remove %s: %v", t.Entry.Slug, err)
	}
}

func (r *Runner) checkNoPublishedPorts(ctx context.Context, envs ...*envctl.Target) {
	allowed := map[string]bool{}
	for _, s := range r.Project.Config.TCPServices() {
		allowed[s.Name] = true
	}
	for _, t := range envs {
		containers, err := r.Ctl.Runtime.Containers(ctx, engine.Selector{
			All: true,
			Labels: map[string]string{
				meta.LabelManaged: "true",
				meta.LabelProject: t.Entry.Project,
				meta.LabelEnv:     t.Entry.Slug,
			},
		})
		if err != nil {
			continue
		}
		for _, ct := range containers {
			svc := ct.ComposeService()
			for port, bindings := range ct.Ports {
				if len(bindings) == 0 {
					continue
				}
				if allowed[svc] {
					for _, b := range bindings {
						if b.IP != "" && b.IP != "127.0.0.1" && b.IP != "::1" {
							r.add(finding.New(finding.CodePortPublished,
								"service %q publishes %s on %s, not on loopback", svc, port, b.IP).
								WithService(svc).WithEnv(t.Entry.Slug))
						}
					}
					continue
				}
				r.add(finding.New(finding.CodePortPublished,
					"service %q publishes container port %s on the host; a second env would collide with it",
					svc, port).
					WithService(svc).WithEnv(t.Entry.Slug).
					WithEvidence("port", port, "bindings", fmt.Sprint(bindings)))
			}
		}
	}
}

func (r *Runner) checkVolumeIsolation(ctx context.Context, a, b *envctl.Target) {
	cfg := r.Project.Config
	if len(cfg.Stateful) == 0 {
		return
	}
	marker := fmt.Sprintf(".%s-doctor-%d", meta.Name, time.Now().UnixNano())

	for _, st := range cfg.Stateful {
		volA := meta.ComposeVolume(a.ComposeProject(), st.Volume)
		volB := meta.ComposeVolume(b.ComposeProject(), st.Volume)

		res, err := r.Ctl.Runtime.Run(ctx, engine.RunSpec{
			Image:   r.helperImage(),
			Cmd:     []string{"sh", "-c", "touch /vol/" + marker},
			Mounts:  []engine.Mount{{Volume: volA, Target: "/vol"}},
			User:    "0:0",
			Timeout: 2 * time.Minute,
		})
		if err != nil || res.ExitCode != 0 {
			r.detail("could not write a marker into %s: %v", volA, err)
			continue
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, _ = r.Ctl.Runtime.Run(cleanup, engine.RunSpec{
				Image:  r.helperImage(),
				Cmd:    []string{"sh", "-c", "rm -f /vol/" + marker},
				Mounts: []engine.Mount{{Volume: volA, Target: "/vol"}},
				User:   "0:0", Timeout: time.Minute,
			})
		}()

		check, err := r.Ctl.Runtime.Run(ctx, engine.RunSpec{
			Image:   r.helperImage(),
			Cmd:     []string{"sh", "-c", "test -e /vol/" + marker + " && echo SHARED || echo ISOLATED"},
			Mounts:  []engine.Mount{{Volume: volB, Target: "/vol", ReadOnly: true}},
			User:    "0:0",
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			r.detail("could not read %s: %v", volB, err)
			continue
		}
		if strings.Contains(check.Output, "SHARED") {
			r.add(finding.New(finding.CodeStateShared,
				"volume %q of service %s is the same volume in both envs, so their databases are not isolated",
				st.Volume, st.Service).
				WithService(st.Service).
				WithEvidence("volume_a", volA, "volume_b", volB))
		} else {
			r.detail("%s: %s and %s are separate volumes", st.Service, volA, volB)
		}
	}
}

var ipv4 = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)

func (r *Runner) checkNetworkIsolation(ctx context.Context, a *envctl.Target) {
	networks := a.Entry.Networks
	if len(networks) == 0 {
		return
	}
	own, err := r.containerIPs(ctx, a.Entry.Project, a.Entry.Slug)
	if err != nil {
		r.detail("could not list env IPs: %v", err)
		return
	}
	all, err := r.containerIPs(ctx, a.Entry.Project, "")
	if err != nil {
		return
	}

	services := make([]string, 0, len(a.Config.Service))
	for _, s := range a.Config.Service {
		services = append(services, s.Name)
	}
	sort.Strings(services)
	if len(services) == 0 {
		return
	}

	script := "for n in " + strings.Join(services, " ") + "; do echo \"== $n\"; nslookup \"$n\" 2>/dev/null; done"
	res, err := r.Ctl.Runtime.Run(ctx, engine.RunSpec{
		Image:   r.helperImage(),
		Cmd:     []string{"sh", "-c", script},
		Network: networks[0],
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		r.detail("could not resolve service names inside %s: %v", networks[0], err)
		return
	}

	for service, addrs := range parseLookup(res.Output) {
		for _, ip := range addrs {
			if own[ip] != "" {
				continue
			}
			if other := all[ip]; other != "" {
				r.add(finding.New(finding.CodeCrossEnvDNS,
					"inside env %s the name %q resolves to %s, which is a container of another env",
					a.Entry.Slug, service, other).
					WithService(service).WithEnv(a.Entry.Slug).
					WithEvidence("name", service, "address", ip, "container", other))
			}
		}
	}
	r.detail("service names resolve only within the env")
}

func parseLookup(out string) map[string][]string {
	result := map[string][]string{}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "== "); ok {
			current = name
			continue
		}
		if current == "" || !strings.HasPrefix(line, "Address") {
			continue
		}
		for _, m := range ipv4.FindAllStringSubmatch(line, -1) {
			ip := m[1]

			if strings.HasPrefix(ip, "127.") {
				continue
			}
			result[current] = append(result[current], ip)
		}
	}
	return result
}

func (r *Runner) containerIPs(ctx context.Context, project, env string) (map[string]string, error) {
	labels := map[string]string{meta.LabelManaged: "true", meta.LabelProject: project}
	if env != "" {
		labels[meta.LabelEnv] = env
	}
	containers, err := r.Ctl.Runtime.Containers(ctx, engine.Selector{All: true, Labels: labels})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ct := range containers {
		for _, ep := range ct.Networks {
			if ep.IPAddress != "" {
				out[ep.IPAddress] = ct.Name
			}
		}
	}
	return out, nil
}

func (r *Runner) checkRoutes(ctx context.Context, t *envctl.Target) {
	hosts := t.Hosts()
	port := t.Config.Router.Port
	for _, s := range t.Config.RoutableServices() {
		host := hosts.Host(s.Name)
		res, err := probeHTTP(ctx, port, host, "/", 20*time.Second)
		switch {
		case err != nil:
			r.add(finding.New(finding.CodeRouteFailed,
				"%s did not answer through the router: %v", hosts.URL(s.Name), err).
				WithService(s.Name).WithEnv(t.Entry.Slug))
		case res.RouterError:
			r.add(finding.New(finding.CodeRouteFailed,
				"the router could not reach %s at %s:%d (it answered %d itself)",
				s.Name, s.Name, s.Port, res.Status).
				WithService(s.Name).WithEnv(t.Entry.Slug).
				WithEvidence("url", hosts.URL(s.Name), "status", res.Status, "body", excerpt(res.Body)))
		case res.Status >= 500:
			r.add(finding.New(finding.CodeRouteFailed,
				"%s answered %d", hosts.URL(s.Name), res.Status).
				WithService(s.Name).WithEnv(t.Entry.Slug).
				WithEvidence("status", res.Status, "body", excerpt(res.Body)).
				WithHint("The router reached the service, so routing works; the service itself returned an error."))
		default:
			r.detail("%s → %d", hosts.URL(s.Name), res.Status)
		}
	}
}

func (r *Runner) checkWebSockets(ctx context.Context, t *envctl.Target) {
	hosts := t.Hosts()
	for _, s := range t.Config.RoutableServices() {
		if s.WebsocketPath == "" {
			continue
		}
		host := hosts.Host(s.Name)
		if err := probeWebSocket(ctx, t.Config.Router.Port, host, s.WebsocketPath, s.WebsocketProtocol, 20*time.Second); err != nil {
			r.add(finding.New(finding.CodeWebsocket,
				"a WebSocket upgrade to %s%s failed: %v", hosts.URL(s.Name), s.WebsocketPath, err).
				WithService(s.Name).WithEnv(t.Entry.Slug).
				WithEvidence("url", hosts.URL(s.Name)+s.WebsocketPath))
			continue
		}
		r.detail("websocket %s%s upgraded", hosts.URL(s.Name), s.WebsocketPath)
	}
}

func (r *Runner) checkUserHTTP(ctx context.Context, t *envctl.Target) {
	hosts := t.Hosts()
	for _, h := range t.Config.Doctor.HTTP {
		svc, ok := t.Config.ServiceByName(h.Service)
		if !ok || !svc.Routable() {
			continue
		}
		want := h.ExpectStatus
		if want == 0 {
			want = 200
		}
		url := hosts.URL(h.Service) + h.Path
		res, err := probeHTTP(ctx, t.Config.Router.Port, hosts.Host(h.Service), h.Path, 30*time.Second)
		if err != nil {
			r.add(finding.New(finding.CodeHTTPCheck, "%s: %v", url, err).
				WithService(h.Service).WithEnv(t.Entry.Slug))
			continue
		}
		if res.Status != want {
			r.add(finding.New(finding.CodeHTTPCheck,
				"%s answered %d, expected %d", url, res.Status, want).
				WithService(h.Service).WithEnv(t.Entry.Slug).
				WithEvidence("url", url, "status", res.Status, "body", excerpt(res.Body)))
			continue
		}
		if h.ExpectBodyContains != "" {
			want, err := t.Config.Render(h.ExpectBodyContains, t.Identity(), h.Service)
			if err != nil {
				r.add(finding.New(finding.CodeConfigInvalid,
					"doctor.http expect_body_contains: %v", err).WithService(h.Service))
				continue
			}
			if !strings.Contains(res.Body, want) {
				r.add(finding.New(finding.CodeHTTPCheck,
					"%s did not contain %q", url, want).
					WithService(h.Service).WithEnv(t.Entry.Slug).
					WithEvidence("url", url, "expected", want, "body", excerpt(res.Body)).
					WithHint("This check exists to prove that env %s reached its own services. "+
						"If the body names a different env, the frontend is talking to a neighbour's backend.",
						t.Entry.Slug))
				continue
			}
		}
		r.detail("%s → %d as expected", url, res.Status)
	}
}

func (r *Runner) checkPrepareIdempotent(ctx context.Context, t *envctl.Target) {
	if len(t.Config.Stateful) == 0 {
		return
	}
	r.step("running prepare a second time to check it is idempotent")
	err := r.timed("prepare:second-run", func() error {
		results, err := r.Ctl.RunPrepare(ctx, t)
		if err != nil {
			return err
		}
		for _, res := range results {
			if res.ExitCode != 0 {
				r.add(finding.New(finding.CodePrepareNotIdem,
					"running prepare twice failed for %s (exit %d)", res.Service, res.ExitCode).
					WithService(res.Service).WithEnv(t.Entry.Slug).
					WithEvidence("output", engine.LastLines(res.Output, 20)))
			}
		}
		return nil
	})
	if err != nil {
		r.add(finding.New(finding.CodePrepareNotIdem,
			"running prepare a second time failed: %v", err).WithEnv(t.Entry.Slug))
	}
}

func (r *Runner) checkHostnameResolution(t *envctl.Target) {
	host := t.Hosts().Base()
	addrs, err := net.LookupHost(host)
	if err == nil {
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && ip.IsLoopback() {
				r.detail("%s resolves to %s", host, a)
				return
			}
		}
	}
	r.add(finding.New(finding.CodeLocalhostResolve,
		"the system resolver does not send %s to the loopback address", host).
		WithEvidence("host", host, "error", errString(err), "addresses", addrs).
		WithHint("Chromium and Firefox resolve *.%s themselves, so browsers work. "+
			"For curl and other CLI clients, point a wildcard DNS name at 127.0.0.1 and set "+
			"router.base_domain in %s to it.", t.Config.Router.BaseDomain, config.FileName))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func excerpt(body string) string {
	body = strings.TrimSpace(body)
	if len(body) > 300 {
		return body[:300] + "…"
	}
	return body
}
