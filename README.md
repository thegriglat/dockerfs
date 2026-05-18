# dockerfs

A read-only FUSE filesystem that exposes Docker containers, Swarm services, nodes, and jobs as a directory tree. Inspect running containers, stream logs, and browse Swarm topology using ordinary shell tools — `cat`, `grep`, `watch`.

```
/mnt/dockerfs/
├── local/
│   └── containers/
│       └── <name>/
│           ├── env        KEY=VALUE lines from container environment
│           ├── inspect    raw JSON from docker inspect
│           ├── logs       live combined log stream (stdout + stderr)
│           ├── stdout     live stdout stream
│           ├── stderr     live stderr stream
│           └── stats      JSON snapshot refreshed on every read
└── swarm/                 empty if Swarm is not active
    ├── services/
    │   └── <name>/
    │       ├── status     human-readable replicas/image/update info
    │       ├── logs       aggregated log stream from all replicas (prefixed [slot])
    │       ├── stdout     stdout only, prefixed [slot]
    │       ├── stderr     stderr only, prefixed [slot]
    │       └── replicas/
    │           └── <name.N>/
    │               ├── stdout
    │               ├── stats
    │               └── node   hostname of the node running this replica
    ├── nodes/
    │   └── <hostname>/
    │       ├── status     role, availability, state, addr, engine version
    │       └── labels     KEY=VALUE lines from node labels
    └── jobs/
        └── <name>/
            ├── status     id, state, exit_code, error
            ├── logs       completed output (non-blocking)
            └── inspect    raw task JSON
```

## Requirements

| Dependency                | Notes                                              |
| ------------------------- | -------------------------------------------------- |
| Linux kernel ≥ 4.x        | FUSE support required                              |
| `fuse` or `fuse3` package | `apt install fuse3` / `dnf install fuse3`          |
| Docker Engine             | Socket at `/var/run/docker.sock` or `$DOCKER_HOST` |
| Go ≥ 1.24                 | Build only                                         |

> **macOS:** macFUSE is partially supported but not tested. Use Linux for production use.

## Installation

```bash
# Install FUSE (Debian / Ubuntu)
sudo apt install fuse3

# Add yourself to the docker group if needed
sudo usermod -aG docker $USER   # re-login after this

# Build
git clone https://github.com/yourname/dockerfs
cd dockerfs
go build -o dockerfs ./cmd/dockerfs

# Create a mount point
mkdir -p ~/mnt/dockerfs

# Mount
./dockerfs --mountpoint ~/mnt/dockerfs

# Unmount
fusermount3 -u ~/mnt/dockerfs
```

## Flags

| Flag            | Default         | Description                                                                                                   |
| --------------- | --------------- | ------------------------------------------------------------------------------------------------------------- |
| `--mountpoint`  | `/mnt/dockerfs` | Directory to mount the filesystem on                                                                          |
| `--debug`       | `false`         | Enable FUSE debug logging and verbose slog output                                                             |
| `--allow-other` | `false`         | Allow users other than the mounter to access the filesystem (requires `user_allow_other` in `/etc/fuse.conf`) |

## Usage examples

### Containers

```bash
# Stream stdout from a running container
cat ~/mnt/dockerfs/local/containers/nginx/stdout

# Watch CPU and memory usage
watch -n1 cat ~/mnt/dockerfs/local/containers/nginx/stats

# Check environment variables
cat ~/mnt/dockerfs/local/containers/postgres/env

# Full docker inspect as JSON
cat ~/mnt/dockerfs/local/containers/myapp/inspect | jq .State

# Search for errors in stdout
grep ERROR ~/mnt/dockerfs/local/containers/myapp/stdout

# Search across all containers (reads historical logs)
grep -h ERROR ~/mnt/dockerfs/local/containers/*/stdout
```

### Swarm services

