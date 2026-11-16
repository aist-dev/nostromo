// Package chat starts, resumes, stops and deletes chats. The launcher and the
// server both use it, so chats from the terminal and from the browser are identical.
package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aist-dev/nostromo/internal/adapter"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
)

var (
	ErrUnknownAgent   = errors.New("unknown agent")
	ErrCwdMissing     = errors.New("directory does not exist")
	ErrAlreadyRunning = errors.New("chat is already running")
	ErrNotRunning     = errors.New("chat is not running")
	ErrTmux           = errors.New("tmux failed")
	ErrDeniedArg      = errors.New("denied argument")
)

// Environment variables passed to every agent.
const (
	EnvChatID = "NOSTROMO_CHAT_ID"
	EnvHome   = "NOSTROMO_HOME"
)

// Service runs chat operations.
type Service struct {
	Store  *store.Store
	Tmux   tmux.API
	Config config.Config
	Paths  config.Paths
	Shell  string
	Now    func() time.Time
	NewID  func() string
}

// New returns a service with real clocks and ids.
func New(st *store.Store, t tmux.API, cfg config.Config, paths config.Paths) *Service {
	return &Service{Store: st, Tmux: t, Config: cfg, Paths: paths, Shell: adapter.UserShell()}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return uuid.NewString()
}

// StartOptions describes a new chat.
type StartOptions struct {
	Agent string
	Cwd   string
	Title string
	// Args are passed through to the agent's start command (not to resume).
	Args []string
	Cols int
	Rows int
}

func checkCwd(cwd string) error {
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", ErrCwdMissing, cwd)
	}
	return nil
}

func (s *Service) agent(name string) (config.Agent, string, error) {
	if name == "" {
		name = s.Config.DefaultAgent
	}
	a, ok := s.Config.Agents[name]
	if !ok {
		return a, name, fmt.Errorf("%w %q (configured: %s)", ErrUnknownAgent, name, strings.Join(s.Config.AgentNames(), ", "))
	}
	return a, name, nil
}

// EnsureTmuxConf writes the nostromo tmux config if it is missing.
func (s *Service) EnsureTmuxConf() error {
	p := s.Paths.TmuxConf()
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return config.WriteFileAtomic(p, []byte(tmux.Conf), 0o644)
}

func (s *Service) launch(ctx context.Context, r store.Record, a config.Agent, resume bool, args []string, cols, rows int) error {
	argsDir := filepath.Join(s.Paths.Home, "run", r.ID)
	cmd, err := adapter.Command(a, resume, adapter.Values{ID: r.ID, ResumeID: r.ResumeID, Cwd: r.Cwd, Args: args}, s.Shell, argsDir)
	if err != nil {
		return err
	}
	if len(args) > 0 && !resume {
		if err := adapter.WriteArgs(argsDir, args); err != nil {
			os.RemoveAll(argsDir)
			return fmt.Errorf("store the agent arguments: %w", err)
		}
	}
	if err := s.EnsureTmuxConf(); err != nil {
		return err
	}
	spec := tmux.SessionSpec{
		Name: r.TmuxSession, ChatID: r.ID, Cwd: r.Cwd, Command: cmd, Cols: cols, Rows: rows,
		Env: []string{EnvChatID + "=" + r.ID, EnvHome + "=" + s.Paths.Home},
	}
	if err := s.Tmux.NewSession(ctx, spec); err != nil {
		os.RemoveAll(argsDir)
		return fmt.Errorf("%w: %v", ErrTmux, err)
	}
	return nil
}

// Start creates a record and a tmux session for a new chat.
func (s *Service) Start(ctx context.Context, o StartOptions) (store.Record, error) {
	if err := checkCwd(o.Cwd); err != nil {
		return store.Record{}, err
	}
	a, name, err := s.agent(o.Agent)
	if err != nil {
		return store.Record{}, err
	}
	if err := a.CheckArgs(o.Args); err != nil {
		return store.Record{}, fmt.Errorf("%w: agent %s: %v", ErrDeniedArg, name, err)
	}
	if _, err := tmux.CheckVersion(ctx, s.Tmux); err != nil {
		return store.Record{}, fmt.Errorf("%w: %v", ErrTmux, err)
	}
	id := s.newID()
	now := s.now()
	r := store.Record{
		V: store.SchemaVersion, ID: id, Agent: name, Cwd: o.Cwd, Title: CleanTitle(o.Title),
		CreatedAt: now, LastStartedAt: now, ResumeID: id, TmuxSession: store.SessionName(id),
	}
	if err := s.Store.Create(r); err != nil {
		return store.Record{}, err
	}
	if err := s.launch(ctx, r, a, false, o.Args, o.Cols, o.Rows); err != nil {
		s.Store.Delete(id)
		return store.Record{}, err
	}
	return r, nil
}

