// Package tmux wraps the tmux command line on a dedicated socket.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ChatOption is the session user option that links a tmux session to a chat record.
const ChatOption = "@nostromo_chat"

// MinMajor and MinMinor are the oldest supported tmux version.
const (
	MinMajor = 3
	MinMinor = 2
)

// Conf is the tmux configuration written to ~/.nostromo/tmux.conf.
const Conf = `# tmux configuration for the nostromo socket. Written by "agent setup".
set -g mouse on
set -g history-limit 50000
set -g default-terminal "tmux-256color"
set -g escape-time 10
set -g status off
set -g set-titles off
set -g focus-events on
# Copies (mouse selection, OSC 52 from the agent) reach the browser clipboard.
set -g set-clipboard on
set -ga terminal-overrides ",xterm-256color:Tc"
`

// ErrNoSession is returned when the target session does not exist.
var ErrNoSession = errors.New("tmux session not found")

// Error is a failed tmux invocation.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return "tmux " + strings.Join(e.Args, " ") + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// Runner executes a program and returns its stdout and stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

// Run executes name with args.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// API is what the rest of nostromo needs from tmux. Tests use fakes.
type API interface {
	Version(ctx context.Context) (Version, error)
	NewSession(ctx context.Context, spec SessionSpec) error
	ListPanes(ctx context.Context) ([]Pane, error)
	KillSession(ctx context.Context, name string) error
	Scroll(ctx context.Context, name, dir string) error
	AttachArgs(name string) []string
	SwitchClient(ctx context.Context, name string) error
	Capture(ctx context.Context, name string) (string, error)
	SourceFile(ctx context.Context, path string) error
}

// Client talks to one tmux server, selected by socket name.
type Client struct {
	Bin    string // tmux binary, default "tmux"
	Socket string // -L socket name
	Config string // -f config file, used when the server starts
	Run    Runner
}

// New returns a client for socket using the real tmux binary.
func New(socket, configPath string) *Client {
	return &Client{Bin: "tmux", Socket: socket, Config: configPath, Run: ExecRunner{}}
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "tmux"
	}
	return c.Bin
}

// BaseArgs are the global flags selecting the socket and config.
func (c *Client) BaseArgs() []string {
	args := []string{"-L", c.Socket}
	if c.Config != "" {
		args = append(args, "-f", c.Config)
	}
	return args
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	full := append(c.BaseArgs(), args...)
	out, errb, err := c.Run.Run(ctx, c.bin(), full...)
	if err != nil {
		return "", &Error{Args: args, Stderr: string(errb), Err: err}
	}
	return string(out), nil
}

// noServer reports whether stderr says the tmux server is not running.
func noServer(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	s := strings.ToLower(te.Stderr)
	return strings.Contains(s, "no server running") || strings.Contains(s, "error connecting to") ||
		strings.Contains(s, "no such file or directory")
}

func noSession(err error) bool {
	var te *Error
	if !errors.As(err, &te) {
		return false
	}
	s := strings.ToLower(te.Stderr)
	return strings.Contains(s, "can't find session") || strings.Contains(s, "session not found") || noServer(err)
}

// Version is a parsed tmux version.
type Version struct {
	Major, Minor int
	Raw          string
}

// AtLeast reports whether v >= major.minor.
func (v Version) AtLeast(major, minor int) bool {
	return v.Major > major || v.Major == major && v.Minor >= minor
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// ParseVersion parses `tmux -V` output such as "tmux 3.2a", "tmux 3.5" or "tmux next-3.6".
// Development builds without a number ("tmux master") count as very new.
func ParseVersion(out string) (Version, error) {
	raw := strings.TrimSpace(out)
	m := versionRe.FindStringSubmatch(raw)
	if m == nil {
		if strings.Contains(raw, "master") {
			return Version{Major: 999, Raw: raw}, nil
		}
		return Version{}, fmt.Errorf("cannot parse tmux version %q", raw)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return Version{Major: major, Minor: minor, Raw: raw}, nil
}

// Version runs `tmux -V`.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, errb, err := c.Run.Run(ctx, c.bin(), "-V")
	if err != nil {
		return Version{}, &Error{Args: []string{"-V"}, Stderr: string(errb), Err: err}
	}
	return ParseVersion(string(out))
}

// CheckVersion returns an error when tmux is missing or older than 3.2.
func CheckVersion(ctx context.Context, t API) (Version, error) {
	v, err := t.Version(ctx)
	if err != nil {
		return v, fmt.Errorf("tmux not found or not working: %w", err)
	}
	if !v.AtLeast(MinMajor, MinMinor) {
		return v, fmt.Errorf("tmux %d.%d or newer is required, found %q", MinMajor, MinMinor, v.Raw)
	}
	return v, nil
}

// SessionSpec describes a new chat session.
type SessionSpec struct {
	Name    string
	ChatID  string
	Cwd     string
	Command string
	Cols    int
	Rows    int
	Env     []string // KEY=VALUE pairs set in the session environment
}

// NewSessionArgs builds the single tmux invocation that creates the session and sets
// its options, so the options apply before the command can exit.
func NewSessionArgs(s SessionSpec) []string {
	cols, rows := s.Cols, s.Rows
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 40
	}
	// tmux expands formats in -c, so "#(cmd)" in a directory name would run cmd;
	// "##" is a literal "#".
	args := []string{"new-session", "-d", "-s", s.Name, "-c", strings.ReplaceAll(s.Cwd, "#", "##"),
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)}
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	// Several arguments make tmux exec the command directly, so /bin/sh parses
	// it no matter what default-shell (the user's $SHELL) is.
	args = append(args, "/bin/sh", "-c", s.Command,
		";", "set-option", "-t", s.Name, ChatOption, s.ChatID,
		";", "set-option", "-w", "-t", s.Name, "remain-on-exit", "failed",
	)
	return args
}

