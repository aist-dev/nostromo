//go:build integration

package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/aist-dev/nostromo/internal/testutil"
	"github.com/aist-dev/nostromo/internal/tmux"
)

// Integration test 15: running agent inside a nostromo tmux session switches the
// client to the new chat instead of nesting tmux.
func TestLauncherInsideTmuxSwitchesClient(t *testing.T) {
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	agentBin := testutil.AgentBinary(t)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(testutil.FakeConfig(testutil.FakeAgent(t), tm.Socket)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOSTROMO_HOME", home)
	ctx := context.Background()

	// A plain shell session on the nostromo socket, with a real client attached through a PTY.
	if err := tm.NewSession(ctx, tmux.SessionSpec{Name: "shell", ChatID: "", Cwd: home, Command: "/bin/sh", Env: []string{"NOSTROMO_HOME=" + home}}); err != nil {
		t.Fatal(err)
	}
	attach := tm.AttachArgs("shell")
	cmd := exec.Command(attach[0], attach[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); f.Close(); cmd.Wait() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	clientSession := func() string {
		args := append(tm.BaseArgs(), "list-clients", "-F", "#{client_session}")
		out, _ := exec.Command("tmux", args...).Output()
		return strings.TrimSpace(string(out))
	}
	testutil.Eventually(t, 5*time.Second, "client attached", func() bool { return clientSession() == "shell" })

	testutil.SendKeys(tm, "shell", "'"+agentBin+"' --nostr-title inner")
	testutil.Eventually(t, 10*time.Second, "client switched to the new chat", func() bool {
		return strings.HasPrefix(clientSession(), "agent-")
	})
	args := append(tm.BaseArgs(), "list-clients")
	if out, _ := exec.Command("tmux", args...).Output(); strings.Count(strings.TrimSpace(string(out)), "\n") != 0 {
		t.Fatalf("expected exactly one client, got:\n%s", out)
	}
}

// The launcher binary end to end: detach, ls, stop, resume --detach-free path via no TTY, rm.
func TestLauncherBinary(t *testing.T) {
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	agentBin := testutil.AgentBinary(t)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(testutil.FakeConfig(testutil.FakeAgent(t), tm.Socket)), 0o644)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(agentBin, args...)
		cmd.Env = append(os.Environ(), "NOSTROMO_HOME="+home, "TMUX=")
		cmd.Dir = home
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("agent %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	prompt := "it's \"quoted\" $HOME $(id) `id` ; && | \\ \nnew line кириллица 🚀 " + strings.Repeat("x", 8000)
	id := strings.TrimSpace(run("--nostr-detach", "--nostr-title", "bin test", "--flag", "-v", prompt))
	if len(id) != 36 {
		t.Fatalf("detach must print only the id: %q", id)
	}
	runs := testutil.FakeRuns(home)
	testutil.Eventually(t, 5*time.Second, "fake agent start", func() bool { runs = testutil.FakeRuns(home); return len(runs) == 1 })
	if a := runs[0].Args; len(a) != 5 || a[2] != "--flag" || a[3] != "-v" || a[4] != prompt {
		t.Fatalf("the agent must get the arguments unchanged, the prompt last: %q", a)
	}
	if out := run("ls"); !strings.Contains(out, id[:8]) || !strings.Contains(out, "running") || !strings.Contains(out, "bin test") {
		t.Fatalf("ls: %s", out)
	}
	run("stop", id[:8])
	if out := run("ls"); !strings.Contains(out, "stopped") {
		t.Fatalf("ls after stop: %s", out)
	}
	if out := strings.TrimSpace(run("resume", id[:8])); out != id {
		t.Fatalf("resume without a TTY prints the id: %q", out)
	}
	if out := run("ls"); !strings.Contains(out, "running") {
		t.Fatalf("ls after resume: %s", out)
	}
	testutil.Eventually(t, 5*time.Second, "fake agent resume", func() bool { runs = testutil.FakeRuns(home); return len(runs) == 2 })
	if a := runs[1].Args; len(a) != 2 || a[0] != "--resume" {
		t.Fatalf("resume must not repeat the start arguments: %q", a)
	}
	run("rm", id[:8])
	if out := run("ls"); !strings.Contains(out, "no chats yet") {
		t.Fatalf("ls after rm: %s", out)
	}
}
