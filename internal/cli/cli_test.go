package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/tmux/faketmux"
	"github.com/aist-dev/nostromo/internal/version"
)

type harness struct {
	env      Env
	vars     map[string]string
	out, err *bytes.Buffer
	fake     *faketmux.Fake
	execs    [][]string
	execEnv  []string
	execErr  error
	tty      bool
	cwd      string
	home     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}, fake: faketmux.New(), tty: true, cwd: t.TempDir(), home: t.TempDir()}
	h.vars = map[string]string{"HOME": h.home, "NOSTROMO_HOME": filepath.Join(h.home, ".nostromo"), "SHELL": "/bin/bash"}
	h.env = Env{
		Stdin: strings.NewReader(""), Stdout: h.out, Stderr: h.err,
		Getenv:   func(k string) string { return h.vars[k] },
		Environ:  func() []string { return []string{"PATH=/bin", "TMUX=/tmp/tmux-1/default,1,0"} },
		Getwd:    func() (string, error) { return h.cwd, nil },
		IsTTY:    func() bool { return h.tty },
		TermSize: func() (int, int, bool) { return 100, 30, true },
		Exec: func(argv, env []string) error {
			h.execs = append(h.execs, argv)
			h.execEnv = env
			return h.execErr
		},
		NewTmux:  func(config.Config, config.Paths) tmux.API { return h.fake },
		Now:      func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
		Commands: map[string]Command{},
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.err.Reset()
	return Main(args, h.env)
}

func (h *harness) onlyChat(t *testing.T) store.Record {
	t.Helper()
	recs, _, _ := store.New(filepath.Join(h.vars["NOSTROMO_HOME"], "chats")).List()
	if len(recs) != 1 {
		t.Fatalf("expected one chat, got %d", len(recs))
	}
	return recs[0]
}

func TestVersionAndHelp(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if code := h.run(args...); code != 0 || strings.TrimSpace(h.out.String()) != version.Version {
			t.Fatalf("%v: %d %q", args, code, h.out)
		}
	}
	if code := h.run("help"); code != 0 || !strings.Contains(h.out.String(), "Usage:") {
		t.Fatal("help")
	}
	if code := h.run("-h"); code != 0 {
		t.Fatalf("-h: %d", code)
	}
	if code := h.run("--help"); code != 0 || !strings.Contains(h.out.String(), "--nostr-detach") {
		t.Fatalf("--help: %d", code)
	}
	for _, args := range [][]string{
		{"--nostr-bogus"}, {"--nostr-cwd"}, {"--nostr-detach=yes"}, {"--cwd", "x"}, {"--title=x"}, {"--detach"},
	} {
		if code := h.run(args...); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
	if !strings.Contains(h.err.String(), "--detach is now --nostr-detach") {
		t.Fatalf("renamed hint: %s", h.err)
	}
	if code := h.run("attach"); code != 2 {
		t.Fatalf("missing id: %d", code)
	}
}

func TestNewChatAttachesWithExec(t *testing.T) {
	h := newHarness(t)
	h.vars["TMUX"] = "/tmp/tmux-1/default,1,0"
	if code := h.run("--nostr-title", "hello"); code != 0 {
		t.Fatalf("code %d: %s", code, h.err)
	}
	r := h.onlyChat(t)
	if r.Cwd != h.cwd || r.Title != "hello" || r.Agent != "claude" {
		t.Fatalf("record %+v", r)
	}
	s, _ := h.fake.Get(r.TmuxSession)
	if s.Spec.Cols != 100 || s.Spec.Rows != 30 || !strings.HasPrefix(s.Spec.Command, "exec /bin/bash -lic") {
		t.Fatalf("spec %+v", s.Spec)
	}
	if len(h.execs) != 1 || h.execs[0][len(h.execs[0])-1] != "="+r.TmuxSession {
		t.Fatalf("exec %v", h.execs)
	}
	for _, kv := range h.execEnv {
		if strings.HasPrefix(kv, "TMUX=") {
			t.Fatal("TMUX must be removed for a nested attach")
		}
	}
	if !strings.Contains(h.err.String(), "inside another tmux") {
		t.Fatalf("nested hint missing: %s", h.err)
	}
}

func TestNewChatDetachAndNoTTY(t *testing.T) {
	h := newHarness(t)
	sub := filepath.Join(h.home, "proj")
	os.Mkdir(sub, 0o755)
	if code := h.run("--nostr-detach", "--nostr-cwd=~/proj"); code != 0 {
		t.Fatalf("%d %s", code, h.err)
	}
	r := h.onlyChat(t)
	if strings.TrimSpace(h.out.String()) != r.ID || r.Cwd != sub || len(h.execs) != 0 {
		t.Fatalf("detach: %q %+v", h.out, r)
	}

	h2 := newHarness(t)
	h2.tty = false
	os.Mkdir(filepath.Join(h2.cwd, "rel"), 0o755)
	if code := h2.run("--nostr-cwd", "rel"); code != 0 {
		t.Fatalf("%d %s", code, h2.err)
	}
	r2 := h2.onlyChat(t)
	if r2.Cwd != filepath.Join(h2.cwd, "rel") || strings.TrimSpace(h2.out.String()) != r2.ID || len(h2.execs) != 0 {
		t.Fatalf("no tty: %q %+v", h2.out, r2)
	}
}

