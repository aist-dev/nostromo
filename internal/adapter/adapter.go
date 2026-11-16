// Package adapter turns agent command templates into shell commands for tmux.
package adapter

import (
	"fmt"
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
	// Args are extra arguments for the agent; each one reaches it as a separate
	// argument, byte for byte. Only start templates use them.
	Args []string
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

// FishQuote quotes s for fish, where a backslash inside single quotes escapes
// a backslash or a quote.
func FishQuote(s string) string {
	if s != "" && ShellQuote(s) == s {
		return s
	}
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}

func isFish(shell string) bool { return filepath.Base(shell) == "fish" }

// Quoter returns the quoting function for the shell that will parse a command.
func Quoter(shell string) func(string) string {
	if isFish(shell) {
		return FishQuote
	}
	return ShellQuote
}

// Render substitutes placeholders in tmpl with values quoted by quote
// (ShellQuote when nil). {args} becomes argsRef, the shell's reference to the
// positional parameters ("$@", or $argv in fish), so the arguments themselves
// never appear in the command; a template without {args} gets argsRef appended
// when v.Args is not empty.
func Render(tmpl string, v Values, quote func(string) string, argsRef string) (string, error) {
	if err := config.ValidateTemplate(tmpl); err != nil {
		return "", err
	}
	if quote == nil {
		quote = ShellQuote
	}
	if !strings.Contains(tmpl, config.ArgsPlaceholder) && len(v.Args) > 0 {
		tmpl += " " + config.ArgsPlaceholder
	}
	r := strings.NewReplacer(
		"{id}", quote(v.ID),
		"{resume_id}", quote(v.ResumeID),
		"{cwd}", quote(v.Cwd),
		config.ArgsPlaceholder, argsRef,
	)
	return r.Replace(tmpl), nil
}

// WriteArgs stores args in dir, one file per argument, for LoadArgs to read.
func WriteArgs(dir string, args []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for i, a := range args {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d", i)), []byte(a), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// LoadArgs is a /bin/sh snippet that sets the positional parameters to the
// arguments WriteArgs stored in dir, byte for byte, and removes dir. Arguments
// travel this way because tmux limits the length of a command.
func LoadArgs(dir string) string {
	d := ShellQuote(dir)
	return `set --; for f in ` + d + `/*; do [ -f "$f" ] || continue; a=$(cat "$f"; printf x); set -- "$@" "${a%x}"; done; rm -rf ` + d + `; `
}

// Wrap builds the command tmux runs in the new pane. With useShell the agent
// command runs through the user's interactive login shell so PATH and rc-file
// variables match a normal terminal; otherwise it is executed as is. The
// positional parameters of the outer /bin/sh are handed on to the login shell.
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
	name := " nostromo" // $0 of a POSIX shell; fish puts every argument in $argv
	if isFish(shell) {
		name = ""
	}
	return "exec " + ShellQuote(shell) + " " + flags + " " + ShellQuote("exec "+cmd) + name + ` "$@"`
}

// UserShell returns the user's login shell: $SHELL, else /bin/sh.
func UserShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// Command renders the start or resume template of an agent for /bin/sh -c.
// With shell wrapping the rendered template is parsed by shell, so values are
// quoted for it. v.Args must already be stored in argsDir with WriteArgs; resume
// never gets them.
func Command(a config.Agent, resume bool, v Values, shell, argsDir string) (string, error) {
	tmpl := a.Start
	if resume {
		tmpl = a.Resume
		v.Args = nil
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	quote, ref := ShellQuote, `"$@"`
	if a.WrapInShell() {
		quote = Quoter(shell)
		if isFish(shell) {
			ref = "$argv"
		}
	}
	cmd, err := Render(tmpl, v, quote, ref)
	if err != nil {
		return "", err
	}
	cmd = Wrap(cmd, shell, a.WrapInShell())
	if len(v.Args) > 0 {
		cmd = LoadArgs(argsDir) + cmd
	}
	return cmd, nil
}
