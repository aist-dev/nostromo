// Package adapter turns agent command templates into shell commands for tmux.
package adapter

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/aist-dev/nostromo/internal/config"
)

// Values are the substitutions available in command templates.
type Values struct {
	ID       string
	ResumeID string
	Cwd      string
}

// ShellQuote quotes s for POSIX shells (and fish) using single quotes.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./=:,+@%", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Render substitutes placeholders in tmpl with shell-quoted values.
func Render(tmpl string, v Values) (string, error) {
	if err := config.ValidateTemplate(tmpl); err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		"{id}", ShellQuote(v.ID),
		"{resume_id}", ShellQuote(v.ResumeID),
		"{cwd}", ShellQuote(v.Cwd),
	)
	return r.Replace(tmpl), nil
}

// Wrap builds the command tmux runs in the new pane. With useShell the agent
// command runs through the user's interactive login shell so PATH and rc-file
// variables match a normal terminal; otherwise it is executed as is.
func Wrap(cmd, shell string, useShell bool) string {
	if !useShell {
		return cmd
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	flags := "-lic"
	if filepath.Base(shell) == "sh" || filepath.Base(shell) == "dash" {
		// POSIX sh has no -l on every platform; -i is enough to read $ENV.
		flags = "-ic"
	}
	return "exec " + ShellQuote(shell) + " " + flags + " " + ShellQuote("exec "+cmd)
}

// UserShell returns the user's login shell: $SHELL, else /bin/sh.
func UserShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// Command renders the start or resume template of an agent and wraps it.
func Command(a config.Agent, resume bool, v Values, shell string) (string, error) {
	tmpl := a.Start
	if resume {
		tmpl = a.Resume
	}
	cmd, err := Render(tmpl, v)
	if err != nil {
		return "", err
	}
	return Wrap(cmd, shell, a.WrapInShell()), nil
}
