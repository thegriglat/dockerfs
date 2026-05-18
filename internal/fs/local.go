package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"dockerfs/internal/docker"
)

// ---- /local ----------------------------------------------------------------

// LocalDir is the /local directory.
type LocalDir struct{ cl *docker.Client }

func NewLocalDir(cl *docker.Client) *LocalDir { return &LocalDir{cl: cl} }

var _ fs.Node = (*LocalDir)(nil)
var _ fs.HandleReadDirAller = (*LocalDir)(nil)
var _ fs.NodeStringLookuper = (*LocalDir)(nil)

func (d *LocalDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *LocalDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "containers", Type: fuse.DT_Dir},
	}, nil
}

func (d *LocalDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if name == "containers" {
		return &ContainersDir{cl: d.cl}, nil
	}
	return nil, fuse.ENOENT
}

// ---- /local/containers -----------------------------------------------------

// ContainersDir lists running containers.
type ContainersDir struct{ cl *docker.Client }

var _ fs.Node = (*ContainersDir)(nil)
var _ fs.HandleReadDirAller = (*ContainersDir)(nil)
var _ fs.NodeStringLookuper = (*ContainersDir)(nil)

func (d *ContainersDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *ContainersDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	containers, err := d.cl.Raw().ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		return nil, docker.MapErr(err, "ContainerList")
	}
	entries := make([]fuse.Dirent, 0, len(containers))
	for _, c := range containers {
		name := containerName(c.Names)
		entries = append(entries, fuse.Dirent{Name: name, Type: fuse.DT_Dir})
	}
	return entries, nil
}

func (d *ContainersDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	containers, err := d.cl.Raw().ContainerList(ctx, container.ListOptions{
		All:     false,
		Filters: filters.NewArgs(filters.Arg("name", "/"+name)),
	})
	if err != nil {
		return nil, docker.MapErr(err, "ContainerList/lookup")
	}
	for _, c := range containers {
		if containerName(c.Names) == name {
			return &ContainerDir{cl: d.cl, id: c.ID, name: name}, nil
		}
	}
	slog.Debug("container not found", "name", name)
	return nil, fuse.ENOENT
}

func containerName(names []string) string {
	if len(names) == 0 {
		return "unknown"
	}
	return strings.TrimPrefix(names[0], "/")
}

// ---- /local/containers/<name> ----------------------------------------------

// ContainerDir is the per-container directory.
type ContainerDir struct {
	cl   *docker.Client
	id   string
	name string
}

var _ fs.Node = (*ContainerDir)(nil)
var _ fs.HandleReadDirAller = (*ContainerDir)(nil)
var _ fs.NodeStringLookuper = (*ContainerDir)(nil)

func (d *ContainerDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

var containerFiles = []fuse.Dirent{
	{Name: "env", Type: fuse.DT_File},
	{Name: "inspect", Type: fuse.DT_File},
	{Name: "logs", Type: fuse.DT_File},
	{Name: "mounts", Type: fuse.DT_File},
	{Name: "network", Type: fuse.DT_File},
	{Name: "ports", Type: fuse.DT_File},
	{Name: "stdout", Type: fuse.DT_File},
	{Name: "stderr", Type: fuse.DT_File},
	{Name: "stats", Type: fuse.DT_File},
}

func (d *ContainerDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return containerFiles, nil
}

func (d *ContainerDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	switch name {
	case "env":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchEnv(ctx, d.cl, d.id)
		}}, nil
	case "inspect":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchInspect(ctx, d.cl, d.id)
		}}, nil
	case "logs":
		return &StreamFile{cl: d.cl, id: d.id, stdout: true, stderr: true}, nil
	case "mounts":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchMounts(ctx, d.cl, d.id)
		}}, nil
	case "network":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchNetwork(ctx, d.cl, d.id)
		}}, nil
	case "ports":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchPorts(ctx, d.cl, d.id)
		}}, nil
	case "stdout":
		return &StreamFile{cl: d.cl, id: d.id, stdout: true, stderr: false}, nil
	case "stderr":
		return &StreamFile{cl: d.cl, id: d.id, stdout: false, stderr: true}, nil
	case "stats":
		return &StatsFile{cl: d.cl, id: d.id}, nil
	}
	return nil, fuse.ENOENT
}

func fetchEnv(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	info, err := cl.Raw().ContainerInspect(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerInspect/env")
	}
	return []byte(strings.Join(info.Config.Env, "\n") + "\n"), nil
}

func fetchMounts(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	info, err := cl.Raw().ContainerInspect(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerInspect/mounts")
	}
	var sb strings.Builder
	for _, m := range info.Mounts {
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		fmt.Fprintf(&sb, "%s  %s  %s\n", m.Source, m.Destination, mode)
	}
	return []byte(sb.String()), nil
}

func fetchNetwork(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	info, err := cl.Raw().ContainerInspect(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerInspect/network")
	}
	var sb strings.Builder
	for name, ep := range info.NetworkSettings.Networks {
		fmt.Fprintf(&sb, "%-20s %s\n", name, ep.IPAddress)
	}
	return []byte(sb.String()), nil
}

func fetchPorts(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	info, err := cl.Raw().ContainerInspect(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerInspect/ports")
	}
	var sb strings.Builder
	for containerPort, bindings := range info.NetworkSettings.Ports {
		if len(bindings) == 0 {
			fmt.Fprintf(&sb, "%s\n", containerPort)
			continue
		}
		for _, b := range bindings {
			fmt.Fprintf(&sb, "%s:%s -> %s\n", b.HostIP, b.HostPort, containerPort)
		}
	}
	return []byte(sb.String()), nil
}

