// Package config loads ~/.nostromo/config.toml and resolves nostromo paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Agent is a CLI agent adapter: command templates for starting and resuming a chat.
type Agent struct {
	Start     string `toml:"start"`
	Resume    string `toml:"resume"`
	Hooks     string `toml:"hooks"`
	ShellWrap *bool  `toml:"shell_wrap"`
	// DenyArgs are arguments the user may not pass through to the agent because
	// they would break chat tracking or the terminal session. An entry starting
	// with "-" is a flag (also matched as "--flag=value"; a one-letter flag is
	// also matched inside combined short flags like "-xr"); any other entry
	// matches an argument that equals it exactly, e.g. an agent subcommand.
	DenyArgs []string `toml:"deny_args"`
}

// WrapInShell reports whether the agent command runs through the user's login shell.
func (a Agent) WrapInShell() bool {
	return a.ShellWrap == nil || *a.ShellWrap
}

// ArgsPlaceholder marks where pass-through arguments go in a start template.
// A template without it gets them appended at the end.
const ArgsPlaceholder = "{args}"

// ClaudeDenyArgs are the Claude Code arguments a chat may not be started with:
// session selection and one-shot modes (they would detach the record from the
// conversation or end the session at once), and subcommands, which Claude runs
// instead of a chat when they are the first positional argument.
var ClaudeDenyArgs = []string{
	"-r", "--resume", "-c", "--continue", "--session-id", "--fork-session", "--from-pr",
	"-p", "--print", "-h", "--help", "-v", "--version", "--bg",
	"agents", "attach", "auth", "auto-mode", "doctor", "gateway", "import", "install", "logs",
	"mcp", "plugin", "plugins", "purge", "respawn", "rm", "setup-token", "stop", "kill",
	"ultrareview", "update", "upgrade",
}

// CheckArgs returns an error naming the first argument that DenyArgs forbids.
func (a Agent) CheckArgs(args []string) error {
	for _, arg := range args {
		for _, d := range a.DenyArgs {
			if denied(arg, d) {
				return fmt.Errorf("argument %q is not allowed: nostromo manages %s itself", arg, d)
			}
		}
	}
	return nil
}

func denied(arg, d string) bool {
	if arg == d {
		return true
	}
	if !strings.HasPrefix(d, "-") {
		return false
	}
	if strings.HasPrefix(d, "--") {
		return strings.HasPrefix(arg, d+"=")
	}
	// One-letter flag: -x also hides in "-yx" and "-xVALUE".
	if len(d) != 2 || len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	letters := arg[1:]
	for i, c := range letters {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			// "-r5" style: only the leading letters are flags.
			letters = letters[:i]
			break
		}
	}
	return strings.ContainsRune(letters, rune(d[1]))
}

// Config is the user configuration.
type Config struct {
	Port           int              `toml:"port"`
	TmuxSocket     string           `toml:"tmux_socket"`
	DefaultAgent   string           `toml:"default_agent"`
	Repo           string           `toml:"repo"`
	CheckUpdates   bool             `toml:"check_updates"`
	AutoUpdate     bool             `toml:"auto_update"`
	AllowedUsers   []string         `toml:"allowed_users"`
	TailscaleServe bool             `toml:"tailscale_serve"`
	Peers          []string         `toml:"peers"`
	Agents         map[string]Agent `toml:"agents"`
}

// DefaultPort is the port the server listens on unless configured otherwise.
const DefaultPort = 8787

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Port:           DefaultPort,
		TmuxSocket:     "nostromo",
		DefaultAgent:   "claude",
		Repo:           "aist-dev/nostromo",
		CheckUpdates:   true,
		AutoUpdate:     false,
		AllowedUsers:   []string{},
		TailscaleServe: true,
		Peers:          []string{},
		Agents: map[string]Agent{
			"claude": {
				Start:    "claude --dangerously-skip-permissions --session-id {id} {args}",
				Resume:   "claude --dangerously-skip-permissions --resume {resume_id}",
				Hooks:    "claude",
				DenyArgs: ClaudeDenyArgs,
			},
		},
	}
}

// DefaultFile is the content written to config.toml on first setup.
const DefaultFile = `# nostromo configuration. A missing key falls back to its default.

port = 8787
tmux_socket = "nostromo"    # tmux -L <name>
default_agent = "claude"
repo = "aist-dev/nostromo"  # where releases come from
check_updates = true        # ask GitHub for a new version once a day
auto_update = false
tailscale_serve = true      # the server sets up the HTTPS address with tailscale serve as soon as it can
allowed_users = []          # Tailscale logins allowed to use the UI ("*" = anyone in the tailnet); empty = this machine only
peers = []                  # other machines for the UI switcher, e.g. ["mac-mini", "linux-box"]

# start/resume placeholders: {id} {resume_id} {cwd}; {args} (start only) is where
# extra arguments of "agent ... <args>" go, appended at the end when it is missing.
# deny_args (optional) lists arguments that may not be passed; built-in agents
# keep their default list unless the file sets one.
[agents.claude]
start      = "claude --dangerously-skip-permissions --session-id {id} {args}"
resume     = "claude --dangerously-skip-permissions --resume {resume_id}"
hooks      = "claude"
shell_wrap = true
`

