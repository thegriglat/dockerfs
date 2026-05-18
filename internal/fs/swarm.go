package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"dockerfs/internal/docker"
)

// ---- /swarm ----------------------------------------------------------------

// SwarmDir is the /swarm top-level directory.
type SwarmDir struct{ cl *docker.Client }

func NewSwarmDir(cl *docker.Client) *SwarmDir { return &SwarmDir{cl: cl} }

var _ fs.Node = (*SwarmDir)(nil)
var _ fs.HandleReadDirAller = (*SwarmDir)(nil)
var _ fs.NodeStringLookuper = (*SwarmDir)(nil)

func (d *SwarmDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *SwarmDir) isActive(ctx context.Context) bool {
	info, err := d.cl.Raw().Info(ctx)
	if err != nil {
		return false
	}
	return string(info.Swarm.LocalNodeState) == "active"
}

func (d *SwarmDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	if !d.isActive(ctx) {
		return []fuse.Dirent{}, nil
	}
	return []fuse.Dirent{
		{Name: "services", Type: fuse.DT_Dir},
		{Name: "nodes", Type: fuse.DT_Dir},
		{Name: "jobs", Type: fuse.DT_Dir},
	}, nil
}

func (d *SwarmDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	if !d.isActive(ctx) {
		return nil, fuse.ENOENT
	}
	switch name {
	case "services":
		return &ServicesDir{cl: d.cl}, nil
	case "nodes":
		return &NodesDir{cl: d.cl}, nil
	case "jobs":
		return &JobsDir{cl: d.cl}, nil
	}
	return nil, fuse.ENOENT
}

// ---- /swarm/services -------------------------------------------------------

type ServicesDir struct{ cl *docker.Client }

var _ fs.Node = (*ServicesDir)(nil)
var _ fs.HandleReadDirAller = (*ServicesDir)(nil)
var _ fs.NodeStringLookuper = (*ServicesDir)(nil)

func (d *ServicesDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *ServicesDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	services, err := d.cl.Raw().ServiceList(ctx, dockertypes.ServiceListOptions{})
	if err != nil {
		return nil, docker.MapErr(err, "ServiceList")
	}
	entries := make([]fuse.Dirent, 0, len(services))
	for _, svc := range services {
		entries = append(entries, fuse.Dirent{Name: svc.Spec.Name, Type: fuse.DT_Dir})
	}
	return entries, nil
}

func (d *ServicesDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	services, err := d.cl.Raw().ServiceList(ctx, dockertypes.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return nil, docker.MapErr(err, "ServiceList/lookup")
	}
	for _, svc := range services {
		if svc.Spec.Name == name {
			return &ServiceDir{cl: d.cl, id: svc.ID, name: name}, nil
		}
	}
	return nil, fuse.ENOENT
}

// ---- /swarm/services/<name> ------------------------------------------------

type ServiceDir struct {
	cl   *docker.Client
	id   string
	name string
}

var _ fs.Node = (*ServiceDir)(nil)
var _ fs.HandleReadDirAller = (*ServiceDir)(nil)
var _ fs.NodeStringLookuper = (*ServiceDir)(nil)

func (d *ServiceDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *ServiceDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "status", Type: fuse.DT_File},
		{Name: "logs", Type: fuse.DT_File},
		{Name: "replicas", Type: fuse.DT_Dir},
	}, nil
}

func (d *ServiceDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	switch name {
	case "status":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchServiceStatus(ctx, d.cl, d.id)
		}}, nil
	case "logs":
		return &ServiceLogsFile{cl: d.cl, id: d.id}, nil
	case "replicas":
		return &ReplicasDir{cl: d.cl, serviceID: d.id, serviceName: d.name}, nil
	}
	return nil, fuse.ENOENT
}

func fetchServiceStatus(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	svc, _, err := cl.Raw().ServiceInspectWithRaw(ctx, id, dockertypes.ServiceInspectOptions{})
	if err != nil {
		return nil, docker.MapErr(err, "ServiceInspect")
	}

	tasks, err := cl.Raw().TaskList(ctx, dockertypes.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", id)),
	})
	if err != nil {
		return nil, docker.MapErr(err, "TaskList/status")
	}

	var running int
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			running++
		}
	}

	var desired int
	if svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil {
		desired = int(*svc.Spec.Mode.Replicated.Replicas)
	}

	image := svc.Spec.TaskTemplate.ContainerSpec.Image
	var msg string
	if svc.UpdateStatus != nil {
		msg = svc.UpdateStatus.Message
	}

	out := fmt.Sprintf("state:    running\nreplicas: %d/%d\nimage:    %s\nupdated:  %s\nmessage:  %s\n",
		running,
		desired,
		image,
		svc.UpdatedAt.String(),
		msg,
	)
	return []byte(out), nil
}

// ---- ServiceLogsFile -------------------------------------------------------

