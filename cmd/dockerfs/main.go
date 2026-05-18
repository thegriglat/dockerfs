package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/spf13/pflag"

	"dockerfs/internal/docker"
	dockerfs "dockerfs/internal/fs"
)

func main() {
	mountpoint := pflag.StringP("mountpoint", "m", "/mnt/dockerfs", "FUSE mount point")
	debug := pflag.BoolP("debug", "v", false, "enable FUSE debug logging")
	allowOther := pflag.BoolP("allow-other", "o", false, "allow other users to access the mountpoint\n(requires user_allow_other in /etc/fuse.conf)")
	daemon := pflag.BoolP("daemon", "d", false, "detach and run in background")
	// internal flag: set when re-exec'd as background child
	child := pflag.Bool("child", false, "")
	pflag.CommandLine.MarkHidden("child")
	pflag.Parse()

	if *daemon && !*child {
		daemonize()
		return
	}

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	cl, err := docker.New()
	if err != nil {
		slog.Error("failed to create docker client", "err", err)
		os.Exit(1)
	}

	if err := cl.Ping(context.Background()); err != nil {
		slog.Error("docker daemon unreachable", "err", err)
		os.Exit(1)
	}
	slog.Info("docker daemon connected")

	mountOpts := []fuse.MountOption{
		fuse.FSName("dockerfs"),
		fuse.Subtype("dockerfs"),
	}
	if *allowOther {
		mountOpts = append(mountOpts, fuse.AllowOther())
	}
	if *debug {
		fuse.Debug = func(msg interface{}) { slog.Debug("fuse", "msg", msg) }
	}

	conn, err := fuse.Mount(*mountpoint, mountOpts...)
	if err != nil {
		slog.Error("mount failed", "mountpoint", *mountpoint, "err", err)
		os.Exit(1)
	}
	slog.Info("mounted", "mountpoint", *mountpoint)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		slog.Info("unmounting", "mountpoint", *mountpoint)
		if err := fuse.Unmount(*mountpoint); err != nil {
			slog.Error("unmount error", "err", err)
		}
	}()

	root := dockerfs.NewRoot(cl)
	if err := fs.Serve(conn, dockerfs.NewFS(root)); err != nil {
		slog.Error("serve error", "err", err)
		os.Exit(1)
	}
}

// daemonize re-execs the current binary with --child appended and detaches it
// from the terminal. The parent prints the child PID and exits.
func daemonize() {
	exe, err := os.Executable()
	if err != nil {
		slog.Error("cannot resolve executable path", "err", err)
		os.Exit(1)
	}

	args := append(os.Args[1:], "--child")
	cmd := exec.Command(exe, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		slog.Error("failed to start background process", "err", err)
		os.Exit(1)
	}
	slog.Info("dockerfs started in background", "pid", cmd.Process.Pid)
}
