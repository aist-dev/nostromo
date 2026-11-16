//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aist-dev/nostromo/internal/adapter"
	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/server"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/testutil"
	"github.com/aist-dev/nostromo/internal/tmux"
)

type rig struct {
	home  string
	tm    *tmux.Client
	chats *chat.Service
	ts    *httptest.Server
	srv   *server.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	fake := adapter.ShellQuote(testutil.FakeAgent(t))
	cfg := config.Default()
	off := false
	cfg.Agents["fake"] = config.Agent{Start: fake + " --session-id {id}", Resume: fake + " --resume {resume_id}", ShellWrap: &off}
	cfg.DefaultAgent = "fake"
	paths := config.Paths{Home: home}
	svc := chat.New(store.New(paths.Chats()), tm, cfg, paths)
	return &rig{home: home, tm: tm, chats: svc}
}

func (r *rig) start(t *testing.T) {
	t.Helper()
	r.srv = server.New(server.Options{Chats: r.chats, Config: r.chats.Config, Version: "test", Home: r.home})
	r.ts = httptest.NewServer(r.srv)
	t.Cleanup(func() { r.ts.Close(); r.srv.Shutdown() })
}

func (r *rig) snapshot(t *testing.T) state.Snapshot {
	t.Helper()
	res, err := http.Get(r.ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var s state.Snapshot
	json.NewDecoder(res.Body).Decode(&s)
	return s
}

func (r *rig) post(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, r.ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Origin", r.ts.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) newChat(t *testing.T) state.Chat {
	t.Helper()
	res := r.post(t, "POST", "/api/chats", map[string]string{"cwd": r.home})
	defer res.Body.Close()
	if res.StatusCode != 201 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	var c state.Chat
	json.NewDecoder(res.Body).Decode(&c)
	testutil.Eventually(t, 5*time.Second, "agent prompt", func() bool {
		return strings.Contains(testutil.CapturePane(r.tm, store.SessionName(c.ID)), "ready>")
	})
	return c
}

func (r *rig) dial(t *testing.T, id string, cols, rows int) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(r.ts.URL, "http") + "/api/chats/" + id + "/tty?cols=" + strconv.Itoa(cols) + "&rows=" + strconv.Itoa(rows)
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {r.ts.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 22)
	return c
}

type reader struct {
	c   *websocket.Conn
	buf strings.Builder
}

func (rd *reader) until(t *testing.T, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for !strings.Contains(rd.buf.String(), want) {
		typ, data, err := rd.c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v\nscreen so far: %q", want, err, rd.buf.String())
		}
		if typ == websocket.MessageBinary {
			rd.buf.Write(data)
		}
	}
}

func (rd *reader) closed(t *testing.T) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var text string
	for {
		typ, data, err := rd.c.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err), text
		}
		if typ == websocket.MessageText {
			text += string(data)
		}
	}
}

