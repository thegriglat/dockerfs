// Package docker wraps the Docker SDK client with FUSE-friendly error mapping.
package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"bazil.org/fuse"
	dockerclient "github.com/docker/docker/client"
)

// Client wraps the Docker SDK client.
type Client struct {
	c *dockerclient.Client
}

// New creates a Docker client from environment variables and negotiates API version.
func New() (*Client, error) {
	c, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Client{c: c}, nil
}

// Ping checks Docker daemon availability.
func (cl *Client) Ping(ctx context.Context) error {
	_, err := cl.c.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}
	return nil
}

// Raw returns the underlying Docker SDK client.
func (cl *Client) Raw() *dockerclient.Client {
	return cl.c
}

// MapErr converts Docker SDK errors to FUSE errors.
func MapErr(err error, op string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	slog.Error("docker error", "op", op, "err", err)
	switch {
	case dockerclient.IsErrNotFound(err):
		return fuse.ENOENT
	case strings.Contains(msg, "swarm") && strings.Contains(msg, "not"):
		return fuse.EPERM
	default:
		return fuse.EIO
	}
}
