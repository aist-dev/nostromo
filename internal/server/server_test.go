package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tailscale"
	"github.com/aist-dev/nostromo/internal/tmux/faketmux"
)

type rig struct {
	srv   *Server
	ts    *httptest.Server
	fake  *faketmux.Fake
	chats *chat.Service
	home  string
	cwd   string
	n     int
}

func newRig(t *testing.T, mutate func(*Options)) *rig {
	t.Helper()
	r := &rig{fake: faketmux.New(), home: t.TempDir(), cwd: t.TempDir()}
	paths := config.Paths{Home: filepath.Join(r.home, ".nostromo")}
	cfg := config.Default()
	cfg.Peers = []string{"other", "full.example.com"}
	r.chats = chat.New(store.New(paths.Chats()), r.fake, cfg, paths)
	r.chats.Shell = "/bin/sh"
	r.chats.NewID = func() string {
		r.n++
		return strings.Replace("00000000-0000-4000-8000-00000000000x", "x", string(rune('0'+r.n)), 1)
	}
	o := Options{
		Chats: r.chats, Config: cfg, Version: "1.2.3", Hostname: "box", Home: r.home,
		TailscaleStatus: func(context.Context) (tailscale.Status, error) {
			return tailscale.Status{BackendState: "Running", DNSName: "box.tail1.ts.net", HostName: "box"}, nil
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	r.srv = New(o)
	r.ts = httptest.NewServer(r.srv)
	t.Cleanup(func() { r.ts.Close(); r.srv.Shutdown() })
	return r
}

type resp struct {
	code int
	body []byte
	hdr  http.Header
}

func (r resp) errCode() string {
	var e apiError
	json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func (r *rig) do(t *testing.T, method, path string, body any, hdr map[string]string) resp {
	t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.ts.URL+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Origin", r.ts.URL)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
		if k == "Host" {
			req.Host = v
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, b, res.Header}
}

func (r *rig) create(t *testing.T) state.Chat {
	t.Helper()
	res := r.do(t, "POST", "/api/chats", map[string]string{"cwd": r.cwd, "title": "hello"}, nil)
	if res.code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.code, res.body)
	}
	var c state.Chat
	json.Unmarshal(res.body, &c)
	return c
}

func TestHealthzAndStatic(t *testing.T) {
	r := newRig(t, nil)
	res := r.do(t, "GET", "/healthz", nil, nil)
	if res.code != 200 || !strings.Contains(string(res.body), `"version":"1.2.3"`) {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if res := r.do(t, "GET", "/", nil, nil); res.code != 200 || string(res.body) != "nostromo" || res.hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("static placeholder: %d %s", res.code, res.body)
	}
	if res := r.do(t, "GET", "/api/nope", nil, nil); res.code != 404 || res.errCode() != "not_found" {
		t.Fatalf("unknown api: %d", res.code)
	}
	if res := r.do(t, "POST", "/index.html", nil, nil); res.code != 405 {
		t.Fatalf("POST to static: %d", res.code)
	}
}

func TestStateGroupsPeersAndETag(t *testing.T) {
	r := newRig(t, func(o *Options) { o.UpdateAvailable = func() string { return "1.3.0" } })
	empty := r.do(t, "GET", "/api/state", nil, nil)
	if !strings.Contains(string(empty.body), `"groups":[]`) {
		t.Fatalf("empty groups must be [] not null: %s", empty.body)
	}
	c := r.create(t)
	res := r.do(t, "GET", "/api/state", nil, nil)
	var snap state.Snapshot
	json.Unmarshal(res.body, &snap)
	if snap.Host != "box" || snap.Version != "1.2.3" || snap.UpdateAvailable != "1.3.0" || snap.DefaultAgent != "claude" ||
		len(snap.Groups) != 1 || snap.Groups[0].Chats[0].ID != c.ID || snap.Groups[0].Chats[0].Status != state.Running ||
		len(snap.RecentDirs) != 1 {
		t.Fatalf("snapshot: %s", res.body)
	}
	if snap.Peers[0].URL != "https://other.tail1.ts.net" || snap.Peers[1].URL != "https://full.example.com" {
		t.Fatalf("peers: %+v", snap.Peers)
	}
	etag := res.hdr.Get("ETag")
	if res2 := r.do(t, "GET", "/api/state", nil, map[string]string{"If-None-Match": etag}); res2.code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", res2.code)
	}
	r.do(t, "PATCH", "/api/chats/"+c.ID, map[string]string{"title": "renamed"}, nil)
	if res3 := r.do(t, "GET", "/api/state", nil, map[string]string{"If-None-Match": etag}); res3.code != 200 {
		t.Fatalf("changed content must not be 304: %d", res3.code)
	}
	r.fake.Errs["ListPanes"] = errors.New("boom")
	if res := r.do(t, "GET", "/api/state", nil, nil); res.code != 500 || res.errCode() != "tmux_error" {
		t.Fatalf("tmux failure: %d %s", res.code, res.body)
	}
}