type ServiceLogsFile struct {
	cl *docker.Client
	id string
}

var _ fs.Node = (*ServiceLogsFile)(nil)
var _ fs.NodeOpener = (*ServiceLogsFile)(nil)

func (f *ServiceLogsFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = 0o444
	return nil
}

func (f *ServiceLogsFile) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	rc, err := f.cl.ServiceLogsStream(streamCtx, f.id)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Flags |= fuse.OpenDirectIO | fuse.OpenNonSeekable
	return &streamHandle{rc: rc, cancel: cancel}, nil
}

// ---- /swarm/services/<name>/replicas ---------------------------------------

type ReplicasDir struct {
	cl          *docker.Client
	serviceID   string
	serviceName string
}

var _ fs.Node = (*ReplicasDir)(nil)
var _ fs.HandleReadDirAller = (*ReplicasDir)(nil)
var _ fs.NodeStringLookuper = (*ReplicasDir)(nil)

func (d *ReplicasDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *ReplicasDir) listTasks(ctx context.Context) ([]swarm.Task, error) {
	tasks, err := d.cl.Raw().TaskList(ctx, dockertypes.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", d.serviceID),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return nil, docker.MapErr(err, "TaskList/replicas")
	}
	return tasks, nil
}

func (d *ReplicasDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	tasks, err := d.listTasks(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]fuse.Dirent, 0, len(tasks))
	for _, t := range tasks {
		entries = append(entries, fuse.Dirent{Name: replicaName(d.serviceName, t), Type: fuse.DT_Dir})
	}
	return entries, nil
}

func (d *ReplicasDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	tasks, err := d.listTasks(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if replicaName(d.serviceName, t) == name {
			return &ReplicaDir{cl: d.cl, task: t}, nil
		}
	}
	return nil, fuse.ENOENT
}

func replicaName(svcName string, t swarm.Task) string {
	return fmt.Sprintf("%s.%d", svcName, t.Slot)
}

// ---- /swarm/services/<name>/replicas/<replica> -----------------------------

type ReplicaDir struct {
	cl   *docker.Client
	task swarm.Task
}

var _ fs.Node = (*ReplicaDir)(nil)
var _ fs.HandleReadDirAller = (*ReplicaDir)(nil)
var _ fs.NodeStringLookuper = (*ReplicaDir)(nil)

func (d *ReplicaDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *ReplicaDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "stdout", Type: fuse.DT_File},
		{Name: "stats", Type: fuse.DT_File},
		{Name: "node", Type: fuse.DT_File},
	}, nil
}

func (d *ReplicaDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	cid := d.task.Status.ContainerStatus.ContainerID
	nodeID := d.task.NodeID
	switch name {
	case "stdout":
		return &StreamFile{cl: d.cl, id: cid, stdout: true, stderr: true}, nil
	case "stats":
		return &StatsFile{cl: d.cl, id: cid}, nil
	case "node":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchNodeName(ctx, d.cl, nodeID)
		}}, nil
	}
	return nil, fuse.ENOENT
}

func fetchNodeName(ctx context.Context, cl *docker.Client, nodeID string) ([]byte, error) {
	node, _, err := cl.Raw().NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return nil, docker.MapErr(err, "NodeInspect")
	}
	return []byte(node.Description.Hostname + "\n"), nil
}

// ---- /swarm/nodes ----------------------------------------------------------

type NodesDir struct{ cl *docker.Client }

var _ fs.Node = (*NodesDir)(nil)
var _ fs.HandleReadDirAller = (*NodesDir)(nil)
var _ fs.NodeStringLookuper = (*NodesDir)(nil)

func (d *NodesDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *NodesDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	nodes, err := d.cl.Raw().NodeList(ctx, dockertypes.NodeListOptions{})
	if err != nil {
		return nil, docker.MapErr(err, "NodeList")
	}
	entries := make([]fuse.Dirent, 0, len(nodes))
	for _, n := range nodes {
		entries = append(entries, fuse.Dirent{Name: n.Description.Hostname, Type: fuse.DT_Dir})
	}
	return entries, nil
}

func (d *NodesDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	nodes, err := d.cl.Raw().NodeList(ctx, dockertypes.NodeListOptions{})
	if err != nil {
		return nil, docker.MapErr(err, "NodeList/lookup")
	}
	for _, n := range nodes {
		if n.Description.Hostname == name {
			return &NodeDir{cl: d.cl, node: n}, nil
		}
	}
	return nil, fuse.ENOENT
}

// ---- /swarm/nodes/<hostname> -----------------------------------------------

type NodeDir struct {
	cl   *docker.Client
	node swarm.Node
}

var _ fs.Node = (*NodeDir)(nil)
var _ fs.HandleReadDirAller = (*NodeDir)(nil)
var _ fs.NodeStringLookuper = (*NodeDir)(nil)

func (d *NodeDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *NodeDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "status", Type: fuse.DT_File},
		{Name: "labels", Type: fuse.DT_File},
	}, nil
}

