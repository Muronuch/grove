package envctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Muronuch/grove/internal/config"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/project"
	"github.com/Muronuch/grove/internal/registry"
)

type CreateOptions struct {
	Branch        string
	From          string
	Headless      bool
	KeepOnFailure bool
	SkipPrepare   bool
	Timeout       time.Duration
}

func (c *Controller) Create(ctx context.Context, p *project.Project, o CreateOptions) (*Target, error) {
	if o.Branch == "" {
		return nil, errors.New("a branch name is required")
	}
	git := p.Git()

	target, err := c.allocate(ctx, p, o)
	if err != nil {
		return nil, err
	}

	var undo []func()
	rollback := func() {
		if o.KeepOnFailure {
			c.step("leaving the partial env in place (--keep-on-failure)")
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	fail := func(err error) (*Target, error) {
		_ = c.setState(ctx, target, registry.StateFailed, err.Error())
		rollback()
		return nil, err
	}

	lock, err := c.Store.EnvLock(target.Entry.Project, target.Entry.Slug)
	if err != nil {
		return fail(err)
	}
	if err := lock.Acquire(ctx, registry.LockTimeout); err != nil {
		return fail(err)
	}
	defer lock.Release()

	undo = append(undo, func() {
		_ = c.Store.Update(context.WithoutCancel(ctx), func(r *registry.Registry) error {
			if ps, ok := r.LookupProject(target.Entry.Project); ok {
				delete(ps.Envs, target.Entry.Slug)
				r.Touch()
			}
			return nil
		})
	})

	worktree := target.Entry.Worktree
	createBranch := !git.BranchExists(ctx, o.Branch)
	from := o.From
	if from == "" {
		from = p.DefaultBranch(ctx)
	}
	if createBranch && !git.RefExists(ctx, from) {
		return fail(fmt.Errorf("cannot start branch %q: %q is not a commit in this repository", o.Branch, from))
	}
	c.step("creating worktree %s", worktree)
	if err := p.EnsureWorktreeBase(); err != nil {
		return fail(err)
	}
	if err := git.WorktreeAdd(ctx, worktree, o.Branch, from, createBranch); err != nil {
		return fail(worktreeError(err, o.Branch, worktree))
	}
	undo = append(undo, func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_ = git.WorktreeRemove(cleanup, worktree, true)
		if createBranch {
			_ = git.DeleteBranch(cleanup, o.Branch)
		}
	})

	copied, err := p.CopyFiles(worktree, target.Identity())
	if err != nil {
		return fail(err)
	}
	if len(copied) > 0 {
		c.detail("copied %s", strings.Join(copied, ", "))
	}
	if err := c.runHooks(ctx, target, p.Config.Hooks.PostCreate, "post_create"); err != nil {
		return fail(err)
	}

	if err := c.ProvisionState(ctx, target); err != nil {
		return fail(err)
	}

	undo = append(undo, func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if len(target.Entry.Networks) > 0 {
			_ = c.Router.Detach(cleanup, target.Entry.Networks)
		}
		if err := c.teardownStack(cleanup, target); err != nil {
			c.step("could not remove the partial stack: %v", err)
		}
	})
	if err := c.upLocked(ctx, target, UpOptions{
		Headless:    o.Headless,
		SkipPrepare: o.SkipPrepare,
		Timeout:     o.Timeout,
	}); err != nil {
		return fail(err)
	}
	if !o.SkipPrepare {
		if err := c.PrepareState(ctx, target); err != nil {
			return fail(err)
		}
	}
	if err := c.runHooks(ctx, target, p.Config.Hooks.PostUp, "post_up"); err != nil {
		return fail(err)
	}

	if err := c.setState(ctx, target, registry.StateRunning, ""); err != nil {
		return fail(err)
	}
	if err := c.SyncRouter(ctx, p); err != nil {
		return fail(err)
	}
	return target, nil
}