func TestCreateErrors(t *testing.T) {
	r := newRig(t, nil)
	cases := []struct {
		body any
		code int
		err  string
	}{
		{`{`, 400, "bad_request"},
		{`{"cwd": "/tmp", "bogus": 1}`, 400, "bad_request"},
		{map[string]string{"cwd": "relative"}, 400, "bad_request"},
		{map[string]string{}, 400, "bad_request"},
		{map[string]string{"cwd": "/definitely/missing"}, 400, "cwd_missing"},
		{map[string]string{"cwd": r.cwd, "agent": "ghost"}, 400, "unknown_agent"},
	}
	for _, c := range cases {
		res := r.do(t, "POST", "/api/chats", c.body, nil)
		if res.code != c.code || res.errCode() != c.err {
			t.Errorf("%v: %d %s", c.body, res.code, res.body)
		}
	}
	big := `{"cwd":"` + strings.Repeat("a", MaxBody) + `"}`
	if res := r.do(t, "POST", "/api/chats", big, nil); res.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit: %d", res.code)
	}
	os.MkdirAll(filepath.Join(r.home, "proj"), 0o755)
	if res := r.do(t, "POST", "/api/chats", map[string]string{"cwd": "~/proj"}, nil); res.code != 201 {
		t.Fatalf("tilde: %d %s", res.code, res.body)
	}
	r.fake.Errs["NewSession"] = errors.New("boom")
	if res := r.do(t, "POST", "/api/chats", map[string]string{"cwd": r.cwd}, nil); res.code != 500 || res.errCode() != "tmux_error" {
		t.Fatalf("tmux: %d %s", res.code, res.body)
	}
}

func TestChatLifecycle(t *testing.T) {
	r := newRig(t, nil)
	c := r.create(t)
	base := "/api/chats/" + c.ID

	if res := r.do(t, "POST", base+"/resume", nil, nil); res.code != 409 || res.errCode() != "already_running" {
		t.Fatalf("resume running: %d %s", res.code, res.body)
	}
	if res := r.do(t, "POST", base+"/scroll", map[string]string{"dir": "up"}, nil); res.code != 204 {
		t.Fatalf("scroll: %d %s", res.code, res.body)
	}
	if res := r.do(t, "POST", base+"/stop", nil, nil); res.code != 204 {
		t.Fatalf("stop: %d %s", res.code, res.body)
	}
	if res := r.do(t, "POST", base+"/stop", nil, nil); res.code != 409 || res.errCode() != "not_running" {
		t.Fatalf("stop twice: %d", res.code)
	}
	if res := r.do(t, "POST", base+"/scroll", map[string]string{"dir": "up"}, nil); res.code != 409 || res.errCode() != "not_running" {
		t.Fatalf("scroll stopped: %d", res.code)
	}
	res := r.do(t, "POST", base+"/resume", nil, nil)
	if res.code != 200 || !strings.Contains(string(res.body), `"status":"running"`) {
		t.Fatalf("resume: %d %s", res.code, res.body)
	}
	res = r.do(t, "PATCH", base, map[string]string{"title": " new\ttitle "}, nil)
	if res.code != 200 || !strings.Contains(string(res.body), `"title":"new title"`) {
		t.Fatalf("rename: %d %s", res.code, res.body)
	}
	if res := r.do(t, "PATCH", base, map[string]string{}, nil); res.code != 400 {
		t.Fatalf("rename without title: %d", res.code)
	}
	if res := r.do(t, "PATCH", base, `{`, nil); res.code != 400 {
		t.Fatalf("rename bad json: %d", res.code)
	}
	if res := r.do(t, "DELETE", base, nil, nil); res.code != 204 {
		t.Fatalf("delete: %d", res.code)
	}
	for _, rq := range [][2]string{{"POST", base + "/resume"}, {"POST", base + "/stop"}, {"DELETE", base}} {
		if res := r.do(t, rq[0], rq[1], nil, nil); res.code != 404 || res.errCode() != "not_found" {
			t.Errorf("%v: %d", rq, res.code)
		}
	}
	if res := r.do(t, "PATCH", base, map[string]string{"title": "x"}, nil); res.code != 404 {
		t.Fatalf("rename missing: %d", res.code)
	}
	if res := r.do(t, "POST", base+"/scroll", map[string]string{"dir": "up"}, nil); res.code != 404 {
		t.Fatalf("scroll missing: %d", res.code)
	}
}