// Live returns the tmux state of one chat.
func (s *Service) Live(ctx context.Context, id string) (state.Live, error) {
	panes, err := s.Tmux.ListPanes(ctx)
	if err != nil {
		return state.Live{}, fmt.Errorf("%w: %v", ErrTmux, err)
	}
	return state.Of(id, state.Index(panes)), nil
}

// Resume restarts a stopped or failed chat. For a running chat it returns the
// record together with ErrAlreadyRunning, so callers can simply attach.
func (s *Service) Resume(ctx context.Context, id string, cols, rows int) (store.Record, error) {
	r, err := s.Store.Get(id)
	if err != nil {
		return r, err
	}
	live, err := s.Live(ctx, id)
	if err != nil {
		return r, err
	}
	switch live.Status {
	case state.Running:
		return r, ErrAlreadyRunning
	case state.Failed:
		if err := s.Tmux.KillSession(ctx, live.Session); err != nil && !errors.Is(err, tmux.ErrNoSession) {
			return r, fmt.Errorf("%w: %v", ErrTmux, err)
		}
	}
	if err := checkCwd(r.Cwd); err != nil {
		return r, err
	}
	a, _, err := s.agent(r.Agent)
	if err != nil {
		return r, err
	}
	if r.TmuxSession == "" {
		r.TmuxSession = store.SessionName(r.ID)
	}
	if err := s.launch(ctx, r, a, true, nil, cols, rows); err != nil {
		return r, err
	}
	return s.Store.Update(id, func(rec *store.Record) error {
		rec.LastStartedAt = s.now()
		rec.TmuxSession = r.TmuxSession
		return nil
	})
}

// Stop ends the chat's tmux session and keeps the record.
func (s *Service) Stop(ctx context.Context, id string) error {
	if _, err := s.Store.Get(id); err != nil {
		return err
	}
	live, err := s.Live(ctx, id)
	if err != nil {
		return err
	}
	if live.Status == state.Stopped {
		return ErrNotRunning
	}
	if err := s.Tmux.KillSession(ctx, live.Session); err != nil {
		if errors.Is(err, tmux.ErrNoSession) {
			return ErrNotRunning
		}
		return fmt.Errorf("%w: %v", ErrTmux, err)
	}
	return nil
}

// Delete ends the session if there is one and removes the record.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.Store.Get(id); err != nil {
		return err
	}
	live, err := s.Live(ctx, id)
	if err != nil {
		return err
	}
	if live.Status != state.Stopped {
		if err := s.Tmux.KillSession(ctx, live.Session); err != nil && !errors.Is(err, tmux.ErrNoSession) {
			return fmt.Errorf("%w: %v", ErrTmux, err)
		}
	}
	return s.Store.Delete(id)
}

// Rename sets a chat title.
func (s *Service) Rename(id, title string) (store.Record, error) {
	return s.Store.Update(id, func(r *store.Record) error {
		r.Title = CleanTitle(title)
		return nil
	})
}

// MaxTitle is the longest stored title, in runes.
const MaxTitle = 200

// CleanTitle collapses whitespace into single spaces and limits the length.
func CleanTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > MaxTitle {
		t = string(r[:MaxTitle])
	}
	return t
}

// List returns all chats grouped for display, plus records that could not be read.
func (s *Service) List(ctx context.Context) ([]state.Group, []store.Record, []tmux.Pane, []store.BadFile, error) {
	recs, bad, err := s.Store.List()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	panes, err := s.Tmux.ListPanes(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%w: %v", ErrTmux, err)
	}
	return state.Build(recs, panes), recs, panes, bad, nil
}