func (c *Controller) allocate(ctx context.Context, p *project.Project, o CreateOptions) (*Target, error) {
	var target *Target
	err := c.Store.Update(ctx, func(r *registry.Registry) error {
		ps := r.Project(p.Name(), p.Root)

		for _, e := range ps.List() {
			if e.Branch == o.Branch {
				return fmt.Errorf("branch %q already has env %q (use `%s up %s`, or `%s down %s` first)",
					o.Branch, e.Slug, meta.Name, e.Slug, meta.Name, e.Slug)
			}
		}

		slug := project.SlugFor(o.Branch, func(s string) bool { return ps.SlugTaken(s, o.Branch) })
		if !config.NameRE.MatchString(slug) {
			return fmt.Errorf("branch %q produces the invalid env name %q", o.Branch, slug)
		}
		slot := project.AllocateSlot(ps.UsedSlots(), p.Config.Project.MaxSlots)
		if slot == 0 {
			return fmt.Errorf("%w: all %d slots of project %s are taken (raise project.max_slots, or remove an env)",
				registry.ErrNoSlots, p.Config.Project.MaxSlots, p.Name())
		}
		worktree, err := p.WorktreePath(slug)
		if err != nil {
			return err
		}
		if _, err := os.Stat(worktree); err == nil {
			return fmt.Errorf("%s already exists; remove it or pick another branch name", worktree)
		}

		entry := &registry.Env{
			Slug:         slug,
			Project:      p.Name(),
			Branch:       o.Branch,
			Slot:         slot,
			Worktree:     worktree,
			State:        registry.StateCreating,
			Headless:     o.Headless,
			OwnsWorktree: true,
			CreatedAt:    time.Now().UTC(),
			LastActivity: time.Now().UTC(),
		}
		ps.Envs[slug] = entry
		r.Touch()
		target = &Target{Project: p, Entry: entry, Config: p.Config}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}

func worktreeError(err error, branch, path string) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already used by worktree"):
		return fmt.Errorf("branch %q is already checked out in another worktree; "+
			"each env owns its branch exclusively:\n%s", branch, msg)
	case strings.Contains(msg, "already exists"):
		return fmt.Errorf("cannot create the worktree at %s: %s", path, msg)
	default:
		return err
	}
}

func (c *Controller) runHooks(ctx context.Context, t *Target, commands []string, phase string) error {
	if len(commands) == 0 {
		return nil
	}
	if err := c.ensureHooksTrusted(ctx, t, phase); err != nil {
		return err
	}
	env := []string{
		meta.EnvVarName("ENV") + "=" + t.Entry.Slug,
		meta.EnvVarName("PROJECT") + "=" + t.Entry.Project,
		meta.EnvVarName("SLOT") + "=" + strconv.Itoa(t.Entry.Slot),
		meta.EnvVarName("WORKTREE") + "=" + t.Entry.Worktree,
	}
	for name, url := range t.Hosts().URLs() {
		env = append(env, meta.EnvVarName("URL", name)+"="+url)
	}

	var out io.Writer = io.Discard
	if c.Progress != nil {
		out = c.Progress
	}
	for _, cmd := range commands {
		c.step("%s: %s", phase, cmd)
		if err := project.RunHook(ctx, t.Entry.Worktree, cmd, env, out); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) ensureHooksTrusted(ctx context.Context, t *Target, phase string) error {
	all := t.Config.Hooks.All()
	reg, err := c.Store.Read()
	if err != nil {
		return err
	}
	ps, ok := reg.LookupProject(t.Entry.Project)
	if ok && ps.HooksTrusted(all) {
		return nil
	}
	if !c.Trust {
		if c.Confirm == nil {
			return fmt.Errorf("%s declares host hooks that have not been approved:\n%s\n\n"+
				"Run an interactive `%s up` once to approve them, or pass --trust",
				config.FileName, indent(strings.Join(all, "\n"), "  "), meta.Name)
		}
		prompt := fmt.Sprintf("%s wants to run these commands on your machine (%s of %s):",
			config.FileName, phase, t.Entry.Project)
		ok, err := c.Confirm(prompt, all)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("host hooks were not approved; pass --trust to run them without asking")
		}
	}
	return c.Store.Update(ctx, func(r *registry.Registry) error {
		r.TrustHooks(r.Project(t.Entry.Project, t.Project.Root), all)
		return nil
	})
}

