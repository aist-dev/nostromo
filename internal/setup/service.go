package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tmux"
)

// Service names.
const (
	SystemdUnit  = "nostromo.service"
	LaunchdLabel = "com.nostromo.server"
)

// ServiceSpec is what the service runs.
type ServiceSpec struct {
	Binary string
	Env    map[string]string
	LogDir string
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}

// SystemdUnitFile renders the systemd user unit. KillMode=process keeps tmux and
// the agents alive when the server restarts.
func SystemdUnitFile(s ServiceSpec) string {
	var b strings.Builder
	b.WriteString("# Written by \"agent setup\". Changes are overwritten on upgrade.\n")
	b.WriteString("[Unit]\nDescription=nostromo agent server\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + systemdQuote(s.Binary) + " serve\n")
	b.WriteString("Restart=on-failure\nRestartSec=2\nKillMode=process\n")
	for _, k := range sortedKeys(s.Env) {
		b.WriteString("Environment=" + systemdQuote(k+"="+s.Env[k]) + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// LaunchdPlist renders the launchd agent. AbandonProcessGroup keeps tmux and the
// agents alive when the server restarts.
func LaunchdPlist(s ServiceSpec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by "agent setup". Changes are overwritten on upgrade. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + LaunchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(s.Binary) + `</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>AbandonProcessGroup</key>
  <true/>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(filepath.Join(s.LogDir, "launchd.log")) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(filepath.Join(s.LogDir, "launchd.log")) + `</string>
  <key>EnvironmentVariables</key>
  <dict>
`)
	for _, k := range sortedKeys(s.Env) {
		b.WriteString("    <key>" + xmlEscape(k) + "</key>\n    <string>" + xmlEscape(s.Env[k]) + "</string>\n")
	}
	b.WriteString("  </dict>\n</dict>\n</plist>\n")
	return b.String()
}

// ServiceManager installs and controls the background service.
type ServiceManager interface {
	Name() string
	// Path is the service definition file.
	Path() string
	Render(s ServiceSpec) string
	// Install writes the definition and (re)loads it; start enables and starts it.
	Install(ctx context.Context, s ServiceSpec, start bool) (changed bool, err error)
	Restart(ctx context.Context) error
	Running(ctx context.Context) bool
	Uninstall(ctx context.Context) error
	// EnableLinger makes the service start without a login (Linux only). It
	// reports whether it turned linger on (false when it was already on).
	EnableLinger(ctx context.Context) (bool, error)
	DisableLinger(ctx context.Context) error
	RestartCommand() string
}

func writeIfChanged(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, config.WriteFileAtomic(path, []byte(content), 0o644)
}

func runCmd(ctx context.Context, r tmux.Runner, name string, args ...string) error {
	_, errb, err := r.Run(ctx, name, args...)
	if err != nil {
		msg := strings.TrimSpace(string(errb))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}

// Systemd manages a systemd user unit.
type Systemd struct {
	Home string
	User string
	Run  tmux.Runner
}

func (m *Systemd) Name() string { return "systemd user service " + SystemdUnit }
func (m *Systemd) Path() string {
	return filepath.Join(m.Home, ".config", "systemd", "user", SystemdUnit)
}
func (m *Systemd) Render(s ServiceSpec) string { return SystemdUnitFile(s) }
func (m *Systemd) RestartCommand() string      { return "systemctl --user restart nostromo" }

func (m *Systemd) Install(ctx context.Context, s ServiceSpec, start bool) (bool, error) {
	changed, err := writeIfChanged(m.Path(), m.Render(s))
	if err != nil {
		return false, err
	}
	if err := runCmd(ctx, m.Run, "systemctl", "--user", "daemon-reload"); err != nil {
		return changed, err
	}
	if start {
		if err := runCmd(ctx, m.Run, "systemctl", "--user", "enable", "--now", "nostromo"); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func (m *Systemd) Restart(ctx context.Context) error {
	return runCmd(ctx, m.Run, "systemctl", "--user", "restart", "nostromo")
}

func (m *Systemd) Running(ctx context.Context) bool {
	return runCmd(ctx, m.Run, "systemctl", "--user", "is-active", "--quiet", "nostromo") == nil
}

func (m *Systemd) Uninstall(ctx context.Context) error {
	runCmd(ctx, m.Run, "systemctl", "--user", "disable", "--now", "nostromo")
	if err := os.Remove(m.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return runCmd(ctx, m.Run, "systemctl", "--user", "daemon-reload")
}

func (m *Systemd) EnableLinger(ctx context.Context) (bool, error) {
	out, _, err := m.Run.Run(ctx, "loginctl", "show-user", m.User, "--property=Linger")
	if err == nil && strings.TrimSpace(string(out)) == "Linger=yes" {
		return false, nil
	}
	if err := runCmd(ctx, m.Run, "loginctl", "enable-linger", m.User); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Systemd) DisableLinger(ctx context.Context) error {
	return runCmd(ctx, m.Run, "loginctl", "disable-linger", m.User)
}

// Launchd manages a launchd user agent.
type Launchd struct {
	Home string
	UID  int
	Run  tmux.Runner
}

func (m *Launchd) Name() string { return "launchd agent " + LaunchdLabel }
func (m *Launchd) Path() string {
	return filepath.Join(m.Home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}
func (m *Launchd) Render(s ServiceSpec) string { return LaunchdPlist(s) }
func (m *Launchd) domain() string              { return "gui/" + strconv.Itoa(m.UID) }
func (m *Launchd) target() string              { return m.domain() + "/" + LaunchdLabel }
func (m *Launchd) RestartCommand() string      { return "launchctl kickstart -k " + m.target() }

func (m *Launchd) Install(ctx context.Context, s ServiceSpec, start bool) (bool, error) {
	changed, err := writeIfChanged(m.Path(), m.Render(s))
	if err != nil {
		return false, err
	}
	loaded := m.Running(ctx)
	switch {
	case loaded && !changed:
		return false, nil
	case loaded:
		// launchd only rereads a plist when the job is loaded again.
		runCmd(ctx, m.Run, "launchctl", "bootout", m.target())
		return true, runCmd(ctx, m.Run, "launchctl", "bootstrap", m.domain(), m.Path())
	case start:
		return changed, runCmd(ctx, m.Run, "launchctl", "bootstrap", m.domain(), m.Path())
	default:
		return changed, nil
	}
}

func (m *Launchd) Restart(ctx context.Context) error {
	return runCmd(ctx, m.Run, "launchctl", "kickstart", "-k", m.target())
}

func (m *Launchd) Running(ctx context.Context) bool {
	return runCmd(ctx, m.Run, "launchctl", "print", m.target()) == nil
}

func (m *Launchd) Uninstall(ctx context.Context) error {
	runCmd(ctx, m.Run, "launchctl", "bootout", m.target())
	if err := os.Remove(m.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Launchd) EnableLinger(context.Context) (bool, error) { return false, nil }
func (m *Launchd) DisableLinger(context.Context) error        { return nil }