func TestScrollErrors(t *testing.T) {
	r := newRig(t, nil)
	c := r.create(t)
	base := "/api/chats/" + c.ID + "/scroll"
	if res := r.do(t, "POST", base, map[string]string{"dir": "left"}, nil); res.code != 400 {
		t.Fatalf("bad dir: %d", res.code)
	}
	if res := r.do(t, "POST", base, `nope`, nil); res.code != 400 {
		t.Fatalf("bad json: %d", res.code)
	}
	r.fake.Errs["Scroll"] = errors.New("x")
	if res := r.do(t, "POST", base, map[string]string{"dir": "down"}, nil); res.code != 500 || res.errCode() != "tmux_error" {
		t.Fatalf("scroll failure: %d %s", res.code, res.body)
	}
	r.fake.Errs["ListPanes"] = errors.New("x")
	if res := r.do(t, "POST", base, map[string]string{"dir": "down"}, nil); res.code != 500 {
		t.Fatalf("list failure: %d", res.code)
	}
	// chatView falls back to "stopped" when tmux cannot be asked.
	res := r.do(t, "PATCH", "/api/chats/"+c.ID, map[string]string{"title": "x"}, nil)
	if res.code != 200 || !strings.Contains(string(res.body), `"status":"stopped"`) {
		t.Fatalf("view fallback: %d %s", res.code, res.body)
	}
}

func TestInternalErrorAndAmbiguous(t *testing.T) {
	r := newRig(t, nil)
	w := httptest.NewRecorder()
	r.srv.fail(w, errors.New("weird"))
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"internal"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.srv.fail(w, store.ErrAmbiguous)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	c := r.create(t)
	r.chats.Store.Dir = filepath.Join(r.cwd, "nodir")
	os.WriteFile(r.chats.Store.Dir, nil, 0o644)
	if res := r.do(t, "PATCH", "/api/chats/"+c.ID, map[string]string{"title": "x"}, nil); res.code != 500 || res.errCode() != "internal" {
		t.Fatalf("store failure: %d %s", res.code, res.body)
	}
}

func TestDirs(t *testing.T) {
	r := newRig(t, nil)
	for _, d := range []string{"b", "A", ".hidden", "c/deep"} {
		os.MkdirAll(filepath.Join(r.home, d), 0o755)
	}
	os.WriteFile(filepath.Join(r.home, "file.txt"), nil, 0o644)
	os.Symlink(filepath.Join(r.home, "b"), filepath.Join(r.home, "link"))
	os.Symlink(filepath.Join(r.home, "file.txt"), filepath.Join(r.home, "filelink"))
	var dl DirList
	res := r.do(t, "GET", "/api/dirs", nil, nil)
	json.Unmarshal(res.body, &dl)
	if res.code != 200 || dl.Path != r.home || dl.Parent != filepath.Dir(r.home) || strings.Join(dl.Dirs, ",") != "A,b,c,link" {
		t.Fatalf("home: %d %s", res.code, res.body)
	}
	json.Unmarshal(r.do(t, "GET", "/api/dirs?all=1&path=~", nil, nil).body, &dl)
	if strings.Join(dl.Dirs, ",") != ".hidden,A,b,c,link" {
		t.Fatalf("all: %v", dl.Dirs)
	}
	json.Unmarshal(r.do(t, "GET", "/api/dirs?path="+filepath.Join(r.home, "c"), nil, nil).body, &dl)
	if strings.Join(dl.Dirs, ",") != "deep" {
		t.Fatalf("sub: %v", dl.Dirs)
	}
	if res := r.do(t, "GET", "/api/dirs?path=relative", nil, nil); res.code != 400 {
		t.Fatalf("relative: %d", res.code)
	}
	if res := r.do(t, "GET", "/api/dirs?path=/definitely/missing", nil, nil); res.code != 404 {
		t.Fatalf("missing: %d", res.code)
	}
}

