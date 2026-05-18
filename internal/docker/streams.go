package docker

import (
	"context"
	"encoding/binary"
	"io"

	"github.com/docker/docker/api/types/container"
)

// ContainerLogsStream opens a streaming log reader for one stream type.
// stdoutOnly/stderrOnly selects which multiplexed stream to expose.
func (cl *Client) ContainerLogsStream(ctx context.Context, id string, stdout, stderr bool) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: stdout,
		ShowStderr: stderr,
		Follow:     true,
		Timestamps: false,
	}
	rc, err := cl.c.ContainerLogs(ctx, id, opts)
	if err != nil {
		return nil, MapErr(err, "ContainerLogs")
	}
	return &demuxReader{src: rc}, nil
}

// ContainerLogsOnce fetches all logs without following (for jobs/inspect).
func (cl *Client) ContainerLogsOnce(ctx context.Context, id string, stdout, stderr bool) ([]byte, error) {
	opts := container.LogsOptions{
		ShowStdout: stdout,
		ShowStderr: stderr,
		Follow:     false,
	}
	rc, err := cl.c.ContainerLogs(ctx, id, opts)
	if err != nil {
		return nil, MapErr(err, "ContainerLogsOnce")
	}
	defer rc.Close()
	data, err := io.ReadAll(&demuxReader{src: rc})
	if err != nil {
		return nil, MapErr(err, "ContainerLogsOnce/read")
	}
	return data, nil
}

// demuxReader strips the 8-byte Docker multiplexing header from each frame.
type demuxReader struct {
	src    io.ReadCloser
	buf    []byte
	remain int
}

func (d *demuxReader) Read(p []byte) (int, error) {
	for {
		if d.remain > 0 {
			n := d.remain
			if n > len(p) {
				n = len(p)
			}
			if len(d.buf) < n {
				d.buf = make([]byte, n)
			}
			nr, err := io.ReadFull(d.src, d.buf[:n])
			d.remain -= nr
			copy(p, d.buf[:nr])
			return nr, err
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(d.src, hdr); err != nil {
			return 0, err
		}
		d.remain = int(binary.BigEndian.Uint32(hdr[4:]))
	}
}

func (d *demuxReader) Close() error { return d.src.Close() }

// ServiceLogsStream opens a streaming log reader for a Swarm service.
func (cl *Client) ServiceLogsStream(ctx context.Context, serviceID string) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
	}
	rc, err := cl.c.ServiceLogs(ctx, serviceID, opts)
	if err != nil {
		return nil, MapErr(err, "ServiceLogs")
	}
	return &demuxReader{src: rc}, nil
}
