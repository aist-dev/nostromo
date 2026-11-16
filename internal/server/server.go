// Package server is the HTTP API, WebSocket terminal and UI of nostromo.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tailscale"
)

// MaxBody is the largest accepted request body.
const MaxBody = 1 << 20

// IdentityHeader is set by `tailscale serve` to the login of the requesting user.
const IdentityHeader = "Tailscale-User-Login"

// Options configure a Server.
type Options struct {
	Chats    *chat.Service
	Config   config.Config
	Version  string
	Hostname string
	Home     string // user home, default for the folder picker
	// TailscaleStatus reports this node's tailnet names; nil disables lookups.
	TailscaleStatus func(ctx context.Context) (tailscale.Status, error)
	// UpdateAvailable returns a newer version, or "".
	UpdateAvailable func() string
	// StartUpdate launches a detached updater process.
	StartUpdate func() error
	Logger      *log.Logger
	Now         func() time.Time
	// PingInterval and PingTimeout tune terminal keepalives; zero means the defaults.
	PingInterval time.Duration
	PingTimeout  time.Duration
	// Static serves the UI; nil serves a placeholder.
	Static http.Handler
}

// Server handles HTTP requests.
type Server struct {
	opt Options
	mux *http.ServeMux

	mu          sync.Mutex
	tsNames     map[string]bool
	tsSuffix    string
	tsChecked   time.Time
	ttys        map[*ttySession]struct{}
	users       []string
	ttyWG       sync.WaitGroup
	shutdownCtx context.Context
	shutdown    context.CancelFunc
}

// New builds a server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = log.New(io.Discard, "", 0)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{opt: o, mux: http.NewServeMux(), ttys: map[*ttySession]struct{}{}, users: o.Config.AllowedUsers}
	s.shutdownCtx, s.shutdown = context.WithCancel(context.Background())
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /api/state", s.getState)
	s.mux.HandleFunc("POST /api/chats", s.createChat)
	s.mux.HandleFunc("POST /api/chats/{id}/resume", s.resumeChat)
	s.mux.HandleFunc("POST /api/chats/{id}/stop", s.stopChat)
	s.mux.HandleFunc("PATCH /api/chats/{id}", s.renameChat)
	s.mux.HandleFunc("DELETE /api/chats/{id}", s.deleteChat)
	s.mux.HandleFunc("GET /api/chats/{id}/tty", s.tty)
	s.mux.HandleFunc("POST /api/chats/{id}/scroll", s.scroll)
	s.mux.HandleFunc("GET /api/dirs", s.dirs)
	s.mux.HandleFunc("POST /api/update", s.update)
	static := s.opt.Static
	if static == nil {
		static = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "nostromo")
		})
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		default:
			static.ServeHTTP(w, r)
		}
	})
}

