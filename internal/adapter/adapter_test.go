package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
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
	cmd, err := Render("printf '%s|%s|%s' {id} {resume_id} {cwd}", Values{ID: "i d", ResumeID: "r;m", Cwd: "/tmp/a $b"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := runSh(t, cmd); got != "i d|r;m|/tmp/a $b" {
		t.Fatalf("got %q", got)
	}
	if _, err := Render("x {bad}", Values{}, nil, ""); err == nil {
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
	start, err := Command(a, false, Values{ID: "abc", ResumeID: "def"}, "/bin/bash", "")
	if err != nil || !strings.Contains(start, "claude --dangerously-skip-permissions --session-id abc") {
		t.Fatalf("start: %s %v", start, err)
	}
	resume, err := Command(a, true, Values{ID: "abc", ResumeID: "def"}, "/bin/bash", "")
	if err != nil || !strings.Contains(resume, "claude --dangerously-skip-permissions --resume def") {
		t.Fatalf("resume: %s %v", resume, err)
	}
	off := false
	a.ShellWrap = &off
	direct, _ := Command(a, false, Values{ID: "abc"}, "/bin/bash", "")
	if direct != `claude --dangerously-skip-permissions --session-id abc "$@"` {
		t.Fatalf("direct: %s", direct)
	}
	if _, err := Command(config.Agent{Start: "x {zzz}"}, false, Values{}, "", ""); err == nil {
		t.Fatal("bad template must fail")
	}
}

// trickyArgs must reach the agent unchanged, one argument each.
func trickyArgs() []string {
	return []string{
		`single ' and double " quotes`,
		"$HOME ${HOME} $(echo hi) `echo hi` ; && | \\ \\\\ \\' !! * ?",
		"line one\nline two\n",
		"кириллица и эмодзи 🚀",
		"--help",
		"-x",
		"",
		strings.Repeat("long text with 'quotes' and \\ ", 300), // about 8 KB
	}
}

// argvOf runs cmd with /bin/sh -c (as tmux does) in an empty HOME and returns
// the arguments printf saw.
func argvOf(t *testing.T, cmd string) []string {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%v", err)
	}
	// An interactive shell's rc files may print first; the arguments follow MARK.
	_, args, ok := strings.Cut(string(out), "MARK\x00")
	if !ok {
		t.Fatalf("no marker in %q", out)
	}
	parts := strings.Split(args, "\x00")
	return parts[:len(parts)-1]
}

func TestArgsRoundTrip(t *testing.T) {
	want := trickyArgs()
	on, off := true, false
	shells := []string{"/bin/sh", "/bin/bash", "/bin/dash"}
	for _, wrap := range []*bool{&off, &on} {
		for _, shell := range shells {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
			a := config.Agent{Start: `printf '%s\0' MARK {args}`, Resume: "x", ShellWrap: wrap}
			dir := filepath.Join(t.TempDir(), "run", "id")
			if err := WriteArgs(dir, want); err != nil {
				t.Fatal(err)
			}
			cmd, err := Command(a, false, Values{Args: want}, shell, dir)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(cmd, "quotes") {
				t.Fatalf("arguments must not be inlined: %s", cmd)
			}
			got := argvOf(t, cmd)
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("the argument files must be removed: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("wrap=%v %s: %d args, want %d", *wrap, shell, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("wrap=%v %s: arg %d = %q, want %q", *wrap, shell, i, got[i], want[i])
				}
			}
		}
	}
}

func TestArgsPlacement(t *testing.T) {
	r := func(tmpl string, args ...string) string {
		s, err := Render(tmpl, Values{ID: "i", Args: args}, nil, `"$@"`)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := r("a {id} {args}"); got != `a i "$@"` {
		t.Fatalf("placeholder: %q", got)
	}
	if got := r("a {id}", "p q"); got != `a i "$@"` {
		t.Fatalf("missing {args} appends: %q", got)
	}
	if got := r("a {id}"); got != "a i" {
		t.Fatalf("no args, no placeholder: %q", got)
	}
	a := config.Agent{Start: "s {id} {args}", Resume: "r {resume_id}"}
	cmd, _ := Command(a, true, Values{ResumeID: "z", Args: []string{"prompt"}}, "/bin/sh", "/run/x")
	if strings.Contains(cmd, "/run/x") || strings.Contains(cmd, "$@\"\"") {
		t.Fatalf("resume must not get the start arguments: %s", cmd)
	}
	if err := WriteArgs("/proc/nope/x", []string{"a"}); err == nil {
		t.Fatal("unwritable dir must fail")
	}
	if err := WriteArgs(filepath.Join(t.TempDir(), "d"), []string{"a"}); err != nil {
		t.Fatal(err)
	}
}

func TestFishQuote(t *testing.T) {
	cases := map[string]string{
		"plain":   "plain",
		"":        "''",
		"it's":    `'it\'s'`,
		`a\b`:     `'a\\b'`,
		`\'`:      `'\\\''`,
		"$x `y`":  "'$x `y`'",
		"two\nln": "'two\nln'",
	}
	for in, want := range cases {
		if got := FishQuote(in); got != want {
			t.Errorf("FishQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if Quoter("/usr/bin/fish")("a b") != "'a b'" || Quoter("/bin/zsh")("it's") != `'it'\''s'` {
		t.Fatal("Quoter")
	}
	a := config.Agent{Start: "c {cwd} {args}", Resume: "x"}
	cmd, _ := Command(a, false, Values{Cwd: `a\b`, Args: []string{"p"}}, "/usr/bin/fish", "/d")
	if !strings.Contains(cmd, `'\''a\\b'\''`) || !strings.HasSuffix(cmd, ` $argv' "$@"`) {
		t.Fatalf("fish wrap must quote for fish and use $argv: %s", cmd)
	}
}
