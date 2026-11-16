// Command agent launches CLI agents in tmux and serves them to the browser.
package main

import (
	"os"

	"github.com/aist-dev/nostromo/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.DefaultEnv()))
}