func TestUpdateEndpoint(t *testing.T) {
	r := newRig(t, nil)
	if res := r.do(t, "POST", "/api/update", nil, nil); res.code != 404 {
		t.Fatalf("no updater: %d", res.code)
	}
	called := false
	r2 := newRig(t, func(o *Options) { o.StartUpdate = func() error { called = true; return nil } })
	if res := r2.do(t, "POST", "/api/update", nil, nil); res.code != 202 || !called {
		t.Fatalf("update: %d", res.code)
	}
	r3 := newRig(t, func(o *Options) { o.StartUpdate = func() error { return errors.New("x") } })
	if res := r3.do(t, "POST", "/api/update", nil, nil); res.code != 500 {
		t.Fatalf("update failure: %d", res.code)
	}
}

func TestHostCheck(t *testing.T) {
	r := newRig(t, nil)
	for host, want := range map[string]int{
		"localhost:8787": 200, "127.0.0.1": 200, "[::1]:8787": 200,
		"box.tail1.ts.net": 200, "BOX.tail1.ts.net:443": 200, "box": 403,
		"evil.example.com": 403, "box.tail1.ts.net.evil.com": 403,
	} {
		if res := r.do(t, "GET", "/healthz", nil, map[string]string{"Host": host}); res.code != want {
			t.Errorf("%s: %d want %d", host, res.code, want)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Host = ""
	r.srv.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("empty host: %d", w.Code)
	}
}

func TestHostCheckWithoutTailscale(t *testing.T) {
	r := newRig(t, func(o *Options) { o.TailscaleStatus = nil })
	if res := r.do(t, "GET", "/healthz", nil, map[string]string{"Host": "box.tail1.ts.net"}); res.code != 403 {
		t.Fatalf("%d", res.code)
	}
	if res := r.do(t, "GET", "/api/state", nil, nil); res.code != 200 {
		t.Fatalf("%d", res.code)
	}
	calls := 0
	r2 := newRig(t, func(o *Options) {
		o.TailscaleStatus = func(context.Context) (tailscale.Status, error) {
			calls++
			return tailscale.Status{}, errors.New("down")
		}
	})
	r2.do(t, "GET", "/healthz", nil, map[string]string{"Host": "a.example"})
	r2.do(t, "GET", "/healthz", nil, map[string]string{"Host": "b.example"})
	if calls != 1 {
		t.Fatalf("lookups must be rate limited, got %d", calls)
	}
}

func TestOriginCheck(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Config.AllowedUsers = []string{"*"} })
	c := r.create(t)
	path := "/api/chats/" + c.ID
	cases := []struct {
		hdr  map[string]string
		want int
	}{
		{map[string]string{"Origin": ""}, 403},
		{map[string]string{"Origin": "https://evil.example.com"}, 403},
		{map[string]string{"Origin": "null"}, 403},
		{map[string]string{"Origin": "::bad"}, 403},
		{map[string]string{"Origin": "https://box.tail1.ts.net", "X-Forwarded-Host": "box.tail1.ts.net"}, 403},
		{map[string]string{"Origin": "https://box.tail1.ts.net", "Host": "box.tail1.ts.net"}, 200},
		{map[string]string{"Origin": "https://box.tail1.ts.net:8443", "Host": "box.tail1.ts.net:8443"}, 200},
		{map[string]string{"Origin": "https://box.tail1.ts.net", "Host": "box.tail1.ts.net:8443"}, 403},
		{map[string]string{"Origin": "http://box.tail1.ts.net:8080", "Host": "box.tail1.ts.net"}, 403},
	}
	for _, c := range cases {
		if res := r.do(t, "PATCH", path, map[string]string{"title": "x"}, c.hdr); res.code != c.want {
			t.Errorf("%v: %d want %d", c.hdr, res.code, c.want)
		}
	}
	// Every request to /tty is checked, whatever its Upgrade header says.
	for _, up := range []string{"websocket", "x, websocket", ""} {
		if res := r.do(t, "GET", path+"/tty", nil, map[string]string{"Upgrade": up, "Origin": "https://evil.example.com"}); res.code != 403 {
			t.Fatalf("tty origin with Upgrade %q: %d", up, res.code)
		}
	}
}

