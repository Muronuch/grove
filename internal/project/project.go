package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Muronuch/grove/internal/config"
)

type Project struct {
	Config    *config.Config
	Root      string
	CommonDir string
	From      string
}

func (p *Project) Name() string { return p.Config.Project.Name }

func (p *Project) Git() Git { return Git{Dir: p.Root} }

func (p *Project) GitIn(dir string) Git { return Git{Dir: dir} }

func (p *Project) RepoName() string { return filepath.Base(p.Root) }

func Find(ctx context.Context, dir string) (*Project, error) {
	path, err := config.Find(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return FromConfig(ctx, cfg, dir)
}

func FromConfig(ctx context.Context, cfg *config.Config, from string) (*Project, error) {
	abs, err := filepath.Abs(from)
	if err != nil {
		abs = from
	}
	g := Git{Dir: cfg.Root}
	common, err := g.CommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository: %w", cfg.Root, err)
	}
	root := filepath.Dir(common)
	if filepath.Base(common) != ".git" {
		if tl, err := g.TopLevel(ctx); err == nil {
			root = tl
		}
	}

	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		root = cfg.Root
	}
	return &Project{Config: cfg, Root: root, CommonDir: common, From: abs}, nil
}

func (p *Project) WorktreeBase() (string, error) {
	rendered, err := p.Config.RenderWorktreeDir(p.RepoName())
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(rendered) {
		rendered = filepath.Join(p.Root, rendered)
	}
	return filepath.Clean(rendered), nil
}

func (p *Project) WorktreePath(slug string) (string, error) {
	base, err := p.WorktreeBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, slug), nil
}

func (p *Project) DefaultBranch(ctx context.Context) string {
	return p.Git().DefaultBranch(ctx, p.Config.Project.DefaultBranch)
}

func (p *Project) EnsureWorktreeBase() error {
	base, err := p.WorktreeBase()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	if !under(p.Root, base) {
		return nil
	}
	marker := filepath.Join(base, ".gitignore")
	switch _, err := os.Lstat(marker); {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return os.WriteFile(marker, []byte("*\n"), 0o644)
}

func under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (p *Project) CopyFiles(dst string, id config.EnvIdentity) ([]string, error) {
	var copied []string
	for _, rel := range p.Config.Worktree.Copy {
		ok, err := p.place(rel, dst, id, false)
		if err != nil {
			return copied, err
		}
		if ok {
			copied = append(copied, rel)
		}
	}
	for _, rel := range p.Config.Worktree.Template {
		ok, err := p.place(rel, dst, id, true)
		if err != nil {
			return copied, err
		}
		if ok {
			copied = append(copied, rel)
		}
	}
	return copied, nil
}

func (p *Project) place(rel, dst string, id config.EnvIdentity, render bool) (bool, error) {
	key := "worktree.copy"
	if render {
		key = "worktree.template"
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return false, fmt.Errorf("%s: %q must be a relative path inside the repository", key, rel)
	}
	src := filepath.Join(p.Root, filepath.FromSlash(rel))
	st, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", key, rel, err)
	}
	target := filepath.Join(dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	switch {
	case st.IsDir() && render:
		return false, fmt.Errorf("%s %s: is a directory; only files can be rendered", key, rel)
	case st.IsDir():
		if err := copyTree(src, target); err != nil {
			return false, fmt.Errorf("%s %s: %w", key, rel, err)
		}
	case render:
		body, err := os.ReadFile(src)
		if err != nil {
			return false, fmt.Errorf("%s %s: %w", key, rel, err)
		}
		out, err := p.Config.Render(string(body), id, "")
		if err != nil {
			return false, fmt.Errorf("%s %s: %w", key, rel, err)
		}
		if err := os.WriteFile(target, []byte(out), st.Mode().Perm()); err != nil {
			return false, fmt.Errorf("%s %s: %w", key, rel, err)
		}
	default:
		if err := copyFile(src, target, st.Mode().Perm()); err != nil {
			return false, fmt.Errorf("%s %s: %w", key, rel, err)
		}
	}
	return true, nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil
			}
			st, err := os.Stat(real)
			if err != nil || st.IsDir() {
				return nil
			}
			return copyFile(real, target, st.Mode().Perm())
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func RunHook(ctx context.Context, dir, command string, env []string, w io.Writer) error {
	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("sh"); err == nil {
			shell = "sh"
		} else {
			shell, flag = "cmd", "/C"
		}
	}
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hook %q failed in %s: %w", command, dir, err)
	}
	return nil
}
