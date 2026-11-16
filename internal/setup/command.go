package setup

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tailscale"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/version"
)

// NewInstaller builds an installer for the real machine.
func NewInstaller(env cli.Env) (*Installer, error) {
	paths, err := env.Paths()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.Config())
	if err != nil {
		cfg = config.Default()
	}
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
	}
	username := env.Getenv("USER")
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	in := &Installer{
		Paths: paths, Home: env.Getenv("HOME"), Shell: env.Getenv("SHELL"), User: username,
		GOOS: runtime.GOOS, CmdName: env.Getenv("NOSTROMO_CMD"), PathEnv: env.Getenv("PATH"),
		Exe: exe, Version: version.Version, Out: env.Stdout,
		Tmux: env.NewTmux(cfg, paths), Run: tmux.ExecRunner{},
		Health: HTTPHealth, HTTPSCheck: HTTPSReachable,
	}
	if in.Shell == "" {
		in.Shell = "/bin/sh"
	}
	switch runtime.GOOS {
	case "linux":
		in.Service = &Systemd{Home: in.Home, User: username, Run: tmux.ExecRunner{}}
	case "darwin":
		in.Service = &Launchd{Home: in.Home, UID: os.Getuid(), Run: tmux.ExecRunner{}}
	}
	if ts, err := tailscale.Find(); err == nil {
		in.Tailscale = ts
	}
	return in, nil
}

// Commands returns the setup, doctor and uninstall subcommands. build creates the
// installer, so tests can substitute fakes.
func Commands(build func(cli.Env) (*Installer, error)) map[string]cli.Command {
	if build == nil {
		build = NewInstaller
	}
	return map[string]cli.Command{
		"setup": func(ctx context.Context, args []string, env cli.Env) int {
			fs := flag.NewFlagSet("agent setup", flag.ContinueOnError)
			fs.SetOutput(env.Stderr)
			var o Options
			fs.BoolVar(&o.Upgrade, "upgrade", false, "migrate data and refresh the service and hooks (used by agent update)")
			fs.BoolVar(&o.NoService, "no-service", false, "do not install the background service")
			fs.BoolVar(&o.NoTailscale, "no-tailscale", false, "do not configure tailscale serve")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			in, err := build(env)
			if err == nil {
				err = in.Setup(ctx, o)
			}
			if err != nil {
				fmt.Fprintf(env.Stderr, "agent setup: %v\n", err)
				return 1
			}
			return 0
		},
		"doctor": func(ctx context.Context, args []string, env cli.Env) int {
			in, err := build(env)
			if err != nil {
				fmt.Fprintf(env.Stderr, "agent doctor: %v\n", err)
				return 1
			}
			if !in.Doctor(ctx) {
				return 1
			}
			return 0
		},
		"uninstall": func(ctx context.Context, args []string, env cli.Env) int {
			fs := flag.NewFlagSet("agent uninstall", flag.ContinueOnError)
			fs.SetOutput(env.Stderr)
			purge := fs.Bool("purge", false, "also delete ~/.nostromo with all chat records")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			in, err := build(env)
			if err == nil {
				err = in.Uninstall(ctx, *purge)
			}
			if err != nil {
				fmt.Fprintf(env.Stderr, "agent uninstall: %v\n", err)
				return 1
			}
			return 0
		},
	}
}
