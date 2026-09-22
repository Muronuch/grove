package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/Muronuch/grove/internal/meta"
)

type Docker struct {
	cli *client.Client
}

var _ Runtime = (*Docker)(nil)

func NewDocker(ctx context.Context) (*Docker, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	d := &Docker{cli: cli}
	if err := d.Ping(ctx); err != nil {
		cli.Close()
		return nil, err
	}
	return d, nil
}

func (d *Docker) Close() error { return d.cli.Close() }

func (d *Docker) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := d.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("%w: %v\n\nStart Docker Desktop, OrbStack, Colima or the Podman compat socket,\nor point DOCKER_HOST at a running engine", ErrDockerUnavailable, err)
	}
	return nil
}

func (d *Docker) Info(ctx context.Context) (Info, error) {
	res, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Info{}, wrap(err, "engine info")
	}
	i := res.Info
	return Info{
		Version:     i.ServerVersion,
		OSType:      i.OSType,
		MemTotal:    i.MemTotal,
		NCPU:        i.NCPU,
		ServerName:  i.Name,
		DockerRootD: i.DockerRootDir,
	}, nil
}

func (s Selector) filters() client.Filters {
	f := make(client.Filters)
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := s.Labels[k]; v == "" {
			f.Add("label", k)
		} else {
			f.Add("label", k+"="+v)
		}
	}
	for _, n := range s.Names {
		f.Add("name", "^/?"+n+"$")
	}
	return f
}

func (d *Docker) Containers(ctx context.Context, sel Selector) ([]Container, error) {
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     sel.All,
		Filters: sel.filters(),
	})
	if err != nil {
		return nil, wrap(err, "list containers")
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		c := Container{
			ID:        s.ID,
			Name:      trimName(s.Names),
			Image:     s.Image,
			ImageID:   s.ImageID,
			State:     State(s.State),
			Labels:    s.Labels,
			CreatedAt: time.Unix(s.Created, 0),
			Ports:     map[string][]HostPort{},
			Networks:  map[string]Endpoint{},
		}
		if s.Health != nil {
			c.Health = normaliseHealth(string(s.Health.Status))
		}
		for _, p := range s.Ports {
			if p.PublicPort == 0 {
				continue
			}
			key := fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
			c.Ports[key] = append(c.Ports[key], HostPort{IP: p.IP.String(), Port: int(p.PublicPort)})
		}
		if s.NetworkSettings != nil {
			for name, ep := range s.NetworkSettings.Networks {
				if ep == nil {
					continue
				}
				c.Networks[name] = Endpoint{
					NetworkID: ep.NetworkID,
					IPAddress: addrString(ep.IPAddress),
					Aliases:   ep.Aliases,
				}
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Docker) Container(ctx context.Context, id string) (Container, error) {
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Container{}, wrap(err, "inspect container "+id)
	}
	j := res.Container
	c := Container{
		ID:       j.ID,
		Name:     strings.TrimPrefix(j.Name, "/"),
		Ports:    map[string][]HostPort{},
		Networks: map[string]Endpoint{},
	}
	if j.Config != nil {
		c.Image = j.Config.Image
		c.Labels = j.Config.Labels
	}
	c.ImageID = j.Image
	if j.State != nil {
		c.State = State(j.State.Status)
		c.ExitCode = j.State.ExitCode
		if j.State.Health != nil {
			c.Health = normaliseHealth(string(j.State.Health.Status))
		}
		c.StartedAt, _ = time.Parse(time.RFC3339Nano, j.State.StartedAt)
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, j.Created)
	if j.NetworkSettings != nil {
		for port, bindings := range j.NetworkSettings.Ports {
			for _, b := range bindings {
				n, err := strconv.Atoi(b.HostPort)
				if err != nil {
					continue
				}
				key := port.String()
				c.Ports[key] = append(c.Ports[key], HostPort{IP: addrString(b.HostIP), Port: n})
			}
		}
		for name, ep := range j.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			c.Networks[name] = Endpoint{
				NetworkID: ep.NetworkID,
				IPAddress: addrString(ep.IPAddress),
				Aliases:   ep.Aliases,
			}
		}
	}
	return c, nil
}

func (d *Docker) Logs(ctx context.Context, id string, o LogOptions) (io.ReadCloser, error) {
	if !o.Stdout && !o.Stderr {
		o.Stdout, o.Stderr = true, true
	}
	res, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: o.Stdout,
		ShowStderr: o.Stderr,
		Follow:     o.Follow,
		Tail:       o.Tail,
		Since:      o.Since,
		Timestamps: o.Timestamps,
	})
	if err != nil {
		return nil, wrap(err, "logs for "+id)
	}
	return res, nil
}

