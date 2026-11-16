package update

import (
	"context"
	"flag"
	"fmt"
	"runtime"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/setup"
	"github.com/aist-dev/nostromo/internal/version"
)

// Environment overrides. The download URL is shared with install.sh; both exist
// for forks, mirrors and tests.
const (
	TokenEnv       = "NOSTROMO_GITHUB_TOKEN"
	APIURLEnv      = "NOSTROMO_API_URL"
	DownloadURLEnv = "NOSTROMO_DOWNLOAD_URL"
)

func envOr(env cli.Env, key, def string) string {
	if v := env.Getenv(key); v != "" {
		return v
	}
	return def
}

// NewUpdater builds an updater for the real machine.
func NewUpdater(env cli.Env) (*Updater, error) {
	in, err := setup.NewInstaller(env)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(in.Paths.Config())
	if err != nil {
		return nil, err
	}
	u := &Updater{
		Paths: in.Paths, Repo: cfg.Repo, APIBase: envOr(env, APIURLEnv, "https://api.github.com"),
		DownloadBase: envOr(env, DownloadURLEnv, "https://github.com/"+cfg.Repo+"/releases"),
		Token:        env.Getenv(TokenEnv), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Current: version.Version, Port: cfg.Port, Out: env.Stdout,
		Health: setup.HTTPHealth, RunBinary: ExecBinary,
	}
	if in.Service != nil {
		u.Service = in.Service
	}
	return u, nil
}

// Command implements `agent update`.
func Command(build func(cli.Env) (*Updater, error)) cli.Command {
	if build == nil {
		build = NewUpdater
	}
	return func(ctx context.Context, args []string, env cli.Env) int {
		fs := flag.NewFlagSet("agent update", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		var o Options
		fs.BoolVar(&o.Check, "check", false, "only report whether a newer version exists")
		fs.StringVar(&o.Version, "version", "", "install this version, e.g. v1.2.3")
		fs.BoolVar(&o.Rollback, "rollback", false, "restore the previous version")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		u, err := build(env)
		if err == nil {
			err = u.Run(ctx, o)
		}
		if err != nil {
			fmt.Fprintf(env.Stderr, "agent update: %v\n", err)
			return 1
		}
		return 0
	}
}
