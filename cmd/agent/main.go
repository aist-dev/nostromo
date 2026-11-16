// Command agent launches CLI agents in tmux and serves them to the browser.
package main

import (
	"context"
	"log"
	"os"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/hook"
	"github.com/aist-dev/nostromo/internal/server"
	"github.com/aist-dev/nostromo/internal/server/web"
	"github.com/aist-dev/nostromo/internal/setup"
	"github.com/aist-dev/nostromo/internal/update"
	"github.com/aist-dev/nostromo/internal/version"
)

func main() {
	env := cli.DefaultEnv()
	env.Commands["serve"] = server.Command(serverExtras)
	env.Commands["hook"] = hook.Command
	env.Commands["update"] = update.Command(nil)
	for name, cmd := range setup.Commands(nil) {
		env.Commands[name] = cmd
	}
	os.Exit(cli.Main(os.Args[1:], env))
}

// serverExtras plugs the UI and the update check into the server.
func serverExtras(app *cli.App) server.Extras {
	checker := &update.Checker{
		Paths: app.Paths, Current: version.Version,
		Enabled: app.Config.CheckUpdates, Auto: app.Config.AutoUpdate,
		Spawn: func() error { return update.SpawnDetached(app.Paths) },
	}
	checker.Latest = func(ctx context.Context) (string, error) {
		u, err := update.NewUpdater(app.Env)
		if err != nil {
			return "", err
		}
		return u.Latest(ctx)
	}
	ex := server.Extras{
		Static:     web.Handler(),
		Background: func(ctx context.Context, logger *log.Logger) { checker.Loop(ctx, logger, 0) },
	}
	if app.Config.CheckUpdates {
		ex.UpdateAvailable = checker.Available
		ex.StartUpdate = checker.Spawn
	}
	return ex
}
