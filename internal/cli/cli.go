// Package cli parses the agent command line and dispatches subcommands.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/aist-dev/nostromo/internal/version"
)

// Env is the process environment the CLI runs in; tests replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// DefaultEnv returns the real process environment.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, env Env) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version":
			fmt.Fprintln(env.Stdout, version.Version)
			return 0
		}
	}
	fmt.Fprintln(env.Stderr, "agent: not implemented yet")
	return 2
}