func (d *Docker) Stats(ctx context.Context, ids []string) (map[string]Stats, error) {
	out := make(map[string]Stats, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := d.oneStat(ctx, id)
			if err != nil {
				return
			}
			mu.Lock()
			out[id] = s
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out, nil
}

func (d *Docker) oneStat(ctx context.Context, id string) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := d.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return Stats{}, err
	}
	defer res.Body.Close()
	var v container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return Stats{}, err
	}
	s := Stats{MemoryBytes: int64(v.MemoryStats.Usage)}

	if cache, ok := v.MemoryStats.Stats["inactive_file"]; ok && uint64(s.MemoryBytes) > cache {
		s.MemoryBytes -= int64(cache)
	}
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta > 0 {
		cpus := float64(v.CPUStats.OnlineCPUs)
		if cpus == 0 {
			cpus = float64(len(v.CPUStats.CPUUsage.PercpuUsage))
		}
		if cpus == 0 {
			cpus = 1
		}
		s.CPUPercent = (cpuDelta / sysDelta) * cpus * 100
	}
	return s, nil
}

func (d *Docker) RemoveContainer(ctx context.Context, id string, force bool) error {
	_, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: force, RemoveVolumes: false})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return wrap(err, "remove container "+id)
	}
	return nil
}

func (d *Docker) Networks(ctx context.Context, sel Selector) ([]Network, error) {
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: sel.filters()})
	if err != nil {
		return nil, wrap(err, "list networks")
	}
	out := make([]Network, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Network{ID: n.ID, Name: n.Name, Driver: n.Driver, Labels: n.Labels})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Docker) Network(ctx context.Context, id string) (Network, error) {
	res, err := d.cli.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if err != nil {
		return Network{}, wrap(err, "inspect network "+id)
	}
	n := res.Network
	out := Network{ID: n.ID, Name: n.Name, Driver: n.Driver, Labels: n.Labels, Containers: map[string]string{}}
	for cid, c := range n.Containers {
		out.Containers[cid] = c.Name
	}
	return out, nil
}

func (d *Docker) Connect(ctx context.Context, net, ctr string, aliases []string) error {
	var cfg *network.EndpointSettings
	if len(aliases) > 0 {
		cfg = &network.EndpointSettings{Aliases: aliases}
	}
	_, err := d.cli.NetworkConnect(ctx, net, client.NetworkConnectOptions{Container: ctr, EndpointConfig: cfg})
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "already exists in network") || strings.Contains(err.Error(), "is already attached") {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("network %s: %w", net, ErrNotFound)
	}
	return wrap(err, fmt.Sprintf("connect %s to network %s", ctr, net))
}

func (d *Docker) Disconnect(ctx context.Context, net, ctr string) error {
	_, err := d.cli.NetworkDisconnect(ctx, net, client.NetworkDisconnectOptions{Container: ctr, Force: true})
	if err == nil || cerrdefs.IsNotFound(err) {
		return nil
	}
	if strings.Contains(err.Error(), "is not connected to") {
		return nil
	}
	return wrap(err, fmt.Sprintf("disconnect %s from network %s", ctr, net))
}

func (d *Docker) RemoveNetwork(ctx context.Context, id string) error {
	_, err := d.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return wrap(err, "remove network "+id)
	}
	return nil
}

func (d *Docker) Volumes(ctx context.Context, sel Selector) ([]Volume, error) {
	res, err := d.cli.VolumeList(ctx, client.VolumeListOptions{Filters: sel.filters()})
	if err != nil {
		return nil, wrap(err, "list volumes")
	}
	out := make([]Volume, 0, len(res.Items))
	for _, v := range res.Items {
		out = append(out, toVolume(v.Name, v.Driver, v.Mountpoint, v.CreatedAt, v.Labels, v.UsageData))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Docker) Volume(ctx context.Context, name string) (Volume, error) {
	res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return Volume{}, fmt.Errorf("volume %s: %w", name, ErrNotFound)
		}
		return Volume{}, wrap(err, "inspect volume "+name)
	}
	v := res.Volume
	return toVolume(v.Name, v.Driver, v.Mountpoint, v.CreatedAt, v.Labels, v.UsageData), nil
}

func (d *Docker) CreateVolume(ctx context.Context, v VolumeSpec) error {
	_, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: v.Name, Labels: v.Labels})
	if err != nil {
		return wrap(err, "create volume "+v.Name)
	}
	return nil
}