func (c *Controller) removeWorktree(ctx context.Context, t *Target, force bool) error {
	git := t.Project.Git()
	c.step("removing worktree %s", t.Entry.Worktree)
	err := git.WorktreeRemove(ctx, t.Entry.Worktree, force)
	if err == nil {
		return nil
	}
	if !force && strings.Contains(err.Error(), "contains modified or untracked files") {
		return fmt.Errorf("worktree %s has uncommitted changes; commit them, or pass --force to discard them\n%v",
			t.Entry.Worktree, err)
	}

	c.detail("git worktree remove failed (%v); pruning", err)
	if err := os.RemoveAll(t.Entry.Worktree); err != nil {
		return fmt.Errorf("remove %s: %w", t.Entry.Worktree, err)
	}
	return git.WorktreePrune(ctx)
}

func (c *Controller) Ephemeral(ctx context.Context, p *project.Project, slug, worktree string) (*Target, error) {
	if !config.NameRE.MatchString(slug) {
		return nil, fmt.Errorf("%q is not a valid env name", slug)
	}
	if worktree == "" {
		worktree = p.Root
	}
	var target *Target
	err := c.Store.Update(ctx, func(r *registry.Registry) error {
		ps := r.Project(p.Name(), p.Root)
		if old, ok := ps.Envs[slug]; ok {
			target = &Target{Project: p, Entry: old, Config: p.Config}
			return nil
		}
		slot := project.AllocateSlot(ps.UsedSlots(), p.Config.Project.MaxSlots)
		if slot == 0 {
			return fmt.Errorf("%w: all %d slots of project %s are taken; remove an env and try again",
				registry.ErrNoSlots, p.Config.Project.MaxSlots, p.Name())
		}
		entry := &registry.Env{
			Slug: slug, Project: p.Name(), Branch: "", Slot: slot,
			Worktree: worktree, State: registry.StateCreating,
			CreatedAt: time.Now().UTC(), LastActivity: time.Now().UTC(),
		}
		ps.Envs[slug] = entry
		r.Touch()
		target = &Target{Project: p, Entry: entry, Config: p.Config}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}

func (c *Controller) Adopt(ctx context.Context, p *project.Project, worktree, branch string, headless bool) (*Target, error) {
	var target *Target
	err := c.Store.Update(ctx, func(r *registry.Registry) error {
		ps := r.Project(p.Name(), p.Root)
		for _, e := range ps.List() {
			if e.Worktree == worktree {
				target = &Target{Project: p, Entry: e, Config: p.Config}
				return nil
			}
		}
		slug := project.SlugFor(branch, func(s string) bool { return ps.SlugTaken(s, branch) })
		slot := project.AllocateSlot(ps.UsedSlots(), p.Config.Project.MaxSlots)
		if slot == 0 {
			return fmt.Errorf("%w: all %d slots of project %s are taken",
				registry.ErrNoSlots, p.Config.Project.MaxSlots, p.Name())
		}
		entry := &registry.Env{
			Slug:         slug,
			Project:      p.Name(),
			Branch:       branch,
			Slot:         slot,
			Worktree:     worktree,
			State:        registry.StateCreating,
			Headless:     headless,
			CreatedAt:    time.Now().UTC(),
			LastActivity: time.Now().UTC(),
		}
		ps.Envs[slug] = entry
		r.Touch()
		target = &Target{Project: p, Entry: entry, Config: p.Config}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}
