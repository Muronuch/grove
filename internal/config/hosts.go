package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type EnvIdentity struct {
	Project string
	Slug    string
	Slot    int
	Repo    string
}

type HostSet struct {
	cfg *Config
	id  EnvIdentity
}

func (c *Config) Hosts(id EnvIdentity) HostSet { return HostSet{cfg: c, id: id} }

func (h HostSet) Domain() string {
	return h.id.Project + "." + h.cfg.Router.BaseDomain
}

func (h HostSet) Base() string { return h.id.Slug + "." + h.Domain() }

func (h HostSet) SlotBase() string {
	return "s" + strconv.Itoa(h.id.Slot) + "." + h.Domain()
}

func (h HostSet) Host(service string) string {
	if h.isDefault(service) {
		return h.Base()
	}
	return service + "." + h.Base()
}

func (h HostSet) SlotHost(service string) string {
	if h.isDefault(service) {
		return h.SlotBase()
	}
	return service + "." + h.SlotBase()
}

func (h HostSet) URL(service string) string { return h.urlFor(h.Host(service)) }

func (h HostSet) SlotURL(service string) string { return h.urlFor(h.SlotHost(service)) }

func (h HostSet) urlFor(host string) string {
	port := h.cfg.Router.Port
	if port == 0 || port == 80 {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func (h HostSet) URLs() map[string]string {
	out := map[string]string{}
	for _, s := range h.cfg.RoutableServices() {
		out[s.Name] = h.URL(s.Name)
	}
	return out
}

func (h HostSet) AllHosts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(h.Base())
	if h.id.Slot > 0 {
		add(h.SlotBase())
	}
	for _, s := range h.cfg.RoutableServices() {
		add(h.Host(s.Name))
		if h.id.Slot > 0 {
			add(h.SlotHost(s.Name))
		}
	}
	sort.Strings(out)
	return out
}

func (h HostSet) isDefault(service string) bool {
	d, ok := h.cfg.DefaultService()
	return ok && d.Name == service
}

func SplitHost(host, baseDomain string) (service, envKey, project string, ok bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	suffix := "." + strings.ToLower(baseDomain)
	if !strings.HasSuffix(host, suffix) {
		return "", "", "", false
	}
	rest := strings.TrimSuffix(host, suffix)
	parts := strings.Split(rest, ".")
	switch len(parts) {
	case 2:
		return "", parts[0], parts[1], true
	case 3:
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}
