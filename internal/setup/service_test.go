package setup

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

type cmdRunner struct {
	calls []string
	fail  map[string]bool // keyed by "name arg0 arg1 ..."
	out   map[string]string
}

func newCmdRunner() *cmdRunner {
	return &cmdRunner{fail: map[string]bool{}, out: map[string]string{}}
}

func (r *cmdRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.calls = append(r.calls, key)
	for k := range r.fail {
		if strings.HasPrefix(key, k) {
			return []byte(r.out[key]), []byte("failed: " + k), errors.New("exit status 1")
		}
	}
	return []byte(r.out[key]), nil, nil
}

func (r *cmdRunner) has(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

var spec = ServiceSpec{
	Binary: "/home/me/.nostromo/bin/agent",
	LogDir: "/home/me/.nostromo/logs",
	Env: map[string]string{
		"HOME": "/home/me", "NOSTROMO_HOME": "/home/me/.nostromo", "SHELL": "/bin/zsh",
		"PATH": "/home/me/.local/bin:/usr/bin:/bin", "LANG": "en_US.UTF-8", "ODD": `a "quoted" 100% <value> & 'more'`,
	},
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(p, []byte(got), 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("golden %s: %v (run go test -run %s -update)", p, err, t.Name())
	}
	if string(want) != got {
		t.Fatalf("%s differs from golden file:\n%s", name, got)
	}
}

func TestGoldenServiceFiles(t *testing.T) {
	unit := SystemdUnitFile(spec)
	golden(t, "nostromo.service", unit)
	for _, want := range []string{"KillMode=process", "Restart=on-failure", `ExecStart="/home/me/.nostromo/bin/agent" serve`, `100%% <value>`} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	plist := LaunchdPlist(spec)
	golden(t, "com.nostromo.server.plist", plist)
	for _, want := range []string{"<key>AbandonProcessGroup</key>\n  <true/>", "<key>RunAtLoad</key>", "&lt;value&gt; &amp; &apos;more&apos;"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
}

func TestSystemd(t *testing.T) {
	ctx := context.Background()
	r := newCmdRunner()
	m := &Systemd{Home: t.TempDir(), User: "me", Run: r}
	if !strings.HasSuffix(m.Path(), ".config/systemd/user/nostromo.service") || m.Name() == "" || m.RestartCommand() != "systemctl --user restart nostromo" {
		t.Fatal(m.Path())
	}
	changed, err := m.Install(ctx, spec, true)
	if err != nil || !changed || !r.has("systemctl --user daemon-reload") || !r.has("systemctl --user enable --now nostromo") {
		t.Fatalf("%v %v %v", changed, err, r.calls)
	}
	if changed, _ := m.Install(ctx, spec, false); changed {
		t.Fatal("unchanged unit")
	}
	if err := m.Restart(ctx); err != nil || !m.Running(ctx) {
		t.Fatal("restart/running")
	}
	r.out["loginctl show-user me --property=Linger"] = "Linger=yes\n"
	r.calls = nil
	if enabled, err := m.EnableLinger(ctx); err != nil || enabled || r.has("loginctl enable-linger") {
		t.Fatal("linger already on")
	}
	r.out["loginctl show-user me --property=Linger"] = "Linger=no\n"
	if enabled, err := m.EnableLinger(ctx); err != nil || !enabled || !r.has("loginctl enable-linger me") {
		t.Fatal("linger")
	}
	if err := m.DisableLinger(ctx); err != nil || !r.has("loginctl disable-linger me") {
		t.Fatal("disable linger")
	}
	r.fail["loginctl enable-linger"] = true
	if _, err := m.EnableLinger(ctx); err == nil {
		t.Fatal("enable-linger failure")
	}
	delete(r.fail, "loginctl enable-linger")
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Path()); !os.IsNotExist(err) {
		t.Fatal("unit not removed")
	}

	r.fail["systemctl --user daemon-reload"] = true
	if _, err := m.Install(ctx, spec, true); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("daemon-reload failure: %v", err)
	}
	delete(r.fail, "systemctl --user daemon-reload")
	r.fail["systemctl --user enable"] = true
	if _, err := m.Install(ctx, spec, true); err == nil {
		t.Fatal("enable failure")
	}
	r.fail["systemctl --user is-active"] = true
	if m.Running(ctx) {
		t.Fatal("not running")
	}
	bad := &Systemd{Home: filepath.Join(m.Home, ".config", "systemd", "user", "nostromo.service"), Run: r}
	if _, err := bad.Install(ctx, spec, true); err == nil {
		t.Fatal("unwritable path")
	}
	os.Remove(m.Path())
	os.MkdirAll(filepath.Join(m.Path(), "x"), 0o755)
	if err := m.Uninstall(ctx); err == nil {
		t.Fatal("cannot remove a non-empty directory")
	}
}

func TestLaunchd(t *testing.T) {
	ctx := context.Background()
	r := newCmdRunner()
	m := &Launchd{Home: t.TempDir(), UID: 501, Run: r}
	if !strings.HasSuffix(m.Path(), "Library/LaunchAgents/com.nostromo.server.plist") || m.RestartCommand() != "launchctl kickstart -k gui/501/com.nostromo.server" {
		t.Fatal(m.Path(), m.RestartCommand())
	}
	// Not loaded yet: bootstrap.
	r.fail["launchctl print"] = true
	if changed, err := m.Install(ctx, spec, true); err != nil || !changed || !r.has("launchctl bootstrap gui/501 "+m.Path()) {
		t.Fatalf("%v %v %v", changed, err, r.calls)
	}
	// Upgrade while not loaded: only write.
	r.calls = nil
	spec2 := spec
	spec2.Binary = "/other/agent"
	if changed, err := m.Install(ctx, spec2, false); err != nil || !changed || r.has("launchctl bootstrap") {
		t.Fatalf("upgrade not loaded: %v %v", changed, r.calls)
	}
	// Loaded and unchanged: nothing.
	delete(r.fail, "launchctl print")
	r.calls = nil
	if changed, _ := m.Install(ctx, spec2, true); changed || r.has("launchctl bootout") {
		t.Fatal("unchanged")
	}
	// Loaded and changed: reload.
	if changed, err := m.Install(ctx, spec, false); err != nil || !changed || !r.has("launchctl bootout gui/501/com.nostromo.server") || !r.has("launchctl bootstrap") {
		t.Fatalf("reload: %v %v", err, r.calls)
	}
	if err := m.Restart(ctx); err != nil || !r.has("launchctl kickstart -k") {
		t.Fatal("restart")
	}
	if enabled, err := m.EnableLinger(ctx); err != nil || enabled || m.DisableLinger(ctx) != nil {
		t.Fatal("linger is a no-op")
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal("second uninstall")
	}
	blocker := filepath.Join(m.Home, "Library")
	os.RemoveAll(blocker)
	os.WriteFile(blocker, nil, 0o644)
	if _, err := m.Install(ctx, spec, true); err == nil {
		t.Fatal("unwritable")
	}
	os.Remove(blocker)
	os.MkdirAll(filepath.Join(m.Path(), "x"), 0o755)
	if err := m.Uninstall(ctx); err == nil {
		t.Fatal("cannot remove a non-empty directory")
	}
}

func TestRunCmdErrorText(t *testing.T) {
	r := newCmdRunner()
	if err := runCmd(context.Background(), r, "x", "y"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(context.Background(), errRunner{}, "x"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatal(err)
	}
}

type errRunner struct{}

func (errRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("boom")
}
