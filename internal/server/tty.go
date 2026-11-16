package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/aist-dev/nostromo/internal/state"
)

// WebSocket close codes used by the terminal endpoint.
const (
	CloseStopped = 4404 // the chat has no tmux session
	CloseExited  = 4410 // the tmux session ended
)

// Default ping settings for terminal connections.
const (
	DefaultPingInterval = 30 * time.Second
	DefaultPingTimeout  = 60 * time.Second
)

type ttySession struct {
	cancel context.CancelFunc
}

type clientMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func clampSize(v, def int) uint16 {
	if v <= 0 {
		return uint16(def)
	}
	if v > 1000 {
		return 1000
	}
	return uint16(v)
}

func (s *Server) tty(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opt.Chats.Store.Get(id); err != nil {
		s.fail(w, err)
		return
	}
	live, err := s.opt.Chats.Live(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The Origin header was already checked by ServeHTTP, including the
		// X-Forwarded-Host case that the library check does not know about.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.opt.Logger.Printf("websocket accept: %v", err)
		return
	}
	conn.SetReadLimit(MaxBody)
	if live.Status == state.Stopped {
		conn.Close(CloseStopped, "chat is stopped")
		return
	}
	if live.Status == state.Failed {
		s.serveSnapshot(conn, live)
		return
	}
	q := r.URL.Query()
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	s.serveTTY(conn, live, clampSize(cols, 120), clampSize(rows, 40))
}

// track registers a terminal connection so Shutdown can end it.
func (s *Server) track() (context.Context, func()) {
	ctx, cancel := context.WithCancel(s.shutdownCtx)
	sess := &ttySession{cancel: cancel}
	s.mu.Lock()
	s.ttys[sess] = struct{}{}
	s.ttyWG.Add(1)
	s.mu.Unlock()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.ttys, sess)
		s.mu.Unlock()
		s.ttyWG.Done()
	}
}

// serveSnapshot sends the last screen of a failed chat, read-only. Attaching would
// let tmux resize the dead pane and push its last lines out of view.
func (s *Server) serveSnapshot(conn *websocket.Conn, live state.Live) {
	ctx, done := s.track()
	defer done()
	screen, err := s.opt.Chats.Tmux.Capture(ctx, live.Session)
	if err != nil {
		conn.Close(CloseExited, "session ended")
		return
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = conn.Write(wctx, websocket.MessageBinary, []byte(screen))
	wcancel()
	if err != nil {
		conn.CloseNow()
		return
	}
	readErr := make(chan struct{})
	go func() {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				close(readErr)
				return
			}
		}
	}()
	select {
	case <-readErr:
	case <-ctx.Done():
		conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
	conn.CloseNow()
}

func (s *Server) serveTTY(conn *websocket.Conn, live state.Live, cols, rows uint16) {
	ctx, done := s.track()
	defer done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	argv := s.opt.Chats.Tmux.AttachArgs(live.Session)
	cmd := exec.Command(argv[0], argv[1:]...)
	env := []string{"TERM=xterm-256color"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TERM=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		s.opt.Logger.Printf("start tmux attach: %v", err)
		conn.Close(websocket.StatusInternalError, "cannot attach")
		return
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// The connection is driven with contexts that are not tied to ctx: cancelling
	// a context passed to Read or Write closes the WebSocket immediately, which
	// would prevent sending a proper close frame on shutdown.
	var wg sync.WaitGroup
	// tmux -> browser
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// browser -> tmux
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var m clientMsg
				if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
					pty.Setsize(f, &pty.Winsize{Cols: clampSize(m.Cols, 120), Rows: clampSize(m.Rows, 40)})
				}
				continue
			}
			f.Write(data)
		}
	}()
	// keepalive
	interval, timeout := s.opt.PingInterval, s.opt.PingTimeout
	if interval <= 0 {
		interval = DefaultPingInterval
	}
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(context.Background(), timeout)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	select {
	case <-exited:
		// Session closed (or attach failed): tell the client, then close.
		wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn.Write(wctx, websocket.MessageText, []byte(`{"type":"exit"}`))
		wcancel()
		conn.Close(CloseExited, "session ended")
	case <-ctx.Done():
		// Client went away or the server is shutting down: end only the attach process.
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
		if errors.Is(s.shutdownCtx.Err(), context.Canceled) {
			conn.Close(websocket.StatusGoingAway, "server shutting down")
		}
		conn.CloseNow()
	}
	cancel()
	f.Close()
	wg.Wait()
}
