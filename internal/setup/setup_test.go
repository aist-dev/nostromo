package setup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tailscale"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/tmux/faketmux"
)

type fakeService struct {
	installed  bool
	running    bool
	installErr error
	lingerErr  error
	restarts   int
	spec       ServiceSpec
	starts     []bool
}

func (f *fakeService) Name() string                { return "fake service" }
func (f *fakeService) Path() string                { return "/fake/service" }
func (f *fakeService) Render(s ServiceSpec) string { return "unit" }
func (f *fakeService) RestartCommand() string      { return "fake restart" }
func (f *fakeService) Install(_ context.Context, s ServiceSpec, start bool) (bool, error) {
	if f.installErr != nil {
		return false, f.installErr
	}
	changed := !f.installed
	f.installed, f.spec = true, s
	f.starts = append(f.starts, start)
	if start {
		f.running = true
	}
	return changed, nil
}
func (f *fakeService) Restart(context.Context) error { f.restarts++; return nil }
func (f *fakeService) Running(context.Context) bool  { return f.running }
func (f *fakeService) Uninstall(context.Context) error {
	f.installed, f.running = false, false
	return nil
}
func (f *fakeService) EnableLinger(context.Context) error { return f.lingerErr }

type fakeTS struct {
	status    tailscale.Status
	statusErr error
	serve     tailscale.ServeConfig
	serveErr  error
	setErr    error
	served    []int
	offs      []int
}

func (f *fakeTS) Status(context.Context) (tailscale.Status, error) { return f.status, f.statusErr }
func (f *fakeTS) ServeStatus(context.Context) (tailscale.ServeConfig, error) {
	return f.serve, f.serveErr
}
func (f *fakeTS) Serve(_ context.Context, port int, target string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.served = append(f.served, port)
	f.serve.HTTPS[port] = target
	return nil
}
func (f *fakeTS) ServeOff(_ context.Context, port int) error {
	f.offs = append(f.offs, port)
	delete(f.serve.HTTPS, port)
	return nil
}

// shellRunner answers login-shell commands.
type shellRunner struct {
	found map[string]string // program -> path for "command -v"
	typ   string            // output of "type agent"
}

func (r *shellRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := args[len(args)-1]
	switch {
	case strings.HasPrefix(cmd, "command -v "):
		if p, ok := r.found[strings.TrimPrefix(cmd, "command -v ")]; ok {
			return []byte("noise from rc\n" + p + "\n"), nil, nil
		}
		return nil, nil, errors.New("exit status 1")
	case strings.HasPrefix(cmd, "type "):
		if r.typ == "" {
			return nil, nil, errors.New("not found")
		}
		return []byte(r.typ), nil, nil
	}
	return nil, nil, errors.New("unexpected " + cmd)
}

type fixture struct {
	in    *Installer
	out   *bytes.Buffer
	svc   *fakeService
	ts    *fakeTS
	tm    *faketmux.Fake
	sh    *shellRunner
	home  string
	exe   string
	alive bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	exe := filepath.Join(t.TempDir(), "agent-build")
	os.WriteFile(exe, []byte("#!/bin/sh\necho binary\n"), 0o755)
	f := &fixture{
		out: &bytes.Buffer{}, svc: &fakeService{}, tm: faketmux.New(), home: home, exe: exe, alive: true,
		ts: &fakeTS{status: tailscale.Status{BackendState: "Running", DNSName: "box.tail1.ts.net", HostName: "box", Login: "me@example.com", MagicDNSEnabled: true, CertDomains: []string{"box.tail1.ts.net"}},
			serve: tailscale.ServeConfig{HTTPS: map[int]string{}}},
	}
	paths := config.Paths{Home: filepath.Join(home, ".nostromo")}
	f.sh = &shellRunner{found: map[string]string{"claude": "/usr/local/bin/claude"}, typ: "agent is " + filepath.Join(home, ".local", "bin", "agent")}
	f.in = &Installer{
		Paths: paths, Home: home, Shell: "/bin/bash", User: "me", GOOS: "linux",
		PathEnv: "/usr/bin:/bin", Exe: exe, Version: "1.0.0", Out: f.out,
		Service: f.svc, Tailscale: f.ts, Tmux: f.tm, Run: f.sh,
		Health: func(context.Context, int) (string, error) {
			if !f.alive {
				return "", errors.New("connection refused")
			}
			return "1.0.0", nil
		},
		HTTPSCheck:  func(context.Context, string) error { return nil },
		WaitTimeout: 300 * time.Millisecond,
	}
	return f
}

