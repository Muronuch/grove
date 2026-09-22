package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func (d *Docker) CreateAndStart(ctx context.Context, spec ContainerSpec) (string, error) {
	if spec.Name != "" {
		if err := d.RemoveContainer(ctx, spec.Name, true); err != nil {
			return "", err
		}
	}
	if err := d.EnsureImage(ctx, spec.Image); err != nil {
		return "", err
	}

	exposed := network.PortSet{}
	bindings := network.PortMap{}
	for _, p := range spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		port, err := network.ParsePort(fmt.Sprintf("%d/%s", p.ContainerPort, proto))
		if err != nil {
			return "", fmt.Errorf("port %d/%s: %w", p.ContainerPort, proto, err)
		}
		exposed[port] = struct{}{}
		host := ""
		if p.HostPort > 0 {
			host = fmt.Sprint(p.HostPort)
		}
		binding := network.PortBinding{HostPort: host}
		if p.HostIP != "" {
			addr, err := parseAddr(p.HostIP)
			if err != nil {
				return "", fmt.Errorf("host ip %q: %w", p.HostIP, err)
			}
			binding.HostIP = addr
		}
		bindings[port] = append(bindings[port], binding)
	}

	mounts := toMounts(spec.Mounts)
	restart := container.RestartPolicy{Name: container.RestartPolicyMode(spec.Restart)}
	if spec.Restart == "" {
		restart.Name = container.RestartPolicyDisabled
	}

	var netCfg *network.NetworkingConfig
	if len(spec.Networks) > 0 {
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
		for _, n := range spec.Networks {
			netCfg.EndpointsConfig[n] = &network.EndpointSettings{}
		}
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:        spec.Image,
			Cmd:          spec.Cmd,
			Env:          spec.Env,
			Labels:       spec.Labels,
			ExposedPorts: exposed,
		},
		HostConfig: &container.HostConfig{
			PortBindings:  bindings,
			Mounts:        mounts,
			RestartPolicy: restart,
		},
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return "", wrap(err, "create container "+spec.Name)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = d.RemoveContainer(context.WithoutCancel(ctx), created.ID, true)
		return "", wrap(err, "start container "+spec.Name)
	}
	return created.ID, nil
}

func (d *Docker) Build(ctx context.Context, spec BuildSpec, w io.Writer) error {
	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	tarball, err := buildContext(spec.ContextDir, dockerfile, spec.DockerfileBody, spec.Exclude)
	if err != nil {
		return err
	}
	args := map[string]*string{}
	for k, v := range spec.BuildArgs {
		args[k] = &v
	}
	res, err := d.cli.ImageBuild(ctx, bytes.NewReader(tarball), client.ImageBuildOptions{
		Tags:       spec.Tags,
		Dockerfile: dockerfile,
		Remove:     true,
		BuildArgs:  args,
	})
	if err != nil {
		return wrap(err, "build image")
	}
	defer res.Body.Close()
	return streamBuild(res.Body, w)
}

func streamBuild(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var msg struct {
			Stream      string `json:"stream"`
			Status      string `json:"status"`
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("build stream: %w", err)
		}
		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return fmt.Errorf("build failed: %s", strings.TrimSpace(msg.ErrorDetail.Message))
		}
		if msg.Error != "" {
			return fmt.Errorf("build failed: %s", strings.TrimSpace(msg.Error))
		}
		if w != nil {
			if msg.Stream != "" {
				io.WriteString(w, msg.Stream)
			} else if msg.Status != "" {
				fmt.Fprintln(w, msg.Status)
			}
		}
	}
}

func buildContext(dir, dockerfile, body string, exclude []string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if body != "" {
		hdr := &tar.Header{Name: dockerfile, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil, err
		}
	}

	if dir != "" {
		err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			for _, ex := range exclude {
				if rel == ex || strings.HasPrefix(rel, ex+"/") {
					if de.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
			}
			if body != "" && rel == dockerfile {
				return nil
			}
			info, err := de.Info()
			if err != nil {
				return err
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if de.IsDir() {
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("pack build context %s: %w", dir, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
