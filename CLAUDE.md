# dockerfs

FUSE filesystem for Docker and Docker Swarm inspection.
Read-only directory tree over containers, services, nodes, and jobs.

## Project layout

```
dockerfs/
  cmd/dockerfs/main.go          entry point — flags, Docker ping, mount, signal handling
  internal/
    docker/
      client.go                 Client wrapper: New(), Ping(), Raw(), MapErr()
      streams.go                ContainerLogsStream, ContainerLogsOnce, ServiceLogsStreamTagged,
                                demuxReader, taggedServiceLogReader
    fs/
      root.go                   Root node (/), FS type, routes to /local and /swarm
      local.go                  /local/containers/<name>/{env,inspect,logs,stdout,stderr,stats}
      swarm.go                  /swarm/services, /swarm/nodes, /swarm/jobs
  go.mod / go.sum
```

## Tech stack

- Go 1.24+
- `bazil.org/fuse` — FUSE userspace bindings
- `github.com/docker/docker` v27 — official Docker SDK (monorepo, +incompatible)
- `log/slog` — structured logging

## FUSE node types

| Type | File | Description |
|---|---|---|
| `StaticFile` | local.go | Fetches once on Open(), serves buffer with offset |
| `StreamFile` | local.go | Opens Follow:true log stream; Release() closes it |
| `StatsFile` | local.go | ContainerStats(stream:true), reads first frame only — avoids ~1s daemon wait |
| `ServiceStreamFile` | swarm.go | Like StreamFile but calls ServiceLogs with Details:true; prefixes each line with [slot] |

All Dir nodes implement `fs.Node + fs.HandleReadDirAller + fs.NodeStringLookuper`.

## Docker error mapping

```
IsErrNotFound()              → fuse.ENOENT
"swarm" + "not" in message   → fuse.EPERM
everything else              → fuse.EIO
```

Always call `docker.MapErr(err, "op")` before returning to FUSE layer.
Never return raw Docker errors.

## Swarm detection

```go
info.Swarm.LocalNodeState == "active"
```

If not active: `/swarm` exists but `ReadDirAll` returns empty slice — no error.

## Jobs detection

Tasks where `Spec.RestartPolicy.Condition == "none"` are treated as jobs.
Their logs are fetched with `ContainerLogsOnce` (non-blocking, no Follow).

## Stats formula

```
cpuDelta  = CPUStats.CPUUsage.TotalUsage - PreCPUStats.CPUUsage.TotalUsage
sysDelta  = CPUStats.SystemUsage         - PreCPUStats.SystemUsage
numCPU    = CPUStats.OnlineCPUs  (fallback: len(PercpuUsage))
cpu_pct   = (cpuDelta / sysDelta) * numCPU * 100
```

## Flags

```
--mountpoint string   mount point path (default: /mnt/dockerfs)
--debug               enable FUSE debug logging (slog.LevelDebug)
--allow-other         AllowOther() FUSE option; requires user_allow_other in /etc/fuse.conf
```

## Running locally

```bash
sudo apt install fuse3
go build -o dockerfs ./cmd/dockerfs
mkdir -p ~/mnt/dockerfs
./dockerfs --mountpoint ~/mnt/dockerfs --debug

# unmount
fusermount3 -u ~/mnt/dockerfs
```

## Known tool limitations

- `tail -f` does not work on streaming files — FUSE emits no inotify events; use `cat` instead
- `grep` on a streaming file blocks until interrupted; pipe through `head` or use `grep ... | head -N`
- `cat` on streaming files does not exit (Follow:true has no EOF while container runs)

## What is NOT implemented

- Any writes — fully read-only
- `/swarm/nodes/<name>/containers/<service.N>/{logs,stdout,stderr,stats,node}` — replicas running on this node
- docker secrets
- mmap
- caching between Read() calls (except the open stream handle)
- hard links
- file creation or deletion
