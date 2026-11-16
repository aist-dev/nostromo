package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func (r *rig) dial(t *testing.T, id string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(r.ts.URL, "http") + "/api/chats/" + id + "/tty?cols=90&rows=20"
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {r.ts.URL}}})
}

// readUntil collects binary output until it contains want.
func readUntil(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q, got %q: %v", want, got.String(), err)
		}
		if typ == websocket.MessageBinary {
			got.Write(data)
		}
	}
	return got.String()
}

func closeCode(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var texts []string
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err), strings.Join(texts, "")
		}
		if typ == websocket.MessageText {
			texts = append(texts, string(data))
		}
	}
}

func TestTTYStoppedAndMissing(t *testing.T) {
	r := newRig(t, nil)
	if _, res, err := r.dial(t, "00000000-0000-4000-8000-00000000000f"); err == nil || res == nil || res.StatusCode != 404 {
		t.Fatalf("missing chat must be 404 before upgrade: %v", err)
	}
	c := r.create(t)
	r.fake.Kill("agent-00000000", 0)
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := closeCode(t, conn); code != CloseStopped {
		t.Fatalf("close code %d", code)
	}
	r.fake.Errs["ListPanes"] = errors.New("x")
	if _, res, err := r.dial(t, c.ID); err == nil || res.StatusCode != 500 {
		t.Fatalf("tmux failure must be 500: %v", err)
	}
}

func TestTTYEchoAndExit(t *testing.T) {
	r := newRig(t, nil)
	r.fake.AttachCmd = []string{"sh", "-c", `stty -echo; printf 'ready '; stty size; read x; echo "got:$x"`}
	c := r.create(t)
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out := readUntil(t, conn, "20 90"); !strings.Contains(out, "ready") {
		t.Fatalf("output: %q", out)
	}
	ctx := context.Background()
	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`))
	conn.Write(ctx, websocket.MessageText, []byte(`not json`))
	conn.Write(ctx, websocket.MessageBinary, []byte("abc\r"))
	readUntil(t, conn, "got:abc")
	code, texts := closeCode(t, conn)
	if code != CloseExited || !strings.Contains(texts, `"exit"`) {
		t.Fatalf("exit: %d %q", code, texts)
	}
}

func TestTTYFailedShowsSnapshot(t *testing.T) {
	r := newRig(t, nil)
	r.fake.Screen = "last screen\r\n"
	c := r.create(t)
	r.fake.Kill("agent-00000000", 3)
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, conn, "last screen")
	conn.Write(context.Background(), websocket.MessageBinary, []byte("typed\r"))
	conn.CloseNow()

	// Shutdown ends a snapshot connection.
	conn2, _, _ := r.dial(t, c.ID)
	readUntil(t, conn2, "last screen")
	go r.srv.Shutdown()
	if code, _ := closeCode(t, conn2); code != websocket.StatusGoingAway {
		t.Fatalf("close code %d", code)
	}
}

func TestTTYSnapshotErrors(t *testing.T) {
	r := newRig(t, nil)
	c := r.create(t)
	r.fake.Kill("agent-00000000", 3)
	r.fake.Errs["Capture"] = errors.New("gone")
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := closeCode(t, conn); code != CloseExited {
		t.Fatalf("close code %d", code)
	}
}

func TestTTYClientDisconnectAndShutdown(t *testing.T) {
	r := newRig(t, func(o *Options) { o.PingInterval = 50 * time.Millisecond })
	r.fake.AttachCmd = []string{"sh", "-c", `trap '' TERM; echo up; sleep 30`}
	c := r.create(t)

	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, conn, "up")
	time.Sleep(120 * time.Millisecond) // a few pings
	conn.CloseNow()

	conn2, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, conn2, "up")
	done := make(chan struct{})
	go func() { r.srv.Shutdown(); close(done) }()
	if code, _ := closeCode(t, conn2); code != websocket.StatusGoingAway {
		t.Fatalf("shutdown close code %d", code)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestTTYAttachFailure(t *testing.T) {
	r := newRig(t, nil)
	r.fake.AttachCmd = []string{"/definitely/not/here"}
	c := r.create(t)
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := closeCode(t, conn); code != websocket.StatusInternalError {
		t.Fatalf("close code %d", code)
	}
}

func TestTTYPingTimeout(t *testing.T) {
	r := newRig(t, func(o *Options) { o.PingInterval, o.PingTimeout = 30*time.Millisecond, 30*time.Millisecond })
	r.fake.AttachCmd = []string{"sh", "-c", `echo up; sleep 30`}
	c := r.create(t)
	conn, _, err := r.dial(t, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, conn, "up")
	// Stop reading: pongs are never processed, so the server gives up.
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}
