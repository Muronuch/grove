package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrNotARepo = errors.New("not a git repository")

type Git struct {
	Dir string
}

func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

func (g Git) Lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := g.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	raw := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func (g Git) ok(ctx context.Context, args ...string) bool {
	_, err := g.Run(ctx, args...)
	return err == nil
}

func (g Git) CommonDir(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotARepo, err)
	}
	return filepath.Clean(out), nil
}

func (g Git) TopLevel(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotARepo, err)
	}
	return filepath.Clean(out), nil
}

func (g Git) CurrentBranch(ctx context.Context) (string, error) {
	return g.Run(ctx, "branch", "--show-current")
}

func (g Git) BranchExists(ctx context.Context, branch string) bool {
	return g.ok(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
}

func (g Git) RefExists(ctx context.Context, ref string) bool {
	return g.ok(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

func (g Git) Resolve(ctx context.Context, ref string) (string, error) {
	return g.Run(ctx, "rev-parse", "--verify", ref+"^{commit}")
}

func (g Git) DefaultBranch(ctx context.Context, configured string) string {
	if configured != "" && g.BranchExists(ctx, configured) {
		return configured
	}
	if out, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(out, "origin/"); b != "" && b != out {
			return b
		}
	}
	for _, cand := range []string{configured, "main", "master", "trunk", "develop"} {
		if cand != "" && g.BranchExists(ctx, cand) {
			return cand
		}
	}
	if configured != "" {
		return configured
	}
	return "main"
}

type Worktree struct {
	Path     string
	Head     string
	Branch   string
	Bare     bool
	Detached bool
	Locked   bool
	Prunable bool
}

func (g Git) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := g.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var (
		list []Worktree
		cur  *Worktree
	)
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &Worktree{Path: filepath.Clean(val)}
		case "HEAD":
			if cur != nil {
				cur.Head = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "locked":
			if cur != nil {
				cur.Locked = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	flush()
	return list, nil
}

func (g Git) WorktreeAdd(ctx context.Context, path, branch, ref string, createBranch bool) error {
	args := []string{"worktree", "add"}
	if createBranch {
		args = append(args, "-b", branch, path, ref)
	} else {
		args = append(args, path, branch)
	}
	_, err := g.Run(ctx, args...)
	return err
}

func (g Git) WorktreeRemove(ctx context.Context, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := g.Run(ctx, args...)
	return err
}

func (g Git) WorktreePrune(ctx context.Context) error {
	_, err := g.Run(ctx, "worktree", "prune")
	return err
}

func (g Git) DeleteBranch(ctx context.Context, branch string) error {
	_, err := g.Run(ctx, "branch", "-D", branch)
	return err
}

func (g Git) TrackedFiles(ctx context.Context, dir string) ([]string, error) {
	args := []string{"ls-files", "-z"}
	if dir != "" && dir != "." {
		args = append(args, "--", dir)
	}
	out, err := g.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}
