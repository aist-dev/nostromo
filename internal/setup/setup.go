package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tailscale"
	"github.com/aist-dev/nostromo/internal/tmux"
)

// Installer performs setup, doctor and uninstall against one machine.
// Every external effect goes through a field, so tests can replace it.
type Installer struct {
	Paths   config.Paths
	Home    string // user home
	Shell   string // login shell
	User    string
	GOOS    string
	CmdName string // name of the symlink on PATH, "agent" by default
	PathEnv string // $PATH of the setup process
	Exe     string // the running binary
	Version string
	Out     io.Writer

	Service   ServiceManager // nil: no supported service manager
	Tailscale tailscale.API  // nil: tailscale CLI not found
	Tmux      tmux.API
	Run       tmux.Runner
	// Health returns the version reported by GET /healthz on the local port.
	Health func(ctx context.Context, port int) (string, error)
	// HTTPSCheck fetches a URL through Tailscale (doctor only).
	HTTPSCheck func(ctx context.Context, url string) error
	// Migrate upgrades stored data; nil means nothing to migrate.
	Migrate func() ([]string, error)
	// WaitTimeout bounds the wait for the service after (re)start.
	WaitTimeout time.Duration
	// TTY reads answers from the user's terminal; nil when setup runs without one
	// (then nothing is asked and manual steps are listed instead).
	TTY *bufio.Reader
	// Sudo runs a command with sudo on the user's terminal.
	Sudo func(ctx context.Context, args ...string) error
}

// Options select setup steps.
type Options struct {
	Upgrade     bool // called by "agent update": no rc files, no Tailscale, no service start
	NoService   bool
	NoTailscale bool
}

type report struct {
	out  io.Writer
	todo []string
}

func (r *report) ok(format string, a ...any)   { fmt.Fprintf(r.out, "  ✓ "+format+"\n", a...) }
func (r *report) warn(format string, a ...any) { fmt.Fprintf(r.out, "  ! "+format+"\n", a...) }
func (r *report) step(name string)             { fmt.Fprintf(r.out, "%s\n", name) }
func (r *report) need(s string)                { r.todo = append(r.todo, s) }

func (in *Installer) linkPath() string {
	name := in.CmdName
	if name == "" {
		name = "agent"
	}
	return filepath.Join(in.Home, ".local", "bin", name)
}

// lingerMarker exists when setup enabled linger for this user.
func (in *Installer) lingerMarker() string {
	return filepath.Join(in.Paths.Home, "linger-enabled-by-setup")
}

// InstallCmd is the one way users install, repair or reinstall nostromo. Users
// are never sent to "agent setup": "agent" is the command that starts Claude.
const InstallCmd = "curl -fsSL https://raw.githubusercontent.com/aist-dev/nostromo/main/install.sh | bash"

// reinstallHint tells the user how to repair the installation.
func reinstallHint() string { return "run the installer again: " + InstallCmd }

func (in *Installer) claudeSettings() string {
	return filepath.Join(in.Home, ".claude", "settings.json")
}

func (in *Installer) target(cfg config.Config) string {
	return "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
}

// Setup installs or upgrades nostromo. Optional steps that fail are reported and
// listed at the end; only missing prerequisites stop it.
func (in *Installer) Setup(ctx context.Context, o Options) error {
	r := &report{out: in.Out}
	r.step("Files")
	newBinary, err := in.files(ctx, o, r)
	if err != nil {
		return err
	}
	cfg, err := config.Load(in.Paths.Config())
	if err != nil {
		return err
	}
	if o.Upgrade && in.Migrate != nil {
		done, err := in.Migrate()
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		for _, m := range done {
			r.ok("migrated: %s", m)
		}
	}
	if !o.Upgrade {
		r.step("Command")
		if err := in.command(r); err != nil {
			return err
		}
	}
	r.step("Checks")
	if v, err := tmux.CheckVersion(ctx, in.Tmux); err != nil {
		return fmt.Errorf("%w\n  install it with: brew install tmux (macOS) or sudo apt install tmux (Debian/Ubuntu)", err)
	} else {
		r.ok("tmux %d.%d", v.Major, v.Minor)
	}
	in.checkAgents(ctx, cfg, r, false)

	r.step("Hooks")
	in.hooks(cfg, r)

	serviceUp := false
	if !o.NoService {
		r.step("Service")
		serviceUp = in.service(ctx, o, newBinary, r)
	}
	if serviceUp && !o.Upgrade {
		in.waitHealthy(ctx, cfg, r)
	}
	if !o.Upgrade && !o.NoTailscale {
		r.step("HTTPS")
		in.tailscaleServe(ctx, cfg, r)
	}

	fmt.Fprintln(in.Out)
	if !o.Upgrade {
		fmt.Fprintf(in.Out, "Local address: %s\n", in.target(cfg))
	}
	if len(r.todo) > 0 {
		fmt.Fprintln(in.Out, "\nLeft to do by hand:")
		for _, t := range r.todo {
			fmt.Fprintf(in.Out, "  - %s\n", strings.ReplaceAll(t, "\n", "\n    "))
		}
	}
	if !o.Upgrade {
		fmt.Fprintln(in.Out)
		in.Doctor(ctx)
	}
	return nil
}