func TestNewChatInsideNostromoTmuxSwitches(t *testing.T) {
	h := newHarness(t)
	h.vars["TMUX"] = "/tmp/tmux-1000/nostromo,42,0"
	if code := h.run(); code != 0 {
		t.Fatalf("%d %s", code, h.err)
	}
	if len(h.execs) != 0 || len(h.fake.Switched) != 1 {
		t.Fatalf("expected switch-client, got exec=%v switched=%v", h.execs, h.fake.Switched)
	}
}

func TestNewChatPassesArgsThrough(t *testing.T) {
	h := newHarness(t)
	h.vars["SHELL"] = "/bin/sh"
	prompt := "it's $HOME `x` \\ \n --help"
	code := h.run("--model", "opus", "--nostr-detach", prompt, "--nostr-title", "probe")
	if code != 0 {
		t.Fatalf("%d %s", code, h.err)
	}
	r := h.onlyChat(t)
	if strings.TrimSpace(h.out.String()) != r.ID || r.Title != "probe" {
		t.Fatalf("detach must print only the id: %q %+v", h.out, r)
	}
	s, _ := h.fake.Get(r.TmuxSession)
	if strings.Contains(s.Spec.Command, "opus") || strings.Contains(s.Spec.Command, "--nostr-") {
		t.Fatalf("arguments must not be inlined in the command: %s", s.Spec.Command)
	}
	dir := filepath.Join(h.vars["NOSTROMO_HOME"], "run", r.ID)
	files, _ := os.ReadDir(dir)
	var got []string
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		got = append(got, string(b))
	}
	if strings.Join(got, "|") != "--model|opus|"+prompt {
		t.Fatalf("stored arguments: %q", got)
	}
}

func TestNewChatDeniedArgs(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"--resume", "x"}, {"-c"}, {"--session-id=x"}, {"update"}, {"-p", "hi"}} {
		if code := h.run(append([]string{"--nostr-detach"}, args...)...); code != 1 || !strings.Contains(h.err.String(), "is not allowed") {
			t.Fatalf("%v: %d %s", args, code, h.err)
		}
	}
	if recs, _, _ := store.New(filepath.Join(h.vars["NOSTROMO_HOME"], "chats")).List(); len(recs) != 0 {
		t.Fatalf("a refused chat must leave no record: %d", len(recs))
	}
	if code := h.run("--nostr-detach", "fix the update script"); code != 0 {
		t.Fatalf("a prompt that only mentions a subcommand is fine: %s", h.err)
	}
}

func TestNewChatErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--nostr-cwd", "/definitely/missing"); code != 1 || !strings.Contains(h.err.String(), "does not exist") {
		t.Fatalf("%d %s", code, h.err)
	}
	if code := h.run("--nostr-agent", "ghost"); code != 1 || !strings.Contains(h.err.String(), "unknown agent") {
		t.Fatalf("%d %s", code, h.err)
	}
	h.execErr = errors.New("exec failed")
	if code := h.run(); code != 1 || !strings.Contains(h.err.String(), "exec failed") {
		t.Fatalf("%d %s", code, h.err)
	}
	h.env.Getwd = func() (string, error) { return "", errors.New("no wd") }
	if code := h.run(); code != 1 {
		t.Fatal("getwd error")
	}
	if code := h.run("--nostr-cwd", "rel"); code != 1 {
		t.Fatal("getwd error for relative path")
	}
}

func TestAppErrors(t *testing.T) {
	h := newHarness(t)
	h.vars["NOSTROMO_HOME"] = ""
	h.vars["HOME"] = ""
	if code := h.run("ls"); code != 1 || !strings.Contains(h.err.String(), "HOME") {
		t.Fatalf("%d %s", code, h.err)
	}
	h.vars["HOME"] = h.home
	if p, _ := h.env.Paths(); p.Home != filepath.Join(h.home, ".nostromo") {
		t.Fatalf("paths: %v", p)
	}
	os.MkdirAll(filepath.Join(h.home, ".nostromo"), 0o755)
	os.WriteFile(filepath.Join(h.home, ".nostromo", "config.toml"), []byte("port = "), 0o644)
	if code := h.run("ls"); code != 1 {
		t.Fatal("bad config must fail")
	}
}

