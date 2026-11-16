// Package cli parses the agent command line and dispatches subcommands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/creack/pty"

	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/version"
)

// Env is the process environment the CLI runs in; tests replace parts of it.
type Env struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Getenv   func(string) string
	Environ  func() []string
	Getwd    func() (string, error)
	IsTTY    func() bool
	TermSize func() (cols, rows int, ok bool)
	// Exec replaces the current process with argv. It only returns on error.
	Exec    func(argv []string, env []string) error
	NewTmux func(cfg config.Config, paths config.Paths) tmux.API
	Now     func() time.Time
	// Commands are subcommands provided by other packages (serve, setup, ...).
	Commands map[string]Command
}

// Command is a subcommand implemented outside this package.
type Command func(ctx context.Context, args []string, env Env) int

// DefaultEnv returns the real process environment.
func DefaultEnv() Env {
	return Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Environ: os.Environ, Getwd: os.Getwd,
		IsTTY: func() bool {
			for _, f := range []*os.File{os.Stdin, os.Stdout} {
				fi, err := f.Stat()
				if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
					return false
				}
			}
			return true
		},
		TermSize: func() (int, int, bool) {
			rows, cols, err := pty.Getsize(os.Stdout)
			return cols, rows, err == nil && cols > 0 && rows > 0
		},
		Exec: func(argv []string, env []string) error {
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return err
			}
			return syscall.Exec(path, argv, env)
		},
		NewTmux: func(cfg config.Config, paths config.Paths) tmux.API {
			return tmux.New(cfg.TmuxSocket, paths.TmuxConf())
		},
		Now:      time.Now,
		Commands: map[string]Command{},
	}
}

// Paths resolves the nostromo home from the environment.
func (e Env) Paths() (config.Paths, error) {
	if h := e.Getenv(config.HomeEnv); h != "" {
		return config.Paths{Home: h}, nil
	}
	home := e.Getenv("HOME")
	if home == "" {
		return config.Paths{}, errors.New("HOME is not set")
	}
	return config.Paths{Home: filepath.Join(home, ".nostromo")}, nil
}

// App is everything a launcher command needs.
type App struct {
	Env    Env
	Paths  config.Paths
	Config config.Config
	Tmux   tmux.API
	Chats  *chat.Service
}

// NewApp loads config and wires the chat service.
func NewApp(env Env) (*App, error) {
	paths, err := env.Paths()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.Config())
	if err != nil {
		return nil, err
	}
	t := env.NewTmux(cfg, paths)
	svc := chat.New(store.New(paths.Chats()), t, cfg, paths)
	if s := env.Getenv("SHELL"); s != "" {
		svc.Shell = s
	}
	return &App{Env: env, Paths: paths, Config: cfg, Tmux: t, Chats: svc}, nil
}