func send(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

// Integration test 6: a chat created while the server was down shows up at once.
func TestChatCreatedWhileServerDown(t *testing.T) {
	r := newRig(t)
	rec, err := r.chats.Start(context.Background(), chat.StartOptions{Cwd: r.home})
	if err != nil {
		t.Fatal(err)
	}
	r.start(t)
	s := r.snapshot(t)
	if len(s.Groups) != 1 || s.Groups[0].Chats[0].ID != rec.ID || s.Groups[0].Chats[0].Status != state.Running {
		t.Fatalf("snapshot: %+v", s)
	}
}

// Integration tests 9, 10 and 12: echo, resize, and disconnect leaves the session alive.
func TestTerminalEchoResizeDisconnect(t *testing.T) {
	r := newRig(t)
	r.start(t)
	c := r.newChat(t)
	conn := r.dial(t, c.ID, 100, 30)
	rd := &reader{c: conn}
	rd.until(t, "ready>")
	send(t, conn, "hello\r")
	rd.until(t, "echo:hello")

	send(t, conn, "size\r")
	rd.until(t, "size:30 100")
	conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":25}`))
	time.Sleep(300 * time.Millisecond)
	send(t, conn, "size\r")
	rd.until(t, "size:25 90")

	conn.CloseNow()
	testutil.Eventually(t, 5*time.Second, "attach process gone", func() bool {
		panes, _ := r.tm.ListPanes(context.Background())
		return len(panes) == 1 && panes[0].Attached == 0
	})
	if l, _ := r.chats.Live(context.Background(), c.ID); l.Status != state.Running {
		t.Fatalf("session must survive a disconnect: %+v", l)
	}
}

// Integration test 11: two clients see the same output; one leaving does not affect the other.
func TestTwoClients(t *testing.T) {
	r := newRig(t)
	r.start(t)
	c := r.newChat(t)
	a := &reader{c: r.dial(t, c.ID, 80, 24)}
	b := &reader{c: r.dial(t, c.ID, 80, 24)}
	a.until(t, "ready>")
	b.until(t, "ready>")
	send(t, a.c, "one\r")
	a.until(t, "echo:one")
	b.until(t, "echo:one")
	a.c.CloseNow()
	send(t, b.c, "two\r")
	b.until(t, "echo:two")
	b.c.CloseNow()
}

// Integration tests 3 and 13: a failed chat shows its last screen; deleting a running chat ends /tty.
func TestFailedScreenAndDelete(t *testing.T) {
	r := newRig(t)
	r.start(t)
	c := r.newChat(t)
	testutil.SendKeys(r.tm, store.SessionName(c.ID), "exit 3")
	testutil.Eventually(t, 5*time.Second, "failed", func() bool {
		l, _ := r.chats.Live(context.Background(), c.ID)
		return l.Status == state.Failed
	})
	rd := &reader{c: r.dial(t, c.ID, 80, 24)}
	rd.until(t, "bye 3")
	rd.c.CloseNow()

	res := r.post(t, "POST", "/api/chats/"+c.ID+"/resume", nil)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("resume failed chat: %d", res.StatusCode)
	}
	testutil.Eventually(t, 5*time.Second, "agent prompt", func() bool {
		return strings.Contains(testutil.CapturePane(r.tm, store.SessionName(c.ID)), "ready>")
	})
	live := &reader{c: r.dial(t, c.ID, 80, 24)}
	live.until(t, "ready>")
	res = r.post(t, "DELETE", "/api/chats/"+c.ID, nil)
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if code, text := live.closed(t); code != server.CloseExited || !strings.Contains(text, `"exit"`) {
		t.Fatalf("delete must end the terminal: %d %q", code, text)
	}
	if _, err := os.Stat(filepath.Join(r.home, "chats", c.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("record must be gone")
	}
}

// Integration test 8: restarting the server process (even killing its whole process
// group) leaves the agent running with the same PID.
func TestServerRestartKeepsAgents(t *testing.T) {
	r := newRig(t)
	bin := testutil.AgentBinary(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg := testutil.FakeConfig(testutil.FakeAgent(t), r.tm.Socket) + ""
	cfg = "port = " + strconv.Itoa(port) + "\n" + cfg
	os.WriteFile(filepath.Join(r.home, "config.toml"), []byte(cfg), 0o644)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	startServe := func() *exec.Cmd {
		cmd := exec.Command(bin, "serve")
		cmd.Env = append(os.Environ(), "NOSTROMO_HOME="+r.home)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		testutil.Eventually(t, 10*time.Second, "server up", func() bool {
			res, err := http.Get(base + "/healthz")
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == 200
		})
		return cmd
	}
	stopGroup := func(cmd *exec.Cmd) {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		cmd.Wait()
	}

	srv := startServe()
	req, _ := http.NewRequest("POST", base+"/api/chats", strings.NewReader(`{"cwd":"`+r.home+`"}`))
	req.Header.Set("Origin", base)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("create via server: %v %v", err, res)
	}
	var c state.Chat
	json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()
	before := testutil.FakeRuns
	testutil.Eventually(t, 5*time.Second, "agent started", func() bool { return len(before(r.home)) == 1 })
	pid := before(r.home)[0].PID

	stopGroup(srv)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("agent died with the server: %v", err)
	}
	srv = startServe()
	defer stopGroup(srv)
	res, err = http.Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var s state.Snapshot
	json.NewDecoder(res.Body).Decode(&s)
	res.Body.Close()
	if len(s.Groups) != 1 || s.Groups[0].Chats[0].ID != c.ID || s.Groups[0].Chats[0].Status != state.Running {
		t.Fatalf("after restart: %+v", s)
	}
	panes, _ := r.tm.ListPanes(context.Background())
	if len(panes) != 1 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("same agent process expected: %+v", panes)
	}
}