func TestSetupFresh(t *testing.T) {
	f := newFixture(t)
	rc := filepath.Join(f.home, ".bashrc")
	write(t, rc, "alias agent='claude --dangerously-skip-permissions'\n")
	write(t, filepath.Join(f.home, ".claude", "settings.json"), `{"hooks":{"PreToolUse":[]}}`)
	if err := f.in.Setup(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	p := f.in.Paths
	if read(t, p.Binary()) != "#!/bin/sh\necho binary\n" {
		t.Fatal("binary not installed")
	}
	if dest, _ := os.Readlink(filepath.Join(f.home, ".local", "bin", "agent")); dest != p.Binary() {
		t.Fatal("symlink")
	}
	if !strings.Contains(read(t, rc), DisabledPrefix+"alias agent=") || !strings.Contains(read(t, rc), BlockStart) {
		t.Fatalf("rc:\n%s", read(t, rc))
	}
	if ok, _ := ClaudeHooksInstalled(filepath.Join(f.home, ".claude", "settings.json")); !ok {
		t.Fatal("hooks")
	}
	if !f.svc.running || f.svc.spec.Env["SHELL"] != "/bin/bash" || f.svc.spec.Env["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("service: %+v", f.svc)
	}
	if len(f.ts.served) != 1 || f.ts.served[0] != 443 {
		t.Fatalf("serve: %v", f.ts.served)
	}
	cfg, _ := config.Load(p.Config())
	if len(cfg.AllowedUsers) != 1 || cfg.AllowedUsers[0] != "me@example.com" || f.svc.restarts != 1 {
		t.Fatalf("allowed_users %v, restarts %d", cfg.AllowedUsers, f.svc.restarts)
	}
	for _, want := range []string{"https://box.tail1.ts.net", "server 1.0.0 answers", "agent \"claude\": /usr/local/bin/claude", "✓ resolve", "disabled in"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Left to do by hand") {
		t.Fatalf("nothing should be left:\n%s", out)
	}

	// A second run changes nothing and stays quiet about it.
	f.out.Reset()
	if err := f.in.Setup(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(f.ts.served) != 1 || strings.Count(read(t, rc), BlockStart) != 1 || !strings.Contains(f.out.String(), "already proxies") {
		t.Fatalf("second run:\n%s", f.out)
	}
}

func TestSetupManualSteps(t *testing.T) {
	f := newFixture(t)
	f.in.PathEnv = filepath.Join(f.home, ".local", "bin") + ":/usr/bin"
	f.svc.lingerErr = errors.New("access denied")
	f.ts.setErr = errors.New("Access denied: serve config denied")
	f.ts.status.MagicDNSEnabled = false
	f.sh.found = map[string]string{}
	write(t, filepath.Join(f.home, ".claude", "settings.json"), `{broken`)
	f.alive = false
	if err := f.in.Setup(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	for _, want := range []string{
		"sudo loginctl enable-linger me",
		"sudo tailscale set --operator=me",
		"admin/dns",
		"\"hooks\"",
		"check the server log",
		"not found in a login shell",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(f.home, ".bashrc")); err == nil {
		t.Fatal("PATH already contains ~/.local/bin: no rc edit expected")
	}
}

func TestSetupTailscaleVariants(t *testing.T) {
	ctx := context.Background()
	// No tailscale at all.
	f := newFixture(t)
	f.in.Tailscale = nil
	f.in.Setup(ctx, Options{})
	if !strings.Contains(f.out.String(), "install Tailscale") {
		t.Fatal(f.out.String())
	}
	// Not running.
	f = newFixture(t)
	f.ts.statusErr = errors.New("stopped")
	f.in.Setup(ctx, Options{})
	if !strings.Contains(f.out.String(), "tailscale up") {
		t.Fatal(f.out.String())
	}
	// Serve status unreadable.
	f = newFixture(t)
	f.ts.serveErr = errors.New("denied")
	f.in.Setup(ctx, Options{})
	if !strings.Contains(f.out.String(), "cannot read tailscale serve status") {
		t.Fatal(f.out.String())
	}
	// 443 taken by something else: use 8443; funnel on 8443 is reported.
	f = newFixture(t)
	f.ts.serve.HTTPS[443] = "http://127.0.0.1:3000"
	f.ts.serve.Funnel = []int{8443}
	f.in.Setup(ctx, Options{})
	if len(f.ts.served) != 1 || f.ts.served[0] != 8443 || !strings.Contains(f.out.String(), "https://box.tail1.ts.net:8443") || !strings.Contains(f.out.String(), "Funnel is ON") {
		t.Fatalf("8443: %v\n%s", f.ts.served, f.out)
	}
	// Both ports taken.
	f = newFixture(t)
	f.ts.serve.HTTPS[443] = "http://127.0.0.1:3000"
	f.ts.serve.HTTPS[8443] = "http://127.0.0.1:3001"
	f.in.Setup(ctx, Options{})
	if len(f.ts.served) != 0 || !strings.Contains(f.out.String(), "both used") {
		t.Fatal(f.out.String())
	}
	// macOS hint and no-tailscale option.
	f = newFixture(t)
	f.in.GOOS = "darwin"
	f.ts.setErr = errors.New("denied")
	f.in.Setup(ctx, Options{})
	if !strings.Contains(f.out.String(), "tailscale serve --bg --https=443 http://127.0.0.1:8787") {
		t.Fatal(f.out.String())
	}
	f = newFixture(t)
	f.in.Setup(ctx, Options{NoTailscale: true, NoService: true})
	if len(f.ts.served) != 0 || f.svc.installed {
		t.Fatal("skipped steps ran")
	}
}

func TestSetupUpgrade(t *testing.T) {
	f := newFixture(t)
	if err := f.in.Setup(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rcBefore := read(t, filepath.Join(f.home, ".bashrc"))
	f.out.Reset()
	f.ts.served = nil
	migrated := false
	f.in.Migrate = func() ([]string, error) { migrated = true; return []string{"chats v1 -> v2"}, nil }
	f.in.Exe = "/nonexistent"
	if err := f.in.Setup(context.Background(), Options{Upgrade: true}); err != nil {
		t.Fatal(err)
	}
	if !migrated || !strings.Contains(f.out.String(), "migrated: chats v1 -> v2") {
		t.Fatal("migration")
	}
	if f.svc.starts[len(f.svc.starts)-1] {
		t.Fatal("upgrade must not start the service")
	}
	if len(f.ts.served) != 0 || read(t, filepath.Join(f.home, ".bashrc")) != rcBefore || strings.Contains(f.out.String(), "✓ config") {
		t.Fatalf("upgrade touched rc, tailscale or ran doctor:\n%s", f.out)
	}
	f.in.Migrate = func() ([]string, error) { return nil, errors.New("boom") }
	if err := f.in.Setup(context.Background(), Options{Upgrade: true}); err == nil {
		t.Fatal("migration failure must fail")
	}
}

func TestSetupStops(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.tm.Ver = "tmux 3.1"
	if err := f.in.Setup(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "apt install tmux") {
		t.Fatalf("old tmux: %v", err)
	}
	// Another program at ~/.local/bin/agent.
	f = newFixture(t)
	write(t, filepath.Join(f.home, ".local", "bin", "agent"), "other")
	if err := f.in.Setup(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "NOSTROMO_CMD=ag") {
		t.Fatalf("occupied: %v", err)
	}
	f = newFixture(t)
	os.MkdirAll(filepath.Join(f.home, ".local", "bin"), 0o755)
	os.Symlink("/opt/other/agent", filepath.Join(f.home, ".local", "bin", "agent"))
	if err := f.in.Setup(ctx, Options{}); err == nil || !strings.Contains(err.Error(), "/opt/other/agent") {
		t.Fatalf("foreign symlink: %v", err)
	}
	// Another name works.
	f.in.CmdName = "ag"
	f.sh.typ = "ag is " + filepath.Join(f.home, ".local", "bin", "ag")
	if err := f.in.Setup(ctx, Options{}); err != nil {
		t.Fatal(err)
	}
	// Broken config.
	f = newFixture(t)
	write(t, f.in.Paths.Config(), "port = ")
	if err := f.in.Setup(ctx, Options{}); err == nil {
		t.Fatal("bad config")
	}
	// Unreadable executable.
	f = newFixture(t)
	f.in.Exe = filepath.Join(f.home, "missing")
	if err := f.in.Setup(ctx, Options{}); err == nil {
		t.Fatal("missing exe")
	}
	// Home is a file.
	f = newFixture(t)
	write(t, filepath.Join(f.home, "file"), "")
	f.in.Paths.Home = filepath.Join(f.home, "file")
	if err := f.in.Setup(ctx, Options{}); err == nil {
		t.Fatal("bad home")
	}
}

func TestSetupServiceProblems(t *testing.T) {
	f := newFixture(t)
	f.in.Service = nil
	f.in.Setup(context.Background(), Options{})
	if !strings.Contains(f.out.String(), "no supported service manager") {
		t.Fatal(f.out.String())
	}
	f = newFixture(t)
	f.svc.installErr = errors.New("no systemd")
	f.in.Setup(context.Background(), Options{})
	if !strings.Contains(f.out.String(), "fake restart") {
		t.Fatal(f.out.String())
	}
	// Wrong version answering.
	f = newFixture(t)
	f.in.Health = func(context.Context, int) (string, error) { return "0.9.0", nil }
	f.in.Setup(context.Background(), Options{})
	if !strings.Contains(f.out.String(), "version 0.9.0") {
		t.Fatal(f.out.String())
	}
}

func TestDoctorFindings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.in.Setup(ctx, Options{})
	f.out.Reset()
	if !f.in.Doctor(ctx) {
		t.Fatalf("healthy install must pass:\n%s", f.out)
	}

	// Break many things at once.
	chats := f.in.Paths.Chats()
	write(t, filepath.Join(chats, "abcdef00-0000-4000-8000-000000000000.json"), "{")
	f.tm.NewSession(ctx, tmux.SessionSpec{Name: "agent-ghost", ChatID: "ghost"})
	f.svc.running = false
	f.alive = false
	f.ts.serve.Funnel = []int{443}
	f.in.HTTPSCheck = func(context.Context, string) error { return errors.New("timeout") }
	f.sh.typ = "agent is aliased to `claude --dangerously-skip-permissions'"
	RemoveClaudeHooks(filepath.Join(f.home, ".claude", "settings.json"))
	config.SetAllowedUsers(f.in.Paths.Config(), nil)
	f.in.PathEnv = "/usr/bin"
	f.out.Reset()
	if f.in.Doctor(ctx) {
		t.Fatal("must fail")
	}
	out := f.out.String()
	for _, want := range []string{"✗ service", "✗ server", "✗ funnel", "! https", "✗ resolve", "shell alias", "! hooks", "! allowed_users", "! PATH", "unreadable records", "agent-ghost"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorMoreFindings(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	write(t, f.in.Paths.Config(), "port = ")
	f.tm.Ver = "tmux 2.9"
	f.in.Service = nil
	f.in.Tailscale = nil
	f.sh.typ = ""
	f.in.Doctor(ctx)
	out := f.out.String()
	for _, want := range []string{"✗ config", "✗ tmux", "✗ binary", "✗ command", "! service", "! tailscale", "! resolve"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	for typ, want := range map[string]string{
		"agent is a shell function from /x": "shell function",
		"agent is /usr/bin/agent":           "another program",
	} {
		f.sh.typ = typ
		f.out.Reset()
		f.in.Doctor(ctx)
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("%q: %s", typ, f.out)
		}
	}

	checks := []struct {
		want    string
		breakIt func(f *fixture)
	}{
		{"! tailscale: not running", func(f *fixture) { f.ts.statusErr = errors.New("x") }},
		{"does not proxy", func(f *fixture) { f.ts.serve.HTTPS = map[int]string{} }},
		{"HTTPS certificates are", func(f *fixture) { f.ts.status.MagicDNSEnabled = false }},
		{"serve: denied", func(f *fixture) { f.ts.serveErr = errors.New("denied") }},
		{"running version 0.9", func(f *fixture) { f.in.Health = func(context.Context, int) (string, error) { return "0.9", nil } }},
		{"not valid JSON", func(f *fixture) { write(t, filepath.Join(f.home, ".claude", "settings.json"), "{") }},
		{"✓ funnel: off", func(f *fixture) {}},
	}
	for _, c := range checks {
		f := newFixture(t)
		f.in.Setup(ctx, Options{})
		c.breakIt(f)
		f.out.Reset()
		f.in.Doctor(ctx)
		if !strings.Contains(f.out.String(), c.want) {
			t.Errorf("doctor lacks %q:\n%s", c.want, f.out)
		}
	}
	f = newFixture(t)
	f.in.Setup(ctx, Options{})
	f.in.Paths.Home = filepath.Join(f.home, "nofile")
	write(t, filepath.Join(f.home, "nofile", "chats"), "")
	f.out.Reset()
	f.in.Doctor(ctx)
	if !strings.Contains(f.out.String(), "✗ chats") {
		t.Fatal(f.out.String())
	}
	if restartFix(&Installer{}) != "restart agent serve" {
		t.Fatal("restartFix")
	}
}

func TestUninstall(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	rc := filepath.Join(f.home, ".bashrc")
	write(t, rc, "alias agent='claude --dangerously-skip-permissions'\n")
	f.in.Setup(ctx, Options{})
	f.ts.serve.HTTPS[8443] = "http://127.0.0.1:9999"
	st := store.New(f.in.Paths.Chats())
	st.Create(store.Record{ID: "abcdef00-0000-4000-8000-000000000000", Cwd: "/"})
	f.out.Reset()
	if err := f.in.Uninstall(ctx, false); err != nil {
		t.Fatal(err)
	}
	if f.svc.installed || len(f.ts.offs) != 1 || f.ts.offs[0] != 443 {
		t.Fatalf("service %v offs %v", f.svc.installed, f.ts.offs)
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".local", "bin", "agent")); !os.IsNotExist(err) {
		t.Fatal("symlink kept")
	}
	if read(t, rc) != "alias agent='claude --dangerously-skip-permissions'\n" {
		t.Fatalf("rc not restored:\n%q", read(t, rc))
	}
	if ok, _ := ClaudeHooksInstalled(filepath.Join(f.home, ".claude", "settings.json")); ok {
		t.Fatal("hooks kept")
	}
	if _, err := os.Stat(f.in.Paths.Home); err != nil {
		t.Fatal("home must be kept without --purge")
	}
	if err := f.in.Uninstall(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.in.Paths.Home); !os.IsNotExist(err) {
		t.Fatal("purge must remove home")
	}

	// Errors are reported, not fatal.
	f = newFixture(t)
	write(t, f.in.Paths.Config(), "port = ")
	write(t, filepath.Join(f.home, ".claude", "settings.json"), "{")
	f.svc.installErr = nil
	f.in.Service = &failingUninstall{f.svc}
	f.ts.serve.HTTPS[443] = "http://127.0.0.1:8787"
	f.in.Tailscale = &failingOff{f.ts}
	os.MkdirAll(filepath.Join(f.home, ".zshrc"), 0o755)
	if err := f.in.Uninstall(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"service: x", "tailscale serve on port 443", "Claude Code hooks"} {
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, f.out)
		}
	}
}

type failingUninstall struct{ *fakeService }

func (failingUninstall) Uninstall(context.Context) error { return errors.New("x") }

type failingOff struct{ *fakeTS }

func (failingOff) ServeOff(context.Context, int) error { return errors.New("y") }

func TestHTTPHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"status":"ok","version":"2.0.0"}`))
		case "/bad":
			w.WriteHeader(500)
		default:
			w.Write([]byte("not json"))
		}
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if v, err := HTTPHealth(context.Background(), port); err != nil || v != "2.0.0" {
		t.Fatal(v, err)
	}
	if err := HTTPSReachable(context.Background(), srv.URL+"/healthz"); err != nil {
		t.Fatal(err)
	}
	if err := HTTPSReachable(context.Background(), srv.URL+"/bad"); err == nil {
		t.Fatal("500")
	}
	if err := HTTPSReachable(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("refused")
	}
	srv.Close()
	if _, err := HTTPHealth(context.Background(), port); err == nil {
		t.Fatal("closed")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	defer srv2.Close()
	port2, _ := strconv.Atoi(srv2.URL[strings.LastIndex(srv2.URL, ":")+1:])
	if _, err := HTTPHealth(context.Background(), port2); err == nil {
		t.Fatal("bad body")
	}
}

func TestHelpersMisc(t *testing.T) {
	if !inPath("/a:/b/", "/b") || inPath("/a", "/b") {
		t.Fatal("inPath")
	}
	if agentProgram(config.Agent{Start: "'/x y/claude' --flag"}) != "/x y/claude" && agentProgram(config.Agent{Start: ""}) != "" {
		t.Fatal("agentProgram")
	}
	if agentProgram(config.Agent{}) != "" || agentProgram(config.Agent{Start: "claude --x"}) != "claude" {
		t.Fatal("agentProgram")
	}
	t.Setenv("LANG", "")
	if envOr("LANG", "d") != "d" {
		t.Fatal("envOr")
	}
	in := &Installer{Shell: "/bin/sh", Run: &shellRunner{found: map[string]string{"x": "/x"}}}
	if p, err := in.loginShell(context.Background(), "command -v x"); err != nil || !strings.HasSuffix(p, "/x") {
		t.Fatal(p, err)
	}
}

func TestCommands(t *testing.T) {
	f := newFixture(t)
	cmds := Commands(func(cli.Env) (*Installer, error) { return f.in, nil })
	env := cli.Env{Stdout: f.out, Stderr: f.out}
	ctx := context.Background()
	if code := cmds["setup"](ctx, []string{"--bogus"}, env); code != 2 {
		t.Fatal("bad flag")
	}
	if code := cmds["setup"](ctx, []string{"--no-tailscale"}, env); code != 0 {
		t.Fatalf("setup: %s", f.out)
	}
	if code := cmds["doctor"](ctx, nil, env); code != 0 {
		t.Fatalf("doctor with warnings only must pass:\n%s", f.out)
	}
	f.svc.running = false
	if code := cmds["doctor"](ctx, nil, env); code != 1 {
		t.Fatal("doctor with a failure must exit 1")
	}
	if code := cmds["uninstall"](ctx, []string{"--bogus"}, env); code != 2 {
		t.Fatal("bad flag")
	}
	if code := cmds["uninstall"](ctx, nil, env); code != 0 {
		t.Fatal("uninstall")
	}
	broken := Commands(func(cli.Env) (*Installer, error) { return nil, errors.New("nope") })
	for _, name := range []string{"setup", "doctor", "uninstall"} {
		if code := broken[name](ctx, nil, env); code != 1 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	f.in.Paths.Home = filepath.Join(f.home, "file")
	write(t, f.in.Paths.Home, "")
	if code := cmds["setup"](ctx, nil, env); code != 1 {
		t.Fatal("setup failure")
	}
	if code := cmds["uninstall"](ctx, []string{"--purge"}, env); code != 0 {
		t.Fatal("purge of a file path still succeeds")
	}
	if len(Commands(nil)) != 3 {
		t.Fatal("default builder")
	}
}

func TestNewInstaller(t *testing.T) {
	home := t.TempDir()
	vars := map[string]string{"HOME": home, "NOSTROMO_HOME": filepath.Join(home, ".nostromo"), "PATH": "/usr/bin"}
	env := cli.DefaultEnv()
	env.Getenv = func(k string) string { return vars[k] }
	in, err := NewInstaller(env)
	if err != nil || in.Shell != "/bin/sh" || in.Exe == "" || in.Tmux == nil || in.Service == nil {
		t.Fatalf("%+v %v", in, err)
	}
	vars["NOSTROMO_SERVICE"] = "none"
	if in, _ := NewInstaller(env); in.Service != nil {
		t.Fatal("NOSTROMO_SERVICE=none must disable the service")
	}
	vars["NOSTROMO_HOME"] = ""
	vars["HOME"] = ""
	if _, err := NewInstaller(env); err == nil {
		t.Fatal("no home")
	}
}