func TestLsAttachResumeStopRm(t *testing.T) {
	h := newHarness(t)
	if code := h.run("ls"); code != 0 || !strings.Contains(h.out.String(), "no chats yet") {
		t.Fatalf("empty ls: %s", h.out)
	}
	h.vars["HOME"] = filepath.Dir(h.cwd)
	h.run("--nostr-detach", "--nostr-title", "first")
	r := h.onlyChat(t)
	id := r.ID[:8]

	h.run("ls")
	out := h.out.String()
	if !strings.Contains(out, "~/"+filepath.Base(h.cwd)) || !strings.Contains(out, id) || !strings.Contains(out, "running") || !strings.Contains(out, "first") {
		t.Fatalf("ls: %s", out)
	}

	if code := h.run("attach", id); code != 0 || len(h.execs) != 1 {
		t.Fatalf("attach: %d %v %s", code, h.execs, h.err)
	}
	if code := h.run("resume", id); code != 0 || len(h.execs) != 2 {
		t.Fatalf("resume running attaches: %d %s", code, h.err)
	}

	h.fake.Kill(r.TmuxSession, 7)
	h.run("ls")
	if !strings.Contains(h.out.String(), "failed(7)") {
		t.Fatalf("ls failed: %s", h.out)
	}

	if code := h.run("stop", id); code != 0 || !strings.Contains(h.out.String(), "stopped") {
		t.Fatalf("stop: %d %s", code, h.err)
	}
	if code := h.run("stop", id); code != 1 {
		t.Fatal("stop twice must fail")
	}
	if code := h.run("attach", id); code != 1 || !strings.Contains(h.err.String(), "agent resume") {
		t.Fatalf("attach stopped: %s", h.err)
	}
	if code := h.run("resume", id); code != 0 || len(h.execs) != 3 {
		t.Fatalf("resume stopped: %d %s", code, h.err)
	}
	if code := h.run("rm", id); code != 0 || !strings.Contains(h.out.String(), "deleted") {
		t.Fatalf("rm: %d %s", code, h.err)
	}
	for _, cmd := range []string{"attach", "resume", "stop", "rm"} {
		if code := h.run(cmd, id); code != 1 || !strings.Contains(h.err.String(), "not found") {
			t.Fatalf("%s missing: %d %s", cmd, code, h.err)
		}
	}
}

func TestLsReportsBadRecordsAndErrors(t *testing.T) {
	h := newHarness(t)
	h.run("--nostr-detach")
	chats := filepath.Join(h.vars["NOSTROMO_HOME"], "chats")
	os.WriteFile(filepath.Join(chats, "abcdef00-0000-4000-8000-000000000000.json"), []byte("{"), 0o600)
	h.run("--nostr-detach")
	if code := h.run("ls"); code != 0 || !strings.Contains(h.err.String(), "1 unreadable") {
		t.Fatalf("%d %s", code, h.err)
	}
	h.fake.Errs["ListPanes"] = errors.New("boom")
	for _, args := range [][]string{{"ls"}, {"attach", "abc"}} {
		if code := h.run(args...); code != 1 {
			t.Fatalf("%v must fail", args)
		}
	}
}

func TestResumeLiveError(t *testing.T) {
	h := newHarness(t)
	h.run("--nostr-detach")
	r := h.onlyChat(t)
	h.fake.Kill(r.TmuxSession, 0)
	h.fake.Errs["ListPanes"] = errors.New("boom")
	if code := h.run("resume", r.ID); code != 1 {
		t.Fatal("resume must fail when tmux fails")
	}
}

func TestExternalCommands(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.env.Commands["serve"] = func(_ context.Context, args []string, _ Env) int { got = args; return 5 }
	if code := h.run("serve", "--x"); code != 5 || strings.Join(got, " ") != "--x" {
		t.Fatalf("%d %v", code, got)
	}
}

func TestHelpers(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{10 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago", 50 * time.Hour: "2d ago"}
	for d, want := range cases {
		if got := Ago(now.Add(-d), now); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
	if DisplayTitle("x", now) != "x" || !strings.HasPrefix(DisplayTitle("", now), "Chat from Nov 16") {
		t.Fatal("DisplayTitle")
	}
	if short("abc") != "abc" || homeRel("/h/x", "/h") != "~/x" || homeRel("/hx", "/h") != "/hx" || homeRel("/h", "/h") != "~" {
		t.Fatal("helpers")
	}
}

func TestDefaultEnv(t *testing.T) {
	e := DefaultEnv()
	if e.Getenv == nil || e.Exec == nil || e.NewTmux == nil || e.Now == nil || e.Commands == nil {
		t.Fatal("DefaultEnv incomplete")
	}
	e.IsTTY()
	e.TermSize()
	if e.NewTmux(config.Default(), config.Paths{Home: "/x"}) == nil {
		t.Fatal("tmux")
	}
	if err := e.Exec([]string{"/definitely/not/a/binary"}, nil); err == nil {
		t.Fatal("exec of a missing binary must fail")
	}
}
