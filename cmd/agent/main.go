// Command agent launches CLI agents in tmux and serves them to the browser.
package main

import (
	"os"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/server"
	"github.com/aist-dev/nostromo/internal/server/web"
)

func main() {
	env := cli.DefaultEnv()
	env.Commands["serve"] = server.Command(func(app *cli.App) server.Extras {
		return server.Extras{Static: web.Handler()}
	})
	os.Exit(cli.Main(os.Args[1:], env))
}
