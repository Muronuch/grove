package transform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/Muronuch/grove/internal/config"
)

type LoadOptions struct {
	WorkingDir string
	Files      []string
	Profiles   []string
	EnvFiles   []string
	Name       string
	Env        []string
}

func Load(ctx context.Context, o LoadOptions) (*types.Project, error) {
	wd, err := filepath.Abs(o.WorkingDir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(wd); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("working directory %s does not exist", wd)
	}

	files := make([]string, 0, len(o.Files))
	for _, f := range o.Files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(wd, f)
		}
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("compose.files: %s: %w", f, err)
		}
		files = append(files, f)
	}

	fns := []cli.ProjectOptionsFn{
		cli.WithWorkingDirectory(wd),
		cli.WithOsEnv,
	}
	if len(o.EnvFiles) > 0 {
		fns = append(fns, cli.WithEnvFiles(o.EnvFiles...))
	}
	fns = append(fns, cli.WithDotEnv)
	if len(o.Env) > 0 {
		fns = append(fns, cli.WithEnv(o.Env))
	}
	if len(files) == 0 {
		fns = append(fns, cli.WithConfigFileEnv, cli.WithDefaultConfigPath)
	}
	fns = append(fns,
		cli.WithProfiles(o.Profiles),
		cli.WithDefaultProfiles(o.Profiles...),
		cli.WithResolvedPaths(true),
		cli.WithNormalization(true),
		cli.WithInterpolation(true),
	)
	if o.Name != "" {
		fns = append(fns, cli.WithName(o.Name))
	}

	opts, err := cli.NewProjectOptions(files, fns...)
	if err != nil {
		return nil, fmt.Errorf("compose options: %w", err)
	}
	p, err := cli.ProjectFromOptions(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("load compose project from %s: %w", wd, err)
	}
	return p, nil
}

func LoadForProject(ctx context.Context, cfg *config.Config, worktree, composeProjectName string, extraEnv []string) (*types.Project, error) {
	return Load(ctx, LoadOptions{
		WorkingDir: worktree,
		Files:      cfg.Compose.Files,
		Profiles:   cfg.Compose.Profiles,
		EnvFiles:   cfg.Compose.EnvFile,
		Name:       composeProjectName,
		Env:        extraEnv,
	})
}

type PublishedPort struct {
	Service   string
	Target    uint32
	Published string
	Protocol  string
}

func OriginalPorts(p *types.Project) []PublishedPort {
	var out []PublishedPort
	for name, s := range p.Services {
		for _, port := range s.Ports {
			out = append(out, PublishedPort{
				Service:   name,
				Target:    port.Target,
				Published: port.Published,
				Protocol:  port.Protocol,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Target < out[j].Target
	})
	return out
}
