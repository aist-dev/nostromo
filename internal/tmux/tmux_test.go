package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []call
	out   map[string]string // keyed by first tmux subcommand
	errs  map[string]string // stderr per subcommand; non-empty means failure
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, call{name, args})
	sub := args[0]
	for i, a := range args {
		if a == "-L" || a == "-f" {
			continue
		}
		if i > 0 && (args[i-1] == "-L" || args[i-1] == "-f") {
			continue
		}
		sub = a
		break
	}
	if e, ok := f.errs[sub]; ok {
		return nil, []byte(e), errors.New("exit status 1")
	}
	return []byte(f.out[sub]), nil, nil
}

func newFake() (*Client, *fakeRunner) {
	r := &fakeRunner{out: map[string]string{}, errs: map[string]string{}}
	return &Client{Socket: "nostromo", Config: "/h/tmux.conf", Run: r}, r
}

func TestParseVersion(t *testing.T) {
	cases := map[string][2]int{
		"tmux 3.2a\n":    {3, 2},
		"tmux 3.5":       {3, 5},
		"tmux next-3.6":  {3, 6},
		"tmux 2.9":       {2, 9},
		"tmux master":    {999, 0},
		"tmux 10.0-rc2":  {10, 0},
		"tmux openbsd-7": {0, 0},
	}
	for in, want := range cases {
		v, err := ParseVersion(in)
		if in == "tmux openbsd-7" {
			if err == nil {
				t.Errorf("%q: expected error", in)
			}
			continue
		}
		if err != nil || v.Major != want[0] || v.Minor != want[1] {
			t.Errorf("%q: got %+v %v", in, v, err)
		}
	}
	if !(Version{Major: 3, Minor: 2}).AtLeast(3, 2) || (Version{Major: 3, Minor: 1}).AtLeast(3, 2) ||
		!(Version{Major: 4}).AtLeast(3, 2) || (Version{Major: 2, Minor: 9}).AtLeast(3, 2) {
		t.Fatal("AtLeast")
	}
}

func TestVersionAndCheck(t *testing.T) {
	c, r := newFake()
	r.out["-V"] = "tmux 3.4\n"
	v, err := CheckVersion(context.Background(), c)
	if err != nil || v.Minor != 4 {
		t.Fatalf("%+v %v", v, err)
	}
	if r.calls[0].name != "tmux" || len(r.calls[0].args) != 1 {
		t.Fatalf("-V must not carry socket flags: %+v", r.calls[0])
	}
	r.out["-V"] = "tmux 3.1c"
	if _, err := CheckVersion(context.Background(), c); err == nil || !strings.Contains(err.Error(), "3.2") {
		t.Fatalf("old tmux: %v", err)
	}
	r.errs["-V"] = "not found"
	if _, err := CheckVersion(context.Background(), c); err == nil {
		t.Fatal("missing tmux must fail")
	}
}

func TestBaseArgs(t *testing.T) {
	c := New("sock", "")
	if strings.Join(c.BaseArgs(), " ") != "-L sock" || c.bin() != "tmux" {
		t.Fatal(c.BaseArgs())
	}
	c.Config = "/x.conf"
	if strings.Join(c.BaseArgs(), " ") != "-L sock -f /x.conf" {
		t.Fatal(c.BaseArgs())
	}
	c.Bin = ""
	if c.bin() != "tmux" {
		t.Fatal("default bin")
	}
}

func TestNewSessionArgs(t *testing.T) {
	args := NewSessionArgs(SessionSpec{Name: "agent-1234", ChatID: "1234-full", Cwd: "/a b", Command: "exec x", Env: []string{"A=1", "B=2"}})
	got := strings.Join(args, "|")
	want := "new-session|-d|-s|agent-1234|-c|/a b|-x|120|-y|40|-e|A=1|-e|B=2|/bin/sh|-c|exec x|;|set-option|-t|agent-1234|@nostromo_chat|1234-full|;|set-option|-w|-t|agent-1234|remain-on-exit|failed"
	if got != want {
		t.Fatalf("\ngot  %s\nwant %s", got, want)
	}
	if a := NewSessionArgs(SessionSpec{Name: "n", Cwd: "/p/#(id)#x"}); a[5] != "/p/##(id)##x" {
		t.Fatalf("formats in -c must be escaped: %s", a[5])
	}
	args = NewSessionArgs(SessionSpec{Name: "n", Cols: 80, Rows: 24})
	if args[7] != "80" || args[9] != "24" {
		t.Fatalf("size: %v", args)
	}
}