```bash
# Watch replica counts
watch -n2 cat ~/mnt/dockerfs/swarm/services/nginx/status

# Stream aggregated logs from all replicas with [slot] prefix
cat ~/mnt/dockerfs/swarm/services/api/logs

# Stream only stderr from all replicas
cat ~/mnt/dockerfs/swarm/services/api/stderr

# Check which node a specific replica runs on
cat ~/mnt/dockerfs/swarm/services/api/replicas/api.1/node

# Live stats for a replica
watch -n1 cat ~/mnt/dockerfs/swarm/services/api/replicas/api.2/stats
```

### Swarm nodes

```bash
# List all nodes
ls ~/mnt/dockerfs/swarm/nodes/

# Check node status
cat ~/mnt/dockerfs/swarm/nodes/worker-1/status

# View node labels
cat ~/mnt/dockerfs/swarm/nodes/worker-1/labels
```

### Swarm jobs

```bash
# List completed and running jobs
ls ~/mnt/dockerfs/swarm/jobs/

# Check job exit status
cat ~/mnt/dockerfs/swarm/jobs/db-migration/status

# Read job output
cat ~/mnt/dockerfs/swarm/jobs/db-migration/logs

# Find all failed jobs
for d in ~/mnt/dockerfs/swarm/jobs/*/; do
  grep -l "state:.*failed" "$d/status" 2>/dev/null
done
```

## Known limitations

- **`tail -f` does not work** on streaming files (`stdout`, `stderr`, `logs`). FUSE does not emit inotify events, so `tail -f` either blocks in pipe mode waiting for EOF (which never comes) or waits for inotify events that never fire. Use `cat` instead — it reads the live stream directly.
- **`grep` on streaming files blocks** until the process is killed (e.g. `grep pattern .../stdout | head -20` works; plain `grep` without `head` runs until interrupted).
- **`cat` on streaming files does not exit** — the log stream uses `Follow: true` and has no EOF while the container runs. Kill with Ctrl-C or pipe through `head`.
- **No writes** — the filesystem is fully read-only.
- **No mmap support**.
- **`/swarm/nodes/<name>/containers`** — not implemented (TODO).
- **Docker secrets** — not exposed.

## Architecture

Every path maps to a FUSE node implementing `bazil.org/fuse/fs`:

```
Node type          Interfaces implemented
─────────────────────────────────────────────────────────────────
Dir nodes          fs.Node, fs.HandleReadDirAller, fs.NodeStringLookuper
StaticFile         fs.Node, fs.NodeOpener → staticHandle (buffer + offset)
StreamFile         fs.Node, fs.NodeOpener → streamHandle (io.ReadCloser)
ServiceStreamFile  fs.Node, fs.NodeOpener → streamHandle (tagged with [slot])
StatsFile          fs.Node, fs.NodeOpener → statsHandle  (fresh fetch per read)
```

**Static files** (`env`, `inspect`, `status`, `labels`, `node`) fetch data once on `Open()` and serve from an in-memory buffer with correct offset handling. No caching between `Open()` calls.

**Streaming files** (`stdout`, `stderr`, `logs`) call `ContainerLogs` or `ServiceLogs` with `Follow: true` on `Open()`. Each `Read()` returns the next chunk from the stream. `Release()` cancels the context and closes the reader.

**Service streaming files** are the same but enable `Details: true` in the API call. Each frame's `com.docker.swarm.task.name` attribute is parsed and replaced with a `[slot]` prefix on the log line.

**Stats files** make a fresh `ContainerStats(stream: true)` request on every `Read()`, read the first JSON frame, then close. CPU is computed as:

```
cpu_percent = (cpuDelta / systemDelta) × numCPUs × 100
```

Docker's multiplexed log stream (8-byte frame header) is stripped transparently by a `demuxReader` so callers receive plain text.

## Error mapping

| Docker error                         | FUSE errno |
| ------------------------------------ | ---------- |
| Container / service / node not found | `ENOENT`   |
| Swarm not active                     | `EPERM`    |
| Network / API error                  | `EIO`      |

## License

MIT
