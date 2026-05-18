// Package fs implements the FUSE filesystem nodes for dockerfs.
package fs

import (
	"context"
	"log/slog"
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"dockerfs/internal/docker"
)

// Root is the top-level FUSE directory (/). It routes to /local and /swarm.
type Root struct {
	cl *docker.Client
}

// NewRoot creates a Root node.
func NewRoot(cl *docker.Client) *Root {
	return &Root{cl: cl}
}

var _ fs.Node = (*Root)(nil)
var _ fs.HandleReadDirAller = (*Root)(nil)
var _ fs.NodeStringLookuper = (*Root)(nil)

func (r *Root) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (r *Root) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Inode: 2, Name: "local", Type: fuse.DT_Dir},
		{Inode: 3, Name: "swarm", Type: fuse.DT_Dir},
	}, nil
}

func (r *Root) Lookup(ctx context.Context, name string) (fs.Node, error) {
	switch name {
	case "local":
		return NewLocalDir(r.cl), nil
	case "swarm":
		return NewSwarmDir(r.cl), nil
	}
	slog.Debug("root lookup miss", "name", name)
	return nil, fuse.ENOENT
}

// FS implements fs.FS and returns the Root node.
type FS struct {
	root *Root
}

// NewFS creates an FS from a Root node.
func NewFS(root *Root) FS {
	return FS{root: root}
}

func (f FS) Root() (fs.Node, error) {
	return f.root, nil
}