func TestNewSession(t *testing.T) {
	c, r := newFake()
	if err := c.NewSession(context.Background(), SessionSpec{Name: "n", ChatID: "id", Cwd: "/", Command: "x"}); err != nil {
		t.Fatal(err)
	}
	a := r.calls[0].args
	if a[0] != "-L" || a[1] != "nostromo" || a[2] != "-f" || a[3] != "/h/tmux.conf" || a[4] != "new-session" {
		t.Fatalf("args: %v", a)
	}
	r.errs["new-session"] = "duplicate session: n"
	err := c.NewSession(context.Background(), SessionSpec{Name: "n"})
	var te *Error
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "duplicate session") || te.Unwrap() == nil {
		t.Fatalf("error: %v", err)
	}
}

func TestParsePanes(t *testing.T) {
	out := "agent-1\tid-1\t0\t\t1\t100\t\nagent-2\tid-2\t1\t3\t0\t200\t\nmine\t\t0\t\t0\t300\nshort\tline\n\n" +
		"agent-3\tid-3\t1\t\t0\t400\t9\nagent-4\tid-4\t1\t\t0\t500\t\n"
	p := ParsePanes(out)
	if len(p) != 5 {
		t.Fatalf("%+v", p)
	}
	if p[3].DeadStatus != 137 || p[3].Unreaped {
		t.Fatalf("signal: %+v", p[3])
	}
	if !p[4].Unreaped {
		t.Fatalf("unreaped: %+v", p[4])
	}
	if p[0] != (Pane{Session: "agent-1", ChatID: "id-1", Attached: 1, PID: 100}) {
		t.Fatalf("%+v", p[0])
	}
	if !p[1].Dead || p[1].DeadStatus != 3 || p[2].ChatID != "" {
		t.Fatalf("%+v", p[1:])
	}
}

func TestListPanes(t *testing.T) {
	c, r := newFake()
	r.out["list-panes"] = "agent-1\tid\t0\t\t0\t1\n"
	p, err := c.ListPanes(context.Background())
	if err != nil || len(p) != 1 {
		t.Fatalf("%v %v", p, err)
	}
	for _, msg := range []string{
		"no server running on /tmp/tmux-1000/nostromo",
		"error connecting to /tmp/tmux-501/nostromo (No such file or directory)",
	} {
		r.errs["list-panes"] = msg
		p, err := c.ListPanes(context.Background())
		if err != nil || p != nil {
			t.Fatalf("%q: %v %v", msg, p, err)
		}
	}
	r.errs["list-panes"] = "something else"
	if _, err := c.ListPanes(context.Background()); err == nil {
		t.Fatal("other errors must propagate")
	}
}

func TestListPanesReapsZombies(t *testing.T) {
	c, r := newFake()
	r.out["list-panes"] = "agent-1\tid\t1\t\t0\t1\t\n"
	p, err := c.ListPanes(context.Background())
	if err != nil || len(p) != 1 {
		t.Fatalf("%v %v", p, err)
	}
	var subs []string
	for _, cl := range r.calls {
		subs = append(subs, cl.args[4])
	}
	if strings.Join(subs, ",") != "list-panes,run-shell,list-panes" {
		t.Fatalf("calls: %v", subs)
	}
	r.errs["run-shell"] = "boom"
	if p, err := c.ListPanes(context.Background()); err != nil || !p[0].Unreaped {
		t.Fatalf("run-shell failure keeps the first listing: %v %v", p, err)
	}
}

