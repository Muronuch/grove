package routerctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Muronuch/grove/internal/engine"
	"github.com/Muronuch/grove/internal/meta"
)

const ModulePath = "github.com/Muronuch/grove"

const PublishedImage = "ghcr.io/muronuch/" + meta.Name + "-router"

const LocalImage = meta.Name + "-router"

var ImageEnv = meta.EnvVarName("ROUTER", "IMAGE")

var SourceEnv = meta.EnvVarName("SOURCE")

const routerDockerfile = `
FROM golang:1-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X ` + ModulePath + `/internal/meta.Version=${VERSION}" \
    -o /out/` + meta.Name + ` ./cmd/` + meta.Name + `

FROM scratch
COPY --from=build /out/` + meta.Name + ` /` + meta.Name + `
EXPOSE 80 9180
ENTRYPOINT ["/` + meta.Name + `", "router-serve"]
`

func ImageRef(configured string) string {
	if v := os.Getenv(ImageEnv); v != "" {
		return v
	}
	if configured != "" {
		return configured
	}
	return PublishedImage + ":" + meta.VersionString()
}

func EnsureImage(ctx context.Context, rt engine.Runtime, ref string, out io.Writer) error {
	if _, err := rt.ImageID(ctx, ref); err == nil {
		return nil
	}
	src, srcErr := FindSource()
	if srcErr != nil {
		return fmt.Errorf("router image %s is not available: %w\n\n"+
			"Either set %s to an image you can pull, or point %s at a %s source tree\nso that `%s router build` can build it",
			ref, srcErr, ImageEnv, SourceEnv, meta.Name, meta.Name)
	}
	if out != nil {
		fmt.Fprintf(out, "building the router image %s from %s\n", ref, src)
	}
	return BuildImage(ctx, rt, src, ref, out)
}

func BuildImage(ctx context.Context, rt engine.Runtime, source, ref string, out io.Writer) error {
	return rt.Build(ctx, engine.BuildSpec{
		ContextDir:     source,
		Dockerfile:     "Dockerfile." + meta.Name + "-router",
		DockerfileBody: routerDockerfile,
		Tags:           []string{ref},
		BuildArgs:      map[string]string{"VERSION": meta.VersionString()},
		Exclude:        []string{".git", "testdata", "dist", "docs", ".github"},
	}, out)
}

func FindSource() (string, error) {
	var candidates []string
	if v := os.Getenv(SourceEnv); v != "" {
		candidates = append(candidates, v)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		candidates = append(candidates, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, c := range candidates {
		if root, ok := walkUpForModule(c); ok {
			return root, nil
		}
	}
	return "", errors.New("no " + meta.Name + " source tree found (set " + SourceEnv + ")")
}

func walkUpForModule(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		if isModuleRoot(abs) {
			return abs, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false
		}
		abs = parent
	}
}

func isModuleRoot(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "module "+ModulePath {
			return true
		}
	}
	return false
}