// files creates the home directory and installs the running binary. It reports
// whether the installed binary is new since the service last started, which is
// always the case when setup runs from it (install.sh puts it in place first).
func (in *Installer) files(ctx context.Context, o Options, r *report) (bool, error) {
	if err := os.MkdirAll(in.Paths.Home, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(in.Paths.Home, 0o700); err != nil {
		return false, err
	}
	for _, d := range []string{in.Paths.Bin(), in.Paths.Chats(), in.Paths.Logs()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return false, err
		}
	}
	if _, err := os.Stat(in.Paths.Config()); errors.Is(err, os.ErrNotExist) {
		if err := config.WriteFileAtomic(in.Paths.Config(), []byte(config.DefaultFile), 0o644); err != nil {
			return false, err
		}
		r.ok("wrote %s", in.Paths.Config())
	}
	if changed, err := writeIfChanged(in.Paths.TmuxConf(), tmux.Conf); err != nil {
		return false, err
	} else if changed && in.Tmux != nil {
		// Running chats keep their tmux server: bring it up to the new config.
		if err := in.Tmux.SourceFile(ctx, in.Paths.TmuxConf()); err != nil {
			r.warn("reload tmux config: %v", err)
		}
	}
	if !o.Upgrade && in.Exe != "" && in.Exe != in.Paths.Binary() {
		data, err := os.ReadFile(in.Exe)
		if err != nil {
			return false, err
		}
		if err := config.WriteFileAtomic(in.Paths.Binary(), data, 0o755); err != nil {
			return false, err
		}
		r.ok("installed %s", in.Paths.Binary())
	}
	r.ok("%s", in.Paths.Home)
	return !o.Upgrade, nil
}

func (in *Installer) command(r *report) error {
	for _, rc := range RCFiles(in.Home) {
		disabled, err := DisableAgentAlias(rc)
		if err != nil {
			r.warn("%s: %v", rc, err)
			continue
		}
		for _, l := range disabled {
			r.ok("disabled in %s: %s (backup: %s.nostromo.bak)", rc, l, rc)
		}
	}
	link := in.linkPath()
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(in.Paths.Binary(), link); err != nil {
			return err
		}
		r.ok("linked %s -> %s", link, in.Paths.Binary())
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		dest, _ := os.Readlink(link)
		if dest != in.Paths.Binary() {
			return fmt.Errorf("%s points to %s, another program.\n  Remove it, or install under another name by running the installer with NOSTROMO_CMD=ag", link, dest)
		}
		r.ok("%s -> %s", link, dest)
	default:
		return fmt.Errorf("%s is another program.\n  Remove it, or install under another name by running the installer with NOSTROMO_CMD=ag", link)
	}
	binDir := filepath.Dir(link)
	if !inPath(in.PathEnv, binDir) {
		rc := RCFileForShell(in.Home, in.Shell)
		if _, err := EnsureBlock(rc, PathLine(rc, binDir)); err != nil {
			r.warn("cannot update %s: %v", rc, err)
			r.need("add " + binDir + " to PATH")
		} else {
			r.ok("added %s to PATH in %s (open a new terminal)", binDir, rc)
		}
	}
	return nil
}