func TestKillSessionAndServer(t *testing.T) {
	c, r := newFake()
	if err := c.KillSession(context.Background(), "agent-1"); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].args[len(r.calls[0].args)-1]; got != "=agent-1" {
		t.Fatalf("exact match target: %s", got)
	}
	r.errs["kill-session"] = "can't find session: =agent-1"
	if err := c.KillSession(context.Background(), "agent-1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
	r.errs["kill-session"] = "permission denied"
	if err := c.KillSession(context.Background(), "agent-1"); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
	if err := c.KillServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.errs["kill-server"] = "no server running on x"
	if err := c.KillServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.errs["kill-server"] = "boom"
	if err := c.KillServer(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestSwitchAndAttach(t *testing.T) {
	c, r := newFake()
	if err := c.SwitchClient(context.Background(), "agent-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls[0].args, " ") != "-L nostromo -f /h/tmux.conf switch-client -t =agent-1" {
		t.Fatal(r.calls[0].args)
	}
	got := strings.Join(c.AttachArgs("agent-1"), " ")
	if got != "tmux -L nostromo -f /h/tmux.conf attach-session -t =agent-1" {
		t.Fatal(got)
	}
}

func TestScroll(t *testing.T) {
	ctx := context.Background()
	c, r := newFake()
	for _, dir := range []string{"up", "down"} {
		r.calls = nil
		if err := c.Scroll(ctx, "agent-1", dir); err != nil {
			t.Fatal(err)
		}
		if len(r.calls) != 2 || r.calls[0].args[4] != "copy-mode" || r.calls[1].args[len(r.calls[1].args)-1] != "halfpage-"+dir {
			t.Fatalf("%s: %+v", dir, r.calls)
		}
	}
	if err := c.Scroll(ctx, "agent-1", "exit"); err != nil {
		t.Fatal(err)
	}
	r.errs["send-keys"] = "not in a mode"
	if err := c.Scroll(ctx, "agent-1", "exit"); err != nil {
		t.Fatalf("exit outside copy mode: %v", err)
	}
	if err := c.Scroll(ctx, "agent-1", "sideways"); err == nil {
		t.Fatal("bad direction")
	}
	r.errs["copy-mode"] = "can't find session: agent-1"
	if err := c.Scroll(ctx, "agent-1", "up"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
	r.errs["copy-mode"] = "weird"
	if err := c.Scroll(ctx, "agent-1", "down"); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
}

func TestErrorFormatting(t *testing.T) {
	e := &Error{Args: []string{"x"}, Err: errors.New("exit status 1")}
	if e.Error() != "tmux x: exit status 1" {
		t.Fatal(e.Error())
	}
	if noServer(errors.New("plain")) || noSession(errors.New("plain")) {
		t.Fatal("plain errors are not tmux errors")
	}
}

func TestInsideSocket(t *testing.T) {
	if !InsideSocket("/tmp/tmux-1000/nostromo,123,0", "nostromo") {
		t.Fatal("same socket")
	}
	if InsideSocket("/tmp/tmux-1000/default,123,0", "nostromo") || InsideSocket("", "nostromo") {
		t.Fatal("other socket")
	}
}

func TestCapture(t *testing.T) {
	c, r := newFake()
	r.out["capture-pane"] = "a\nb\n\n\n"
	got, err := c.Capture(context.Background(), "agent-1")
	if err != nil || got != "a\r\nb\r\n" {
		t.Fatalf("%q %v", got, err)
	}
	if args := strings.Join(r.calls[0].args, " "); !strings.HasSuffix(args, "capture-pane -p -e -J -S -2000 -t =agent-1:") {
		t.Fatal(args)
	}
	r.errs["capture-pane"] = "can't find session: agent-1"
	if _, err := c.Capture(context.Background(), "agent-1"); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
	r.errs["capture-pane"] = "other"
	if _, err := c.Capture(context.Background(), "agent-1"); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
}