func (d *NodeDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	id := d.node.ID
	switch name {
	case "status":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchNodeStatus(ctx, d.cl, id)
		}}, nil
	case "labels":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchNodeLabels(ctx, d.cl, id)
		}}, nil
	}
	return nil, fuse.ENOENT
}

func fetchNodeStatus(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	n, _, err := cl.Raw().NodeInspectWithRaw(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "NodeInspect/status")
	}
	out := fmt.Sprintf("id:           %s\nrole:         %s\navailability: %s\nstate:        %s\naddr:         %s\nengine:       %s\n",
		n.ID,
		strings.ToLower(string(n.Spec.Role)),
		strings.ToLower(string(n.Spec.Availability)),
		strings.ToLower(string(n.Status.State)),
		n.Status.Addr,
		n.Description.Engine.EngineVersion,
	)
	return []byte(out), nil
}

func fetchNodeLabels(ctx context.Context, cl *docker.Client, id string) ([]byte, error) {
	n, _, err := cl.Raw().NodeInspectWithRaw(ctx, id)
	if err != nil {
		return nil, docker.MapErr(err, "NodeInspect/labels")
	}
	var sb strings.Builder
	for k, v := range n.Spec.Labels {
		sb.WriteString(k + "=" + v + "\n")
	}
	return []byte(sb.String()), nil
}

// ---- /swarm/jobs -----------------------------------------------------------

type JobsDir struct{ cl *docker.Client }

var _ fs.Node = (*JobsDir)(nil)
var _ fs.HandleReadDirAller = (*JobsDir)(nil)
var _ fs.NodeStringLookuper = (*JobsDir)(nil)

func (d *JobsDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *JobsDir) listJobTasks(ctx context.Context) ([]swarm.Task, error) {
	tasks, err := d.cl.Raw().TaskList(ctx, dockertypes.TaskListOptions{})
	if err != nil {
		return nil, docker.MapErr(err, "TaskList/jobs")
	}
	var jobs []swarm.Task
	for _, t := range tasks {
		if t.Spec.RestartPolicy != nil && string(t.Spec.RestartPolicy.Condition) == "none" {
			jobs = append(jobs, t)
		}
	}
	return jobs, nil
}

func (d *JobsDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	tasks, err := d.listJobTasks(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	entries := []fuse.Dirent{}
	for _, t := range tasks {
		name := jobName(t)
		if !seen[name] {
			seen[name] = true
			entries = append(entries, fuse.Dirent{Name: name, Type: fuse.DT_Dir})
		}
	}
	return entries, nil
}

func (d *JobsDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	tasks, err := d.listJobTasks(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if jobName(t) == name {
			return &JobDir{cl: d.cl, task: t}, nil
		}
	}
	return nil, fuse.ENOENT
}

func jobName(t swarm.Task) string {
	if t.Name != "" {
		return t.Name
	}
	return t.ID[:12]
}

// ---- /swarm/jobs/<name> ----------------------------------------------------

type JobDir struct {
	cl   *docker.Client
	task swarm.Task
}

var _ fs.Node = (*JobDir)(nil)
var _ fs.HandleReadDirAller = (*JobDir)(nil)
var _ fs.NodeStringLookuper = (*JobDir)(nil)

func (d *JobDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (d *JobDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "status", Type: fuse.DT_File},
		{Name: "logs", Type: fuse.DT_File},
		{Name: "inspect", Type: fuse.DT_File},
	}, nil
}

func (d *JobDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	t := d.task
	cid := t.Status.ContainerStatus.ContainerID
	switch name {
	case "status":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return fetchJobStatus(t), nil
		}}, nil
	case "logs":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			return d.cl.ContainerLogsOnce(ctx, cid, true, true)
		}}, nil
	case "inspect":
		return &StaticFile{fetch: func(ctx context.Context) ([]byte, error) {
			raw, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				return nil, fuse.EIO
			}
			return append(raw, '\n'), nil
		}}, nil
	}
	return nil, fuse.ENOENT
}

func fetchJobStatus(t swarm.Task) []byte {
	started := "n/a"
	if !t.Status.Timestamp.IsZero() {
		started = t.Status.Timestamp.String()
	}
	exitCode := 0
	errMsg := ""
	if t.Status.ContainerStatus != nil {
		exitCode = t.Status.ContainerStatus.ExitCode
	}
	if t.Status.Err != "" {
		errMsg = t.Status.Err
	}
	out := fmt.Sprintf("id:         %s\nstate:      %s\nstarted:    %s\nfinished:   n/a\nexit_code:  %d\nerror:      %s\n",
		t.ID,
		strings.ToLower(string(t.Status.State)),
		started,
		exitCode,
		errMsg,
	)
	return []byte(out)
}

// suppress unused imports
var _ = io.EOF
var _ = slog.LevelDebug
