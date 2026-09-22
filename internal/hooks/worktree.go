package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/envctl"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/registry"
)

type CreateInput struct {
	Name          string `json:"name"`
	WorktreeName  string `json:"worktree_name"`
	Branch        string `json:"branch"`
	BranchName    string `json:"branch_name"`
	WorktreePath  string `json:"worktree_path"`
	Path          string `json:"path"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
}

func (i CreateInput) branch() string {
	for _, v := range []string{i.Name, i.WorktreeName, i.Branch, i.BranchName} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

type RemoveInput struct {
	WorktreePath string `json:"worktree_path"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	CWD          string `json:"cwd"`
}

func (i RemoveInput) path() string {
	for _, v := range []string{i.WorktreePath, i.Path} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func Create(ctx context.Context, ctl *envctl.Controller, cwd string, stdin io.Reader, stdout, progress io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("read the hook payload: %w", err)
	}
	var in CreateInput
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("the hook payload is not JSON: %w\n%s", err, excerpt(raw))
		}
	}
	branch := in.branch()
	if branch == "" {
		return fmt.Errorf("the hook payload has no worktree name; got:\n%s", excerpt(raw))
	}
	if in.CWD != "" {
		cwd = in.CWD
	}

	p, err := ctl.FindProject(ctx, cwd)
	if err != nil {
		return err
	}

	if existing, ok := findByBranch(ctx, ctl, p.Name(), branch); ok {
		fmt.Fprintf(progress, "%s: reusing env %s\n", meta.Name, existing.Slug)
		if err := ctl.Up(ctx, &envctl.Target{Project: p, Entry: existing, Config: p.Config},
			envctl.UpOptions{Headless: true}); err != nil {
			return err
		}
		fmt.Fprintln(stdout, existing.Worktree)
		return nil
	}

	fmt.Fprintf(progress, "%s: creating an environment for %s\n", meta.Name, branch)
	t, err := ctl.Create(ctx, p, envctl.CreateOptions{
		Branch:   branch,
		Headless: true,
	})
	if err != nil {
		return err
	}

	for name, url := range t.Hosts().URLs() {
		fmt.Fprintf(progress, "  %s %s\n", name, url)
	}

	fmt.Fprintln(stdout, t.Entry.Worktree)
	return nil
}

func Remove(ctx context.Context, ctl *envctl.Controller, cwd string, stdin io.Reader, progress io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("read the hook payload: %w", err)
	}
	var in RemoveInput
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("the hook payload is not JSON: %w\n%s", err, excerpt(raw))
		}
	}
	path := in.path()
	if path == "" && in.Name == "" {
		fmt.Fprintf(progress, "%s: the hook payload named no worktree; nothing to do\n", meta.Name)
		return nil
	}
	if in.CWD != "" {
		cwd = in.CWD
	}

	reg, err := ctl.Store.Read()
	if err != nil {
		return err
	}
	var entry *registry.Env
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			entry, _ = reg.FindByWorktree(abs)
		}
	}
	if entry == nil && in.Name != "" {
		if p, err := ctl.FindProject(ctx, cwd); err == nil {
			if e, ok := findByBranch(ctx, ctl, p.Name(), in.Name); ok {
				entry = e
			} else if e, ok := findBySlug(reg, p.Name(), project.Slugify(in.Name)); ok {
				entry = e
			}
		}
	}
	if entry == nil {
		fmt.Fprintf(progress, "%s: no env for %s; nothing to do\n", meta.Name, firstNonEmpty(path, in.Name))
		return nil
	}

	p, err := ctl.FindProject(ctx, entry.Worktree)
	if err != nil {
		if ps, ok := reg.LookupProject(entry.Project); ok && ps.Root != "" {
			p, err = ctl.FindProject(ctx, ps.Root)
		}
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(progress, "%s: removing env %s\n", meta.Name, entry.Slug)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err = ctl.Down(ctx, &envctl.Target{Project: p, Entry: entry, Config: p.Config}, envctl.DownOptions{
		KeepWorktree: true,
		Force:        true,
	})
	if err != nil && !errors.Is(err, registry.ErrEnvNotFound) {
		return err
	}
	return nil
}

func findByBranch(ctx context.Context, ctl *envctl.Controller, projectName, branch string) (*registry.Env, bool) {
	reg, err := ctl.Store.Read()
	if err != nil {
		return nil, false
	}
	ps, ok := reg.LookupProject(projectName)
	if !ok {
		return nil, false
	}
	for _, e := range ps.List() {
		if e.Branch == branch {
			return e, true
		}
	}
	return nil, false
}

func findBySlug(reg *registry.Registry, projectName, slug string) (*registry.Env, bool) {
	ps, ok := reg.LookupProject(projectName)
	if !ok {
		return nil, false
	}
	e, ok := ps.Envs[slug]
	return e, ok
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return "(unnamed)"
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
