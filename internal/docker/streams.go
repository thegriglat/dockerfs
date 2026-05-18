package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"

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

// ServiceLogsStreamTagged opens a streaming log reader for a Swarm service.
// Each log line is prefixed with [slot] extracted from com.docker.swarm.task.name.
func (cl *Client) ServiceLogsStreamTagged(ctx context.Context, serviceID string, stdout, stderr bool) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: stdout,
		ShowStderr: stderr,
		Follow:     true,
		Details:    true,
	}
	rc, err := cl.c.ServiceLogs(ctx, serviceID, opts)
	if err != nil {
		return nil, MapErr(err, "ServiceLogs")
	}
	return &taggedServiceLogReader{src: rc}, nil
}

// taggedServiceLogReader reads Docker service logs with Details:true and
// prepends [slot] to each frame using com.docker.swarm.task.name.
type taggedServiceLogReader struct {
	src    io.ReadCloser
	outBuf []byte
}

func (r *taggedServiceLogReader) Read(p []byte) (int, error) {
	for len(r.outBuf) == 0 {
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(r.src, hdr); err != nil {
			return 0, err
		}
		size := int(binary.BigEndian.Uint32(hdr[4:]))
		if size == 0 {
			continue
		}
		frame := make([]byte, size)
		if _, err := io.ReadFull(r.src, frame); err != nil {
			return 0, err
		}
		r.outBuf = tagServiceFrame(frame)
	}
	n := copy(p, r.outBuf)
	r.outBuf = r.outBuf[n:]
	return n, nil
}

func (r *taggedServiceLogReader) Close() error { return r.src.Close() }

// tagServiceFrame parses "key=val,key2=val2 logline" and returns "[slot] logline".
func tagServiceFrame(frame []byte) []byte {
	idx := bytes.IndexByte(frame, ' ')
	if idx < 0 {
		return frame
	}
	slot := extractTaskSlot(string(frame[:idx]))
	out := make([]byte, 0, 4+len(slot)+len(frame)-idx)
	out = append(out, '[')
	out = append(out, slot...)
	out = append(out, ']', ' ')
	out = append(out, frame[idx+1:]...)
	return out
}

// extractTaskSlot parses com.docker.swarm.task.name=svcname.slot from attrs.
func extractTaskSlot(attrs string) string {
	for _, kv := range strings.Split(attrs, ",") {
		v, ok := strings.CutPrefix(kv, "com.docker.swarm.task.name=")
		if !ok {
			continue
		}
		if i := strings.LastIndex(v, "."); i >= 0 {
			return v[i+1:]
		}
		return v
	}
	return "?"
}
