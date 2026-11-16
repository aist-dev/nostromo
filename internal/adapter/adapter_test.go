package adapter

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/aist-dev/nostromo/internal/config"
)

// runSh executes a command string with /bin/sh and returns its output; it proves
// the quoting round-trips through a real shell.
func runSh(t *testing.T, cmd string) string {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", cmd, err, out)
	}
	return string(out)
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"", "plain", "with space", "it's", `"dq"`, "$HOME", "a;b", "`x`", "back\\slash", "ünïcode dir", "new\nline", "a'b'c"} {
		got := runSh(t, "printf %s "+ShellQuote(s))
		if got != s {
			t.Errorf("quote %q: shell produced %q", s, got)
		}
	}
	if ShellQuote("abc-1.2/x") != "abc-1.2/x" {
		t.Error("safe strings must stay unquoted")
	}
}

func TestRender(t *testing.T) {
	cmd, err := Render("printf '%s|%s|%s' {id} {resume_id} {cwd}", Values{ID: "i d", ResumeID: "r;m", Cwd: "/tmp/a $b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := runSh(t, cmd); got != "i d|r;m|/tmp/a $b" {
		t.Fatalf("got %q", got)
	}
	if _, err := Render("x {bad}", Values{}); err == nil {
		t.Fatal("unknown placeholder must fail")
	}
}

func TestWrap(t *testing.T) {
	if Wrap("echo hi", "/bin/bash", false) != "echo hi" {
		t.Fatal("no-shell wrap must return the command unchanged")
	}
	w := Wrap("printf %s 'a b'", "/bin/bash", true)
	if !strings.HasPrefix(w, "exec /bin/bash -lic ") {
		t.Fatalf("wrap: %s", w)
	}
	if got := runSh(t, strings.Replace(w, "-lic", "-c", 1)); got != "a b" {
		t.Fatalf("wrapped command produced %q", got)
	}
	if w := Wrap("x", "", true); !strings.HasPrefix(w, "exec /bin/sh -ic ") {
		t.Fatalf("default shell: %s", w)
	}
	if w := Wrap("x", "/usr/bin/fish", true); !strings.HasPrefix(w, "exec /usr/bin/fish -lic ") {
		t.Fatalf("fish: %s", w)
	}
}

func TestUserShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if UserShell() != "/bin/zsh" {
		t.Fatal("SHELL not used")
	}
	t.Setenv("SHELL", "")
	if UserShell() != "/bin/sh" {
		t.Fatal("fallback")
	}
}

func TestCommand(t *testing.T) {
	cfg := config.Default()
	a := cfg.Agents["claude"]
	start, err := Command(a, false, Values{ID: "abc", ResumeID: "def"}, "/bin/bash")
	if err != nil || !strings.Contains(start, "claude --dangerously-skip-permissions --session-id abc") {
		t.Fatalf("start: %s %v", start, err)
	}
	resume, err := Command(a, true, Values{ID: "abc", ResumeID: "def"}, "/bin/bash")
	if err != nil || !strings.Contains(resume, "claude --dangerously-skip-permissions --resume def") {
		t.Fatalf("resume: %s %v", resume, err)
	}
	off := false
	a.ShellWrap = &off
	direct, _ := Command(a, false, Values{ID: "abc"}, "/bin/bash")
	if direct != "claude --dangerously-skip-permissions --session-id abc" {
		t.Fatalf("direct: %s", direct)
	}
	if _, err := Command(config.Agent{Start: "x {zzz}"}, false, Values{}, ""); err == nil {
		t.Fatal("bad template must fail")
	}
}