// NewSession creates a detached session.
func (c *Client) NewSession(ctx context.Context, s SessionSpec) error {
	_, err := c.run(ctx, NewSessionArgs(s)...)
	return err
}

// Pane is one row of `list-panes -a`.
type Pane struct {
	Session    string
	ChatID     string
	Dead       bool
	DeadStatus int
	Attached   int
	PID        int
	// Unreaped is a dead pane whose process tmux has not collected yet, so its
	// exit status is unknown.
	Unreaped bool
}

// PanesFormat is the -F format for ListPanes; fields are tab separated.
const PanesFormat = "#{session_name}\t#{" + ChatOption + "}\t#{pane_dead}\t#{pane_dead_status}\t#{session_attached}\t#{pane_pid}\t#{pane_dead_signal}"

// ParsePanes parses ListPanes output. Malformed lines are skipped.
func ParsePanes(out string) []Pane {
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		status, _ := strconv.Atoi(f[3])
		attached, _ := strconv.Atoi(f[4])
		pid, _ := strconv.Atoi(f[5])
		p := Pane{Session: f[0], ChatID: f[1], Dead: f[2] == "1", DeadStatus: status, Attached: attached, PID: pid}
		signal := ""
		if len(f) > 6 {
			signal = f[6]
		}
		if p.Dead && f[3] == "" {
			if sig, err := strconv.Atoi(signal); err == nil && sig > 0 {
				p.DeadStatus = 128 + sig
			} else {
				p.Unreaped = true
			}
		}
		panes = append(panes, p)
	}
	return panes
}

// ListPanes lists every pane on the server. No server running means no panes.
//
// tmux occasionally misses SIGCHLD for a pane whose process exited: the pane is
// dead but the process stays a zombie with no exit status, so remain-on-exit
// cannot tell success from failure. Running a short job makes the server handle
// SIGCHLD again, which reaps every zombie; then the panes are listed once more.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	panes, err := c.listPanes(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range panes {
		if p.Unreaped {
			if _, err := c.run(ctx, "run-shell", "true"); err != nil && !noServer(err) {
				return panes, nil
			}
			return c.listPanes(ctx)
		}
	}
	return panes, nil
}

func (c *Client) listPanes(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", PanesFormat)
	if err != nil {
		if noServer(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParsePanes(out), nil
}

// KillSession ends a session. A missing session returns ErrNoSession.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session", "-t", "="+name)
	if err != nil && noSession(err) {
		return ErrNoSession
	}
	return err
}

// KillServer stops the whole tmux server on this socket.
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	if err != nil && noServer(err) {
		return nil
	}
	return err
}

// SourceFile reloads a config file into a running server. No server is not an error:
// the next server reads the file on start.
func (c *Client) SourceFile(ctx context.Context, path string) error {
	_, err := c.run(ctx, "source-file", path)
	if err != nil && noServer(err) {
		return nil
	}
	return err
}

// SwitchClient switches the current tmux client to a session.
func (c *Client) SwitchClient(ctx context.Context, name string) error {
	_, err := c.run(ctx, "switch-client", "-t", "="+name)
	return err
}

// AttachArgs returns the full argv that attaches a terminal to a session.
func (c *Client) AttachArgs(name string) []string {
	return append(append([]string{c.bin()}, c.BaseArgs()...), "attach-session", "-t", "="+name)
}

// Scroll moves through a session's history: dir is "up", "down" or "exit".
func (c *Client) Scroll(ctx context.Context, name, dir string) error {
	target := "=" + name + ":"
	var err error
	switch dir {
	case "up":
		if _, err = c.run(ctx, "copy-mode", "-t", target); err == nil {
			_, err = c.run(ctx, "send-keys", "-t", target, "-X", "halfpage-up")
		}
	case "down":
		if _, err = c.run(ctx, "copy-mode", "-t", target); err == nil {
			_, err = c.run(ctx, "send-keys", "-t", target, "-X", "halfpage-down")
		}
	case "exit":
		_, err = c.run(ctx, "send-keys", "-t", target, "-X", "cancel")
		if err != nil && strings.Contains(err.Error(), "not in a mode") {
			err = nil
		}
	default:
		return fmt.Errorf("unknown scroll direction %q", dir)
	}
	if err != nil && noSession(err) {
		return ErrNoSession
	}
	return err
}

// CaptureLines is how much history Capture returns.
const CaptureLines = 2000

// Capture returns the pane's history and screen with colors, as terminal output
// (lines end with CRLF).
func (c *Client) Capture(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "capture-pane", "-p", "-e", "-J", "-S", "-"+strconv.Itoa(CaptureLines), "-t", "="+name+":")
	if err != nil {
		if noSession(err) {
			return "", ErrNoSession
		}
		return "", err
	}
	return strings.ReplaceAll(strings.TrimRight(out, "\n"), "\n", "\r\n") + "\r\n", nil
}

// InsideSocket reports whether a $TMUX value refers to a server on the given socket name.
func InsideSocket(tmuxEnv, socket string) bool {
	if tmuxEnv == "" {
		return false
	}
	path, _, _ := strings.Cut(tmuxEnv, ",")
	return filepath.Base(path) == socket
}