func fetchInspect(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	info, err := cl.Raw().ContainerInspect(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerInspect")
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fuse.EIO
	}
	return append(data, '\n'), nil
}

// ---- StaticFile ------------------------------------------------------------

// StaticFile fetches data once on Open() and serves it from a buffer.
type StaticFile struct {
	fetch func(context.Context) ([]byte, error)
}

var _ fs.Node = (*StaticFile)(nil)
var _ fs.NodeOpener = (*StaticFile)(nil)

func (f *StaticFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o444
	a.Size = 0 // unknown until opened
	return nil
}

func (f *StaticFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	data, err := f.fetch(ctx)
	if err != nil {
		return nil, err
	}
	resp.Flags |= fuse.OpenDirectIO
	return &staticHandle{data: data}, nil
}

type staticHandle struct{ data []byte }

var _ fs.Handle = (*staticHandle)(nil)
var _ fs.HandleReader = (*staticHandle)(nil)

func (h *staticHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	off := int(req.Offset)
	if off >= len(h.data) {
		return nil
	}
	end := off + req.Size
	if end > len(h.data) {
		end = len(h.data)
	}
	resp.Data = h.data[off:end]
	return nil
}

// ---- StreamFile ------------------------------------------------------------

// StreamFile is stdout/stderr — opens a Follow:true log stream.
type StreamFile struct {
	cl     *docker.Client
	id     string
	stdout bool
	stderr bool
}

var _ fs.Node = (*StreamFile)(nil)
var _ fs.NodeOpener = (*StreamFile)(nil)

func (f *StreamFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o444
	return nil
}

func (f *StreamFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	rc, err := f.cl.ContainerLogsStream(streamCtx, f.id, f.stdout, f.stderr)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Flags |= fuse.OpenDirectIO | fuse.OpenNonSeekable
	return &streamHandle{rc: rc, cancel: cancel}, nil
}

type streamHandle struct {
	rc        io.ReadCloser
	cancel    context.CancelFunc
	closeOnce sync.Once
}

var _ fs.Handle = (*streamHandle)(nil)
var _ fs.HandleReader = (*streamHandle)(nil)
var _ fs.HandleReleaser = (*streamHandle)(nil)

func (h *streamHandle) close() {
	h.closeOnce.Do(func() {
		h.cancel()
		h.rc.Close()
	})
}

func (h *streamHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	type result struct {
		n   int
		err error
	}
	buf := make([]byte, req.Size)
	ch := make(chan result, 1)
	go func() {
		n, err := h.rc.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case <-ctx.Done():
		// FUSE_INTERRUPT received (Ctrl-C on cat). Close the stream so the
		// goroutine above unblocks, then signal the kernel to retry or give up.
		h.close()
		return fuse.EINTR
	case r := <-ch:
		if r.n > 0 {
			resp.Data = buf[:r.n]
			return nil
		}
		if r.err == io.EOF {
			return nil
		}
		return fuse.EIO
	}
}

func (h *streamHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	h.close()
	return nil
}

// ---- StatsFile -------------------------------------------------------------

// StatsFile returns a fresh stats JSON on every Read().
type StatsFile struct {
	cl *docker.Client
	id string
}

var _ fs.Node = (*StatsFile)(nil)
var _ fs.NodeOpener = (*StatsFile)(nil)

func (f *StatsFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o444
	return nil
}

func (f *StatsFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	resp.Flags |= fuse.OpenDirectIO
	return &statsHandle{cl: f.cl, id: f.id}, nil
}

type statsHandle struct {
	cl  *docker.Client
	id  string
	buf []byte
	off int
}

var _ fs.Handle = (*statsHandle)(nil)
var _ fs.HandleReader = (*statsHandle)(nil)

func (h *statsHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.Offset == 0 || h.buf == nil {
		data, err := fetchStats(ctx, h.cl, h.id)
		if err != nil {
			return err
		}
		h.buf = data
		h.off = 0
	}
	off := int(req.Offset)
	if off >= len(h.buf) {
		return nil
	}
	end := off + req.Size
	if end > len(h.buf) {
		end = len(h.buf)
	}
	resp.Data = h.buf[off:end]
	return nil
}

type statsOutput struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`
	MemLimit   uint64  `json:"mem_limit"`
	NetRx      uint64  `json:"net_rx"`
	NetTx      uint64  `json:"net_tx"`
}

func fetchStats(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	resp, err := cl.Raw().ContainerStats(ctx, id, true)
	if err != nil {
		return nil, docker.MapErr(err, "ContainerStats")
	}
	defer resp.Body.Close()

	var s dockertypes.StatsJSON
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fuse.EIO
	}

	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
	numCPU := float64(s.CPUStats.OnlineCPUs)
	if numCPU == 0 {
		numCPU = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	var cpuPct float64
	if sysDelta > 0 && cpuDelta > 0 {
		cpuPct = (cpuDelta / sysDelta) * numCPU * 100.0
	}

	var netRx, netTx uint64
	for _, v := range s.Networks {
		netRx += v.RxBytes
		netTx += v.TxBytes
	}

	out := statsOutput{
		CPUPercent: cpuPct,
		MemUsage:   s.MemoryStats.Usage,
		MemLimit:   s.MemoryStats.Limit,
		NetRx:      netRx,
		NetTx:      netTx,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fuse.EIO
	}
	return append(data, '\n'), nil
}

// ensure unused import compiles
var _ = fmt.Sprintf