func inPath(pathEnv, dir string) bool {
	for _, p := range filepath.SplitList(pathEnv) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// loginShellCheck runs a command in the user's login shell and returns its stdout.
func (in *Installer) loginShell(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	flags := "-lic"
	if b := filepath.Base(in.Shell); b == "sh" || b == "dash" {
		flags = "-ic"
	}
	out, _, err := in.Run.Run(ctx, in.Shell, flags, command)
	return strings.TrimSpace(string(out)), err
}

func agentProgram(a config.Agent) string {
	f := strings.Fields(a.Start)
	if len(f) == 0 {
		return ""
	}
	return strings.Trim(f[0], `'"`)
}

func (in *Installer) checkAgents(ctx context.Context, cfg config.Config, r *report, quiet bool) bool {
	all := true
	for _, name := range cfg.AgentNames() {
		prog := agentProgram(cfg.Agents[name])
		path, err := in.loginShell(ctx, "command -v "+prog)
		if err != nil || path == "" {
			all = false
			if !quiet {
				r.warn("agent %q: %s not found in a login shell", name, prog)
			}
			continue
		}
		if !quiet {
			r.ok("agent %q: %s", name, lastLine(path))
		}
	}
	return all
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func (in *Installer) hooks(cfg config.Config, r *report) {
	installed := false
	for _, name := range cfg.AgentNames() {
		if cfg.Agents[name].Hooks != "claude" || installed {
			continue
		}
		installed = true
		changed, err := InstallClaudeHooks(in.claudeSettings(), in.Paths.Binary())
		switch {
		case errors.Is(err, ErrInvalidSettings):
			r.warn("%v", err)
			r.need("add the Claude Code hooks to " + in.claudeSettings() + " by hand:\n" + ClaudeHooksSnippet(in.Paths.Binary()))
		case err != nil:
			r.warn("Claude Code hooks: %v", err)
		case changed:
			r.ok("Claude Code hooks added to %s", in.claudeSettings())
		default:
			r.ok("Claude Code hooks already present")
		}
	}
}

func (in *Installer) serviceSpec() ServiceSpec {
	return ServiceSpec{
		Binary: in.Paths.Binary(),
		LogDir: in.Paths.Logs(),
		Env: map[string]string{
			"HOME": in.Home, "NOSTROMO_HOME": in.Paths.Home, "SHELL": in.Shell, "PATH": in.PathEnv,
			"LANG": envOr("LANG", "en_US.UTF-8"),
		},
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (in *Installer) service(ctx context.Context, o Options, newBinary bool, r *report) bool {
	wasRunning := in.Service != nil && in.Service.Running(ctx)
	if in.Service == nil {
		r.warn("no supported service manager on %s", in.GOOS)
		r.need("run \"agent serve\" in the background yourself")
		return false
	}
	changed, err := in.Service.Install(ctx, in.serviceSpec(), !o.Upgrade)
	if err != nil {
		r.warn("%s: %v", in.Service.Name(), err)
		r.need("start the service: " + in.Service.RestartCommand())
		return false
	}
	if changed {
		r.ok("%s written to %s", in.Service.Name(), in.Service.Path())
	} else {
		r.ok("%s up to date", in.Service.Name())
	}
	// A running service keeps running the old binary and the old unit until it
	// restarts: "systemctl enable --now" does not restart a running unit. (On
	// upgrade, agent update restarts it after setup.)
	if wasRunning && !o.Upgrade && (newBinary || changed) {
		if err := in.Service.Restart(ctx); err != nil {
			r.warn("restart %s: %v", in.Service.Name(), err)
			r.need("restart the service: " + in.Service.RestartCommand())
		} else {
			r.ok("restarted %s with the new binary", in.Service.Name())
		}
	}
	if !o.Upgrade && in.GOOS == "linux" {
		enabled, err := in.Service.EnableLinger(ctx)
		switch {
		case err != nil && in.ask("Starting the service at boot without a login needs \"sudo loginctl enable-linger "+in.User+"\". Run it now?") && in.Sudo(ctx, "loginctl", "enable-linger", in.User) == nil:
			os.WriteFile(in.lingerMarker(), nil, 0o600)
			r.ok("service starts at boot without a login (linger enabled)")
		case err != nil:
			r.warn("cannot enable start at boot without login: %v", err)
			r.need("let the service start at boot without a login: sudo loginctl enable-linger " + in.User)
		case enabled:
			// Remember that setup turned it on, so uninstall turns it off again.
			os.WriteFile(in.lingerMarker(), nil, 0o600)
			r.ok("service starts at boot without a login (linger enabled)")
		default:
			r.ok("service starts at boot without a login (linger was already on)")
		}
	}
	return true
}

func (in *Installer) waitHealthy(ctx context.Context, cfg config.Config, r *report) {
	timeout := in.WaitTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		v, err := in.Health(ctx, cfg.Port)
		if err == nil && v == in.Version {
			r.ok("server %s answers on %s", v, in.target(cfg))
			return
		}
		if err == nil {
			last = "version " + v
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			r.warn("server did not report version %s within %v (%s)", in.Version, timeout, last)
			r.need("check the server log: " + in.Paths.ServerLog())
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const selfHeal = "the server finishes the HTTPS setup by itself within a minute after that"

// operatorHint is the one-time command that lets a Linux user manage tailscale serve.
func (in *Installer) operatorHint() string {
	if in.GOOS == "linux" {
		return "allow your user to configure Tailscale: sudo tailscale set --operator=" + in.User + "\n " + selfHeal
	}
	return "check that the Tailscale app is running and logged in; " + selfHeal
}

// ask prints a yes/no question on the terminal; Enter means yes.
func (in *Installer) ask(question string) bool {
	if in.TTY == nil {
		return false
	}
	fmt.Fprintf(in.Out, "  ? %s [Y/n] ", question)
	line, _ := in.TTY.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// waitForHTTPS lets the user enable HTTPS certificates in the admin console
// while setup waits, instead of rerunning setup afterwards.
func (in *Installer) waitForHTTPS(ctx context.Context) bool {
	if in.TTY == nil {
		return false
	}
	fmt.Fprintln(in.Out, "  HTTPS certificates are off for your tailnet. In the Tailscale admin console,")
	fmt.Fprintln(in.Out, "  https://login.tailscale.com/admin/dns, turn on MagicDNS and HTTPS Certificates.")
	for i := 0; i < 20; i++ {
		fmt.Fprint(in.Out, "  ? Press Enter when done, or type s and Enter to skip: ")
		line, err := in.TTY.ReadString('\n')
		if err != nil || strings.EqualFold(strings.TrimSpace(line), "s") {
			return false
		}
		if st, err := in.Tailscale.Status(ctx); err == nil && st.HTTPSReady() {
			return true
		}
		fmt.Fprintln(in.Out, "  Still off (it can take a few seconds to apply).")
	}
	return false
}

func (in *Installer) tailscaleServe(ctx context.Context, cfg config.Config, r *report) {
	if in.Tailscale == nil {
		r.warn("tailscale CLI not found: the UI is only reachable on this machine")
		r.need("install Tailscale and log in (https://tailscale.com/download); " + selfHeal)
		return
	}
	st, err := in.Tailscale.Status(ctx)
	if err != nil || !st.Running() {
		r.warn("Tailscale is not running")
		r.need("start Tailscale and log in (tailscale up); " + selfHeal)
		return
	}
	if !st.HTTPSReady() {
		if !in.waitForHTTPS(ctx) {
			// tailscale serve would block waiting for HTTPS to be enabled.
			r.warn("MagicDNS or HTTPS certificates are off in this tailnet")
			r.need("enable MagicDNS and HTTPS certificates once for the tailnet: https://login.tailscale.com/admin/dns\n " + selfHeal)
			return
		}
		st, _ = in.Tailscale.Status(ctx)
		r.ok("HTTPS certificates are on")
	}
	target := in.target(cfg)
	port, changed, err := tailscale.Ensure(ctx, in.Tailscale, target)
	if err != nil && !errors.Is(err, tailscale.ErrPortsTaken) && in.GOOS == "linux" &&
		in.ask("Tailscale does not let your user set up HTTPS yet. Run \"sudo tailscale set --operator="+in.User+"\" now?") {
		if serr := in.Sudo(ctx, "tailscale", "set", "--operator="+in.User); serr != nil {
			r.warn("sudo tailscale set: %v", serr)
		} else {
			port, changed, err = tailscale.Ensure(ctx, in.Tailscale, target)
		}
	}
	switch {
	case errors.Is(err, tailscale.ErrPortsTaken):
		r.warn("%v", err)
		r.need("free https port 443 or 8443 in tailscale serve; " + selfHeal)
		return
	case err != nil:
		r.warn("tailscale serve: %v", err)
		r.need(in.operatorHint())
		return
	case changed:
		r.ok("tailscale serve: https port %d -> %s", port, target)
	default:
		r.ok("tailscale serve already proxies https port %d to %s", port, target)
	}
	if sc, err := in.Tailscale.ServeStatus(ctx); err == nil && sc.FunnelOn(port) {
		r.warn("Tailscale Funnel is ON for port %d: the UI is on the public internet", port)
		r.need(fmt.Sprintf("turn Funnel off: tailscale funnel --https=%d off", port))
	}
	if len(cfg.AllowedUsers) == 0 && st.Login != "" {
		if err := config.SetAllowedUsers(in.Paths.Config(), []string{st.Login}); err != nil {
			r.warn("cannot set allowed_users: %v", err)
		} else {
			r.ok("allowed_users = [%q]", st.Login)
			if in.Service != nil {
				if err := in.Service.Restart(ctx); err != nil {
					r.warn("restart the service to apply allowed_users: %v", err)
				}
			}
		}
	}
	url := "https://" + st.DNSName
	if port != 443 {
		url += ":" + strconv.Itoa(port)
	}
	fmt.Fprintf(in.Out, "\nOpen on any of your devices: %s\n", url)
}

// HTTPHealth asks the local server for its version.
func HTTPHealth(ctx context.Context, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Version, nil
}

// HTTPSReachable checks that a URL answers with a 2xx status.
func HTTPSReachable(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", res.StatusCode)
	}
	return nil
}
