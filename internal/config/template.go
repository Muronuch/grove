package config

import (
	"fmt"
	"sort"
	"strings"
	"text/template"
)

type TemplateData struct {
	Env     string
	Slug    string
	Slot    int
	Project string
	Repo    string
	Service string
	Domain  string
}

func (c *Config) funcs(h HostSet) template.FuncMap {
	known := func(service string) error {
		s, ok := c.ServiceByName(service)
		if !ok {
			return fmt.Errorf("service %q is not declared as a [[service]] in %s (declared: %s)",
				service, FileName, strings.Join(c.serviceNames(), ", "))
		}
		if !s.Routable() {
			return fmt.Errorf("service %q has no HTTP URL (it is tcp = true)", service)
		}
		return nil
	}
	return template.FuncMap{
		"url": func(service string) (string, error) {
			if err := known(service); err != nil {
				return "", err
			}
			return h.URL(service), nil
		},
		"slotUrl": func(service string) (string, error) {
			if err := known(service); err != nil {
				return "", err
			}
			return h.SlotURL(service), nil
		},
		"host": func(service string) (string, error) {
			if err := known(service); err != nil {
				return "", err
			}
			return h.Host(service), nil
		},
		"slotHost": func(service string) (string, error) {
			if err := known(service); err != nil {
				return "", err
			}
			return h.SlotHost(service), nil
		},
	}
}

func (c *Config) serviceNames() []string {
	out := make([]string, 0, len(c.Service))
	for _, s := range c.Service {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func (c *Config) compileTemplate(name, text string) (*template.Template, error) {
	h := c.Hosts(EnvIdentity{Project: c.Project.Name, Slug: "validate", Slot: 1, Repo: c.Project.Name})
	t, err := template.New(name).Option("missingkey=error").Funcs(c.funcs(h)).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("invalid template: %v", err)
	}

	var sb strings.Builder
	if err := t.Execute(&sb, c.templateData(EnvIdentity{
		Project: c.Project.Name, Slug: "validate", Slot: 1, Repo: c.Project.Name,
	}, "")); err != nil {
		return nil, fmt.Errorf("invalid template: %v", unwrapTemplateError(err))
	}
	return t, nil
}

func (c *Config) templateData(id EnvIdentity, service string) TemplateData {
	return TemplateData{
		Env:     id.Slug,
		Slug:    id.Slug,
		Slot:    id.Slot,
		Project: id.Project,
		Repo:    id.Repo,
		Service: service,
		Domain:  c.Router.BaseDomain,
	}
}

func (c *Config) Render(text string, id EnvIdentity, service string) (string, error) {
	h := c.Hosts(id)
	t, err := template.New("t").Option("missingkey=error").Funcs(c.funcs(h)).Parse(text)
	if err != nil {
		return "", fmt.Errorf("invalid template %q: %v", text, err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, c.templateData(id, service)); err != nil {
		return "", fmt.Errorf("template %q: %v", text, unwrapTemplateError(err))
	}
	return sb.String(), nil
}

func (c *Config) RenderEnvFor(service string, id EnvIdentity) (map[string]string, error) {
	vars, ok := c.Env[service]
	if !ok || len(vars) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(vars))
	for _, k := range keys {
		v, err := c.Render(vars[k], id, service)
		if err != nil {
			return nil, fmt.Errorf("env.%s.%s: %w", service, k, err)
		}
		out[k] = v
	}
	return out, nil
}

func (c *Config) RenderWorktreeDir(repo string) (string, error) {
	t, err := template.New("worktree.dir").Option("missingkey=error").Parse(c.Worktree.Dir)
	if err != nil {
		return "", fmt.Errorf("worktree.dir: invalid template: %v", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, TemplateData{Project: c.Project.Name, Repo: repo}); err != nil {
		return "", fmt.Errorf("worktree.dir: %v", unwrapTemplateError(err))
	}
	return sb.String(), nil
}

func unwrapTemplateError(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		if j := strings.Index(msg, "error calling "); j >= 0 {
			if k := strings.Index(msg[j:], ": "); k >= 0 {
				return msg[j+k+2:]
			}
		}
		_ = i
	}
	return msg
}