// Load reads the config file at path. A missing file yields Default().
// Agents defined in the file are added to (or replace) the built-in ones.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var file Config
	md, err := toml.Decode(string(data), &file)
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return cfg, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	merge(&cfg, file, md)
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func merge(cfg *Config, file Config, md toml.MetaData) {
	if md.IsDefined("port") {
		cfg.Port = file.Port
	}
	if md.IsDefined("tmux_socket") {
		cfg.TmuxSocket = file.TmuxSocket
	}
	if md.IsDefined("default_agent") {
		cfg.DefaultAgent = file.DefaultAgent
	}
	if md.IsDefined("repo") {
		cfg.Repo = file.Repo
	}
	if md.IsDefined("check_updates") {
		cfg.CheckUpdates = file.CheckUpdates
	}
	if md.IsDefined("auto_update") {
		cfg.AutoUpdate = file.AutoUpdate
	}
	if md.IsDefined("allowed_users") {
		cfg.AllowedUsers = file.AllowedUsers
	}
	if md.IsDefined("tailscale_serve") {
		cfg.TailscaleServe = file.TailscaleServe
	}
	if md.IsDefined("peers") {
		cfg.Peers = file.Peers
	}
	for name, a := range file.Agents {
		if def, ok := cfg.Agents[name]; ok && !md.IsDefined("agents", name, "deny_args") {
			a.DenyArgs = def.DenyArgs
		}
		cfg.Agents[name] = a
	}
}

var placeholderRe = regexp.MustCompile(`\{([^{}]*)\}`)

// Placeholders lists the substitutions allowed in agent command templates.
var Placeholders = []string{"id", "resume_id", "cwd", "args"}

// ValidateTemplate checks that a command template only uses known placeholders.
func ValidateTemplate(tmpl string) error {
	if strings.TrimSpace(tmpl) == "" {
		return errors.New("empty command template")
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(tmpl, -1) {
		known := false
		for _, p := range Placeholders {
			if m[1] == p {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown placeholder {%s} in %q", m[1], tmpl)
		}
	}
	return nil
}

// Validate checks the configuration for errors that would only surface later.
func (c Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.TmuxSocket == "" || strings.ContainsAny(c.TmuxSocket, "/ \t") {
		return fmt.Errorf("invalid tmux_socket %q", c.TmuxSocket)
	}
	for name, a := range c.Agents {
		if err := ValidateTemplate(a.Start); err != nil {
			return fmt.Errorf("agents.%s.start: %w", name, err)
		}
		if err := ValidateTemplate(a.Resume); err != nil {
			return fmt.Errorf("agents.%s.resume: %w", name, err)
		}
		if strings.Contains(a.Resume, ArgsPlaceholder) {
			return fmt.Errorf("agents.%s.resume: %s is only allowed in start (a resumed chat does not get the start arguments again)", name, ArgsPlaceholder)
		}
	}
	if _, ok := c.Agents[c.DefaultAgent]; !ok {
		return fmt.Errorf("default_agent %q is not defined under [agents]", c.DefaultAgent)
	}
	return nil
}

// AgentNames returns the configured agent names, sorted.
func (c Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var allowedUsersRe = regexp.MustCompile(`(?m)^allowed_users\s*=.*$`)

// SetAllowedUsers rewrites the allowed_users line of the config file at path,
// keeping the rest of the file (comments included) intact. A missing file is
// created from DefaultFile first.
func SetAllowedUsers(path string, users []string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(DefaultFile)
	} else if err != nil {
		return err
	}
	quoted := make([]string, len(users))
	for i, u := range users {
		quoted[i] = fmt.Sprintf("%q", u)
	}
	line := "allowed_users = [" + strings.Join(quoted, ", ") + "]"
	text := string(data)
	if allowedUsersRe.MatchString(text) {
		text = allowedUsersRe.ReplaceAllLiteralString(text, line)
	} else {
		// Top-level keys must come before the first table header.
		if i := strings.Index(text, "\n["); i >= 0 {
			text = text[:i+1] + line + "\n" + text[i+1:]
		} else {
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += line + "\n"
		}
	}
	return WriteFileAtomic(path, []byte(text), 0o644)
}

// WriteFileAtomic writes data to a temp file in the same directory and renames it over path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