// ServeHTTP applies the security checks and dispatches the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	if !s.hostAllowed(r.Context(), r.Host) {
		s.opt.Logger.Printf("rejected host %q from %s", r.Host, r.RemoteAddr)
		writeError(w, http.StatusForbidden, "forbidden", "host not allowed")
		return
	}
	if !s.identityAllowed(r) {
		s.opt.Logger.Printf("rejected user %q for %s %s", r.Header.Get(IdentityHeader), r.Method, r.URL.Path)
		writeError(w, http.StatusForbidden, "forbidden", "user not allowed")
		return
	}
	if needsOriginCheck(r) && !originAllowed(r) {
		s.opt.Logger.Printf("rejected origin %q for %s %s", r.Header.Get("Origin"), r.Method, r.URL.Path)
		writeError(w, http.StatusForbidden, "forbidden", "origin not allowed")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.mux.ServeHTTP(w, r)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func (s *Server) hostAllowed(ctx context.Context, hostport string) bool {
	h := hostOnly(hostport)
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	case "":
		return false
	}
	s.mu.Lock()
	ok := s.tsNames[h]
	stale := s.opt.Now().Sub(s.tsChecked) > 30*time.Second
	s.mu.Unlock()
	if ok || !stale || s.opt.TailscaleStatus == nil {
		return ok
	}
	s.refreshTailscale(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tsNames[h]
}

func (s *Server) refreshTailscale(ctx context.Context) {
	if s.opt.TailscaleStatus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.opt.TailscaleStatus(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tsChecked = s.opt.Now()
	if err != nil {
		return
	}
	names := map[string]bool{}
	// Only full names: tailscale serve always uses them, and a bare host name could
	// be rebound to 127.0.0.1 by a hostile DNS server.
	for _, n := range append([]string{st.DNSName}, st.CertDomains...) {
		if n != "" {
			names[strings.ToLower(n)] = true
		}
	}
	s.tsNames = names
	if i := strings.Index(st.DNSName, "."); i > 0 {
		s.tsSuffix = st.DNSName[i+1:]
	}
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// proxied reports whether a request came through tailscale serve, which always
// adds these headers. A client can add them but cannot make the proxy drop them.
func proxied(r *http.Request) bool {
	h := r.Header
	return h.Get("X-Forwarded-For") != "" || h.Get("Tailscale-Headers-Info") != "" ||
		h.Get(IdentityHeader) != "" || h.Get("X-Forwarded-Host") != ""
}

// localBrowser reports a direct request from this machine (for example a browser
// on http://127.0.0.1:8787). tailscale serve forwards the client's Host header
// unchanged, so a loopback Host alone proves nothing; the proxy headers do.
func localBrowser(r *http.Request) bool {
	return fromLoopback(r) && isLoopbackHost(hostOnly(r.Host)) && !proxied(r)
}

// identityAllowed applies allowed_users. Direct local requests are always
// allowed. The Tailscale identity header is only trusted on loopback connections,
// which is where tailscale serve connects from. An empty list allows local access
// only; "*" allows every tailnet user.
func (s *Server) identityAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		return true
	}
	if localBrowser(r) {
		return true
	}
	s.mu.Lock()
	users := s.users
	s.mu.Unlock()
	if len(users) == 0 || !fromLoopback(r) {
		return false
	}
	login := r.Header.Get(IdentityHeader)
	for _, u := range users {
		if u == "*" || (login != "" && strings.EqualFold(login, u)) {
			return true
		}
	}
	return false
}

func needsOriginCheck(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/tty") || r.Header.Get("Upgrade") != ""
}

func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	// Exact host and port: another app on the same machine name (for example on
	// https port 443 while nostromo is served on 8443) must not get in.
	return strings.EqualFold(u.Host, r.Host)
}

// --- responses ---

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = msg
	writeJSON(w, status, e)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "chat not found")
	case errors.Is(err, store.ErrAmbiguous):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, chat.ErrCwdMissing):
		writeError(w, http.StatusBadRequest, "cwd_missing", err.Error())
	case errors.Is(err, chat.ErrUnknownAgent):
		writeError(w, http.StatusBadRequest, "unknown_agent", err.Error())
	case errors.Is(err, chat.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, "already_running", err.Error())
	case errors.Is(err, chat.ErrNotRunning):
		writeError(w, http.StatusConflict, "not_running", err.Error())
	case errors.Is(err, chat.ErrTmux):
		s.opt.Logger.Printf("tmux error: %v", err)
		writeError(w, http.StatusInternalServerError, "tmux_error", err.Error())
	default:
		s.opt.Logger.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "bad_request", "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- handlers ---

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opt.Version})
}

