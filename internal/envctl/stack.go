package envctl

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/finding"
	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/transform"
)

type Stack struct {
	Target  *Target
	Model   *types.Project
	Result  *transform.Result
	Compose *engine.Compose
	Path    string
	Routed  bool
}

type PrepareOptions struct {
	Headless   bool
	Only       []string
	WorkingDir string
	Unrouted   bool
	Slug       string
	Slot       int
}

func (c *Controller) Prepare(ctx context.Context, t *Target, o PrepareOptions) (*Stack, error) {
	slug, slot := t.Entry.Slug, t.Entry.Slot
	if o.Slug != "" {
		slug, slot = o.Slug, o.Slot
	}
	worktree := o.WorkingDir
	if worktree == "" {
		worktree = t.Entry.Worktree
	}
	if worktree == "" {
		worktree = t.Project.Root
	}
	composeProject := meta.ComposeProject(t.Entry.Project, slug)

	ec := transform.Context{
		Project:  t.Entry.Project,
		Slug:     slug,
		Slot:     slot,
		Repo:     t.Project.RepoName(),
		Worktree: worktree,
		Headless: o.Headless,
		Only:     o.Only,
	}

	extraEnv := []string{
		meta.EnvVarName("ENV") + "=" + slug,
		meta.EnvVarName("PROJECT") + "=" + t.Entry.Project,
		meta.EnvVarName("SLOT") + "=" + strconv.Itoa(slot),
	}

	model, err := transform.LoadForProject(ctx, t.Config, worktree, composeProject, extraEnv)
	if err != nil {
		return nil, err
	}
	res, err := transform.Apply(model, t.Config, ec)
	if err != nil {
		return nil, err
	}
	if res.Findings.HasErrors() {
		return nil, fmt.Errorf("this project cannot be isolated yet:\n%s", res.Findings.Errors().Error())
	}
	for _, f := range res.Findings {
		if f.Severity == finding.Warning {
			c.detail("%s: %s", f.Code, f.Message)
		}
	}

	path := filepath.Join(c.Store.EnvDir(t.Entry.Project, slug), "compose.yaml")
	if err := transform.Write(model, path); err != nil {
		return nil, err
	}

	bin, err := engine.ComposeCommand()
	if err != nil {
		return nil, err
	}
	var out, errOut io.Writer
	if c.Verbose {
		out, errOut = c.Progress, c.Progress
	} else {
		out, errOut = io.Discard, io.Discard
	}
	return &Stack{
		Target: t,
		Model:  model,
		Result: res,
		Path:   path,
		Routed: !o.Unrouted,
		Compose: &engine.Compose{
			Bin:        bin,
			File:       path,
			ProjectDir: worktree,
			Project:    composeProject,
			Out:        out,
			Err:        errOut,
		},
	}, nil
}

func (s *Stack) Services() []string { return s.Model.ServiceNames() }

func (s *Stack) RouterNetworks() []string {
	return transform.RouterNetworks(s.Model, s.Target.Config, s.Result)
}

func (s *Stack) Volumes() map[string]string { return s.Result.Volumes }

func (s *Stack) Findings() finding.List { return s.Result.Findings }