const usage = `Usage:
  agent [--nostr-<flag>...] [<agent args>...]   start a new chat and attach
  agent ls                       list chats by directory
  agent attach <id>              attach to a running chat
  agent resume <id>              resume a stopped chat and attach
  agent stop <id>                end a chat's tmux session, keep the record
  agent rm <id>                  end the session and delete the chat
  agent serve                    run the web server (started by the service)
  agent doctor | update | uninstall | version
  agent hook <adapter>           internal: called by agent hooks

Flags of a new chat (they may appear anywhere; "--nostr-x=v" works too):
  --nostr-agent <name>   agent from config.toml (default: default_agent)
  --nostr-cwd <dir>      working directory (default: the current one)
  --nostr-title <text>   chat title (default: set from the first prompt)
  --nostr-detach         do not attach; print the chat id

Every other argument goes to the agent's start command unchanged, e.g.
  agent --nostr-detach --model opus "Fix the failing test"
Arguments that would break chat tracking (deny_args in config.toml) are refused.
A resumed chat does not get them again.

<id> may be any unambiguous prefix of a chat id.
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, env Env) int {
	ctx := context.Background()
	if len(args) > 0 && isCommand(args[0], env) {
		cmd, rest := args[0], args[1:]
		switch cmd {
		case "version":
			fmt.Fprintln(env.Stdout, version.Version)
			return 0
		case "help":
			fmt.Fprint(env.Stdout, usage)
			return 0
		case "ls", "list":
			return withApp(env, func(a *App) error { return a.List(ctx) })
		case "attach", "resume", "stop", "rm":
			if len(rest) != 1 {
				fmt.Fprintf(env.Stderr, "agent %s: expected one chat id\n", cmd)
				return 2
			}
			return withApp(env, func(a *App) error {
				switch cmd {
				case "attach":
					return a.Attach(ctx, rest[0])
				case "resume":
					return a.Resume(ctx, rest[0])
				case "stop":
					return a.Stop(ctx, rest[0])
				default:
					return a.Remove(ctx, rest[0])
				}
			})
		}
		return env.Commands[cmd](ctx, rest, env)
	}
	if len(args) == 1 {
		switch args[0] {
		case "--version":
			fmt.Fprintln(env.Stdout, version.Version)
			return 0
		case "-h", "--help":
			fmt.Fprint(env.Stdout, usage)
			return 0
		}
	}
	return newChat(ctx, args, env)
}

var builtins = map[string]bool{"version": true, "help": true, "ls": true, "list": true, "attach": true, "resume": true, "stop": true, "rm": true}

// isCommand reports whether arg names a subcommand; anything else starts a chat
// and is passed to the agent.
func isCommand(arg string, env Env) bool {
	if builtins[arg] {
		return true
	}
	_, ok := env.Commands[arg]
	return ok
}

func withApp(env Env, fn func(*App) error) int {
	a, err := NewApp(env)
	if err == nil {
		err = fn(a)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "agent: %v\n", err)
		return 1
	}
	return 0
}

// FlagPrefix starts every launcher flag, so they never collide with the flags
// of an agent.
const FlagPrefix = "--nostr-"

// launch holds the parsed new-chat command line.
type launch struct {
	agent, cwd, title string
	detach            bool
	args              []string
}

// renamed are launcher flags of nostromo 0.1 that now pass through; they are
// refused so old scripts fail loudly instead of handing them to the agent.
var renamed = []string{"--cwd", "--title", "--detach"}

func parseLaunch(args []string) (launch, error) {
	var l launch
	for i := 0; i < len(args); i++ {
		arg := args[i]
		for _, old := range renamed {
			if arg == old || strings.HasPrefix(arg, old+"=") {
				return l, fmt.Errorf("%s is now %s%s", old, FlagPrefix, old[2:])
			}
		}
		if !strings.HasPrefix(arg, FlagPrefix) {
			l.args = append(l.args, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg[len(FlagPrefix):], "=")
		var dst *string
		switch name {
		case "detach":
			if hasValue {
				return l, fmt.Errorf("%sdetach takes no value", FlagPrefix)
			}
			l.detach = true
			continue
		case "agent":
			dst = &l.agent
		case "cwd":
			dst = &l.cwd
		case "title":
			dst = &l.title
		default:
			return l, fmt.Errorf("unknown flag %s%s", FlagPrefix, name)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return l, fmt.Errorf("%s%s needs a value", FlagPrefix, name)
			}
			i++
			value = args[i]
		}
		*dst = value
	}
	return l, nil
}

func newChat(ctx context.Context, args []string, env Env) int {
	l, err := parseLaunch(args)
	if err != nil {
		fmt.Fprintf(env.Stderr, "agent: %v\n\n%s", err, usage)
		return 2
	}
	return withApp(env, func(a *App) error {
		dir, err := a.resolveDir(l.cwd)
		if err != nil {
			return err
		}
		cols, rows, _ := env.TermSize()
		r, err := a.Chats.Start(ctx, chat.StartOptions{Agent: l.agent, Cwd: dir, Title: l.title, Args: l.args, Cols: cols, Rows: rows})
		if err != nil {
			return err
		}
		if l.detach {
			fmt.Fprintln(env.Stdout, r.ID)
			return nil
		}
		return a.attach(ctx, r.ID, r.TmuxSession)
	})
}

func (a *App) resolveDir(dir string) (string, error) {
	if dir == "" {
		return a.Env.Getwd()
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(a.Env.Getenv("HOME"), dir[1:])
	}
	if !filepath.IsAbs(dir) {
		wd, err := a.Env.Getwd()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(wd, dir)
	}
	return filepath.Clean(dir), nil
}

// attach connects the terminal to a session: switch-client inside a nostromo tmux,
// otherwise exec tmux attach. Without a TTY it prints the chat id instead.
func (a *App) attach(ctx context.Context, id, session string) error {
	if !a.Env.IsTTY() {
		fmt.Fprintln(a.Env.Stdout, id)
		return nil
	}
	tmuxEnv := a.Env.Getenv("TMUX")
	if tmux.InsideSocket(tmuxEnv, a.Config.TmuxSocket) {
		return a.Tmux.SwitchClient(ctx, session)
	}
	if tmuxEnv != "" {
		fmt.Fprintln(a.Env.Stderr, "agent: attaching inside another tmux; press the prefix key twice to reach the chat")
	}
	var env []string
	for _, kv := range a.Env.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") {
			env = append(env, kv)
		}
	}
	return a.Env.Exec(a.Tmux.AttachArgs(session), env)
}

// Attach attaches to a running or failed chat.
func (a *App) Attach(ctx context.Context, prefix string) error {
	r, err := a.Chats.Store.Find(prefix)
	if err != nil {
		return err
	}
	live, err := a.Chats.Live(ctx, r.ID)
	if err != nil {
		return err
	}
	if live.Status == state.Stopped {
		return fmt.Errorf("chat %s is stopped; run: agent resume %s", short(r.ID), short(r.ID))
	}
	return a.attach(ctx, r.ID, live.Session)
}

// Resume restarts a chat (or attaches if it is running) and attaches.
func (a *App) Resume(ctx context.Context, prefix string) error {
	r, err := a.Chats.Store.Find(prefix)
	if err != nil {
		return err
	}
	cols, rows, _ := a.Env.TermSize()
	r, err = a.Chats.Resume(ctx, r.ID, cols, rows)
	if err != nil && !errors.Is(err, chat.ErrAlreadyRunning) {
		return err
	}
	live, err := a.Chats.Live(ctx, r.ID)
	if err != nil {
		return err
	}
	session := live.Session
	if session == "" {
		session = r.TmuxSession
	}
	return a.attach(ctx, r.ID, session)
}

// Stop ends a chat's session.
func (a *App) Stop(ctx context.Context, prefix string) error {
	r, err := a.Chats.Store.Find(prefix)
	if err != nil {
		return err
	}
	if err := a.Chats.Stop(ctx, r.ID); err != nil {
		return err
	}
	fmt.Fprintf(a.Env.Stdout, "stopped %s\n", short(r.ID))
	return nil
}

// Remove deletes a chat.
func (a *App) Remove(ctx context.Context, prefix string) error {
	r, err := a.Chats.Store.Find(prefix)
	if err != nil {
		return err
	}
	if err := a.Chats.Delete(ctx, r.ID); err != nil {
		return err
	}
	fmt.Fprintf(a.Env.Stdout, "deleted %s\n", short(r.ID))
	return nil
}

// List prints chats grouped by directory.
func (a *App) List(ctx context.Context) error {
	groups, _, _, bad, err := a.Chats.List(ctx)
	if err != nil {
		return err
	}
	now := a.Env.Now()
	if len(groups) == 0 {
		fmt.Fprintln(a.Env.Stdout, "no chats yet; start one with: agent")
	}
	tw := tabwriter.NewWriter(a.Env.Stdout, 0, 4, 2, ' ', 0)
	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintf(tw, "%s\n", homeRel(g.Cwd, a.Env.Getenv("HOME")))
		for _, c := range g.Chats {
			status := string(c.Status)
			if c.ExitCode != nil {
				status = fmt.Sprintf("failed(%d)", *c.ExitCode)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", short(c.ID), status, c.Agent, DisplayTitle(c.Title, c.CreatedAt), Ago(c.LastStartedAt, now))
		}
	}
	tw.Flush()
	if len(bad) > 0 {
		fmt.Fprintf(a.Env.Stderr, "agent: %d unreadable chat records; run: agent doctor\n", len(bad))
	}
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func homeRel(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// DisplayTitle returns the title, or a date-based placeholder when it is empty.
func DisplayTitle(title string, created time.Time) string {
	if title != "" {
		return title
	}
	return "Chat from " + created.Local().Format("Jan 2, 15:04")
}

// Ago formats a past time relative to now.
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