// Snapshot builds the GET /api/state body.
func (s *Server) Snapshot(ctx context.Context) (state.Snapshot, error) {
	groups, _, _, _, err := s.opt.Chats.List(ctx)
	if err != nil {
		return state.Snapshot{}, err
	}
	s.mu.Lock()
	if s.tsSuffix == "" && s.tsChecked.IsZero() {
		s.mu.Unlock()
		s.refreshTailscale(ctx)
		s.mu.Lock()
	}
	suffix := s.tsSuffix
	s.mu.Unlock()
	peers := make([]state.Peer, 0, len(s.opt.Config.Peers))
	for _, p := range s.opt.Config.Peers {
		u := "https://" + p
		if suffix != "" && !strings.Contains(p, ".") {
			u += "." + suffix
		}
		peers = append(peers, state.Peer{Name: p, URL: u})
	}
	upd := ""
	if s.opt.UpdateAvailable != nil {
		upd = s.opt.UpdateAvailable()
	}
	if groups == nil {
		groups = []state.Group{}
	}
	return state.Snapshot{
		Host: s.opt.Hostname, Version: s.opt.Version, UpdateAvailable: upd,
		Agents: s.opt.Config.AgentNames(), DefaultAgent: s.opt.Config.DefaultAgent,
		Peers: peers, RecentDirs: state.RecentDirs(groups, 10), Groups: groups,
	}, nil
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Snapshot(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	body, etag := state.Encode(snap)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) chatView(ctx context.Context, r store.Record) state.Chat {
	live, err := s.opt.Chats.Live(ctx, r.ID)
	if err != nil {
		live = state.Live{Status: state.Stopped}
	}
	return state.NewChat(r, live)
}

func (s *Server) expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(s.opt.Home, p[1:])
	}
	return filepath.Clean(p)
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
		Cwd   string `json:"cwd"`
		Title string `json:"title"`
	}
	if !decode(w, r, &req) {
		return
	}
	cwd := s.expandPath(strings.TrimSpace(req.Cwd))
	if req.Cwd == "" || !filepath.IsAbs(cwd) {
		writeError(w, http.StatusBadRequest, "bad_request", "cwd must be an absolute path")
		return
	}
	rec, err := s.opt.Chats.Start(r.Context(), chat.StartOptions{Agent: req.Agent, Cwd: cwd, Title: req.Title, Cols: 120, Rows: 40})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.opt.Logger.Printf("chat %s started in %s", rec.ID, rec.Cwd)
	writeJSON(w, http.StatusCreated, s.chatView(r.Context(), rec))
}

func (s *Server) resumeChat(w http.ResponseWriter, r *http.Request) {
	rec, err := s.opt.Chats.Resume(r.Context(), r.PathValue("id"), 120, 40)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.opt.Logger.Printf("chat %s resumed", rec.ID)
	writeJSON(w, http.StatusOK, s.chatView(r.Context(), rec))
}

func (s *Server) stopChat(w http.ResponseWriter, r *http.Request) {
	if err := s.opt.Chats.Stop(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renameChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title *string `json:"title"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Title == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	rec, err := s.opt.Chats.Rename(r.PathValue("id"), *req.Title)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.chatView(r.Context(), rec))
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.opt.Chats.Delete(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.opt.Logger.Printf("chat %s deleted", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Dir != "up" && req.Dir != "down" && req.Dir != "exit" {
		writeError(w, http.StatusBadRequest, "bad_request", `dir must be "up", "down" or "exit"`)
		return
	}
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
	if live.Status == state.Stopped {
		s.fail(w, chat.ErrNotRunning)
		return
	}
	if err := s.opt.Chats.Tmux.Scroll(r.Context(), live.Session, req.Dir); err != nil {
		s.fail(w, errors.Join(chat.ErrTmux, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DirList is the GET /api/dirs response.
type DirList struct {
	Path   string   `json:"path"`
	Parent string   `json:"parent"`
	Dirs   []string `json:"dirs"`
}

func (s *Server) dirs(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = s.opt.Home
	}
	p = s.expandPath(p)
	if !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "bad_request", "path must be absolute")
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "cannot read directory: "+p)
		return
	}
	all := r.URL.Query().Get("all") == "1"
	dirs := []string{}
	for _, e := range entries {
		name := e.Name()
		if !all && strings.HasPrefix(name, ".") {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(p, name)); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if isDir {
			dirs = append(dirs, name)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	writeJSON(w, http.StatusOK, DirList{Path: p, Parent: filepath.Dir(p), Dirs: dirs})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	if s.opt.StartUpdate == nil {
		writeError(w, http.StatusNotFound, "not_found", "updates are not available")
		return
	}
	if err := s.opt.StartUpdate(); err != nil {
		s.fail(w, err)
		return
	}
	s.opt.Logger.Printf("update started from the UI")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// SetAllowedUsers replaces allowed_users in the running server.
func (s *Server) SetAllowedUsers(users []string) {
	s.mu.Lock()
	s.users = append([]string(nil), users...)
	s.mu.Unlock()
}

// Shutdown closes every terminal connection and waits for them to finish.
func (s *Server) Shutdown() {
	s.shutdown()
	s.ttyWG.Wait()
}