func TestAllowedUsers(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Config.AllowedUsers = []string{"me@example.com"} })
	if res := r.do(t, "GET", "/api/state", nil, nil); res.code != 403 || res.errCode() != "forbidden" {
		t.Fatalf("no identity: %d", res.code)
	}
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{IdentityHeader: "other@example.com"}); res.code != 403 {
		t.Fatalf("other user: %d", res.code)
	}
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{IdentityHeader: "ME@example.com"}); res.code != 200 {
		t.Fatalf("allowed user: %d", res.code)
	}
	if res := r.do(t, "GET", "/healthz", nil, nil); res.code != 200 {
		t.Fatalf("healthz stays open: %d", res.code)
	}
	star := newRig(t, func(o *Options) { o.Config.AllowedUsers = []string{"*"} })
	if res := star.do(t, "GET", "/api/state", nil, map[string]string{"Host": "box.tail1.ts.net"}); res.code != 200 {
		t.Fatalf("* allows every tailnet user: %d", res.code)
	}
	// The identity header is not trusted from a non-loopback connection.
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.RemoteAddr = "100.64.0.9:5555"
	req.Header.Set(IdentityHeader, "me@example.com")
	if r.srv.identityAllowed(req) {
		t.Fatal("identity from a remote connection must be ignored")
	}
	req.RemoteAddr = "garbage"
	if r.srv.identityAllowed(req) {
		t.Fatal("unparsable remote address")
	}
}

func TestEmptyAllowedUsersIsLocalOnly(t *testing.T) {
	r := newRig(t, nil)
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{"Host": "localhost:8787"}); res.code != 200 {
		t.Fatalf("local: %d", res.code)
	}
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{"Host": "box.tail1.ts.net"}); res.code != 403 {
		t.Fatalf("tailnet name with empty allowed_users: %d", res.code)
	}
}

func TestRotatingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "server.log")
	f, err := OpenRotating(p, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		if _, err := f.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	read := func(name string) string { b, _ := os.ReadFile(name); return string(b) }
	if read(p) != "dddddddd\n" || read(p+".1") != "cccccccc\n" || read(p+".2") != "bbbbbbbb\n" {
		t.Fatalf("rotation: %q %q %q", read(p), read(p+".1"), read(p+".2"))
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("kept too many files")
	}
	g, _ := OpenRotating(filepath.Join(t.TempDir(), "x.log"), 1, 0)
	g.Write([]byte("12"))
	g.Write([]byte("34"))
	g.Close()
	if _, err := OpenRotating(filepath.Join(p, "sub", "x"), 1, 1); err == nil {
		t.Fatal("open under a file must fail")
	}
	dir := t.TempDir()
	if _, err := OpenRotating(dir, 1, 1); err == nil {
		t.Fatal("open a directory must fail")
	}
	h, _ := OpenRotating(filepath.Join(dir, "y.log"), 1, 1)
	h.Write([]byte("1"))
	os.Remove(filepath.Join(dir, "y.log"))
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if os.Getuid() != 0 {
		if _, err := h.Write([]byte("2")); err == nil {
			t.Fatal("rotation into a read-only dir must fail")
		}
	}
}

func TestHelpers(t *testing.T) {
	if !isLoopback("127.0.0.1:1") || !isLoopback("localhost:1") || !isLoopback("[::1]:1") || isLoopback("0.0.0.0:1") || isLoopback("nonsense") {
		t.Fatal("isLoopback")
	}
	if shortHostname() == "" {
		t.Fatal("hostname")
	}
	if clampSize(0, 7) != 7 || clampSize(5000, 7) != 1000 || clampSize(80, 7) != 80 {
		t.Fatal("clampSize")
	}
	_ = time.Now
}