func (d *Docker) RemoveVolume(ctx context.Context, name string, force bool) error {
	_, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: force})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return wrap(err, "remove volume "+name)
	}
	return nil
}

func (d *Docker) ImageID(ctx context.Context, ref string) (string, error) {
	res, err := d.cli.ImageInspect(ctx, ref)
	if err == nil {
		return res.ID, nil
	}
	if !cerrdefs.IsNotFound(err) {
		return "", wrap(err, "inspect image "+ref)
	}
	if err := d.EnsureImage(ctx, ref); err != nil {
		return "", err
	}
	res, err = d.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", wrap(err, "inspect image "+ref)
	}
	return res.ID, nil
}

func (d *Docker) EnsureImage(ctx context.Context, ref string) error {
	if _, err := d.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	res, err := d.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return wrap(err, "pull "+ref)
	}
	defer res.Close()

	if _, err := io.Copy(io.Discard, res); err != nil {
		return wrap(err, "pull "+ref)
	}
	return nil
}

func (d *Docker) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	start := time.Now()
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	if err := d.EnsureImage(ctx, spec.Image); err != nil {
		return RunResult{}, err
	}

	mounts := make([]mount.Mount, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.Volume,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	labels := map[string]string{meta.LabelManaged: "true", meta.LabelRole: "helper"}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    spec.Env,
			Labels: labels,
			User:   spec.User,
		},
		HostConfig: &container.HostConfig{
			Mounts:      mounts,
			AutoRemove:  false,
			NetworkMode: container.NetworkMode(spec.Network),
		},
	})
	if err != nil {
		return RunResult{}, wrap(err, "create helper container")
	}
	id := created.ID
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = d.RemoveContainer(cleanup, id, true)
	}()

	waitRes := d.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return RunResult{}, wrap(err, "start helper container")
	}

	code, err := waitForExit(ctx, waitRes)
	if err != nil {
		return RunResult{}, err
	}

	if final, err := d.Container(context.WithoutCancel(ctx), id); err == nil && final.State == StateExited {
		code = final.ExitCode
	}

	out, err := d.readAllLogs(context.WithoutCancel(ctx), id)
	if err != nil {
		out = "(logs unavailable: " + err.Error() + ")"
	}
	return RunResult{ExitCode: code, Output: out, Duration: time.Since(start)}, nil
}

func waitForExit(ctx context.Context, res client.ContainerWaitResult) (int, error) {
	errs, results := res.Error, res.Result
	for {
		select {
		case err, ok := <-errs:
			if ok && err != nil {
				return 0, wrap(err, "wait for helper container")
			}
			errs = nil
		case r, ok := <-results:
			if !ok {
				results = nil
				if errs == nil {
					return 0, fmt.Errorf("the engine closed the wait stream without reporting an exit status")
				}
				continue
			}
			if r.Error != nil && r.Error.Message != "" {
				return int(r.StatusCode), fmt.Errorf("helper container: %s", r.Error.Message)
			}
			return int(r.StatusCode), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (d *Docker) readAllLogs(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := d.Logs(ctx, id, LogOptions{Stdout: true, Stderr: true, Tail: "200"})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, 256<<10)); err != nil {
		return buf.String(), err
	}
	return DemuxLogs(buf.Bytes()), nil
}

func toVolume(name, driver, mountpoint string, created string, labels map[string]string, usage any) Volume {
	v := Volume{Name: name, Driver: driver, Mountpoint: mountpoint, Labels: labels, Size: -1}
	v.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if u, ok := usage.(interface{ GetSize() int64 }); ok {
		v.Size = u.GetSize()
	}
	return v
}

func trimName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func normaliseHealth(s string) Health {
	switch strings.ToLower(s) {
	case "healthy":
		return HealthHealthy
	case "unhealthy":
		return HealthUnhealthy
	case "starting":
		return HealthStarting
	}
	return HealthNone
}

func addrString(a interface{ IsValid() bool }) string {
	if a == nil || !a.IsValid() {
		return ""
	}
	if s, ok := a.(fmt.Stringer); ok {
		return s.String()
	}
	return ""
}

func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	if client.IsErrConnectionFailed(err) {
		return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func toMounts(in []Mount) []mount.Mount {
	out := make([]mount.Mount, 0, len(in))
	for _, m := range in {
		out = append(out, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.Volume,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func parseAddr(s string) (netip.Addr, error) { return netip.ParseAddr(s) }
