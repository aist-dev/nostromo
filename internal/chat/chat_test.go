package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/tmux/faketmux"
)

const testID = "11111111-2222-4333-8444-555555555555"

type env struct {
	svc   *Service
	fake  *faketmux.Fake
	clock time.Time
	cwd   string
}

func setup(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	paths := config.Paths{Home: home}
	fake := faketmux.New()
	e := &env{fake: fake, clock: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC), cwd: t.TempDir()}
	e.svc = New(store.New(paths.Chats()), fake, config.Default(), paths)
	e.svc.Shell = "/bin/bash"
	e.svc.Now = func() time.Time { return e.clock }
	e.svc.NewID = func() string { return testID }
	return e
}

func TestStart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Title: "  fix\n the   bug ", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != testID || r.ResumeID != testID || r.Agent != "claude" || r.TmuxSession != "agent-11111111" || r.Title != "fix the bug" {
		t.Fatalf("record: %+v", r)
	}
	if strings.Join(e.fake.Calls, ",") != "Version,NewSession" {
		t.Fatalf("call order: %v", e.fake.Calls)
	}
	s, _ := e.fake.Get("agent-11111111")
	spec := s.Spec
	if spec.Cwd != e.cwd || spec.Cols != 100 || spec.Rows != 30 || spec.ChatID != testID {
		t.Fatalf("spec: %+v", spec)
	}
	if !strings.Contains(spec.Command, "claude --dangerously-skip-permissions --session-id "+testID) ||
		!strings.HasPrefix(spec.Command, "exec /bin/bash -lic ") {
		t.Fatalf("command: %s", spec.Command)
	}
	if strings.Join(spec.Env, " ") != "NOSTROMO_CHAT_ID="+testID+" NOSTROMO_HOME="+e.svc.Paths.Home {
		t.Fatalf("env: %v", spec.Env)
	}
	if _, err := os.Stat(e.svc.Paths.TmuxConf()); err != nil {
		t.Fatalf("tmux.conf not written: %v", err)
	}
	if got, err := e.svc.Store.Get(testID); err != nil || !got.CreatedAt.Equal(e.clock) {
		t.Fatalf("stored: %+v %v", got, err)
	}
	live, _ := e.svc.Live(ctx, testID)
	if live.Status != state.Running {
		t.Fatalf("status %s", live.Status)
	}
}

func TestStartErrors(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: filepath.Join(e.cwd, "missing")}); !errors.Is(err, ErrCwdMissing) {
		t.Fatalf("cwd: %v", err)
	}
	file := filepath.Join(e.cwd, "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: file}); !errors.Is(err, ErrCwdMissing) {
		t.Fatalf("cwd is a file: %v", err)
	}
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Agent: "ghost"}); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("agent: %v", err)
	}
	e.fake.Ver = "tmux 3.1"
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd}); !errors.Is(err, ErrTmux) {
		t.Fatalf("old tmux: %v", err)
	}
	e.fake.Ver = "tmux 3.4"

	// tmux failing to create the session rolls the record back.
	e.fake.Errs["NewSession"] = errors.New("boom")
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd}); !errors.Is(err, ErrTmux) {
		t.Fatalf("new-session: %v", err)
	}
	if recs, _, _ := e.svc.Store.List(); len(recs) != 0 {
		t.Fatalf("record not rolled back: %+v", recs)
	}
	delete(e.fake.Errs, "NewSession")

	// A bad template fails without leaving a record.
	e.svc.Config.Agents["bad"] = config.Agent{Start: "x {nope}", Resume: "x"}
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Agent: "bad"}); err == nil {
		t.Fatal("bad template")
	}
	// The store refusing to create (duplicate id) is reported.
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd}); err == nil {
		t.Fatal("duplicate id must fail")
	}
}

func TestEnsureTmuxConfError(t *testing.T) {
	e := setup(t)
	os.MkdirAll(e.svc.Paths.Home, 0o755)
	// tmux.conf path occupied by a non-empty directory cannot be written.
	os.MkdirAll(filepath.Join(e.svc.Paths.TmuxConf()), 0o755)
	if err := e.svc.EnsureTmuxConf(); err != nil {
		t.Fatalf("existing path counts as present: %v", err)
	}
	e.svc.Paths.Home = filepath.Join(e.cwd, "file")
	os.WriteFile(e.svc.Paths.Home, nil, 0o644)
	if _, err := e.svc.Start(context.Background(), StartOptions{Cwd: e.cwd}); err == nil {
		t.Fatal("unwritable home must fail")
	}
}

func TestResume(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.svc.Start(ctx, StartOptions{Cwd: e.cwd})

	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("running: %v", err)
	}

	// Stopped chat resumes with the resume template and current resume_id.
	e.fake.Kill("agent-11111111", 0)
	e.svc.Store.Update(testID, func(r *store.Record) error { r.ResumeID = "99999999-0000-4000-8000-000000000000"; return nil })
	e.clock = e.clock.Add(time.Hour)
	r, err := e.svc.Resume(ctx, testID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if !r.LastStartedAt.Equal(e.clock) {
		t.Fatalf("last_started_at not updated: %v", r.LastStartedAt)
	}
	s, _ := e.fake.Get("agent-11111111")
	if !strings.Contains(s.Spec.Command, "--resume 99999999-0000-4000-8000-000000000000") || s.Spec.Cols != 80 {
		t.Fatalf("resume spec: %+v", s.Spec)
	}

	// Failed chat is killed first, then resumed.
	e.fake.Kill("agent-11111111", 3)
	e.fake.Calls = nil
	if _, err := e.svc.Resume(ctx, testID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.fake.Calls, ",") != "ListPanes,KillSession,NewSession" {
		t.Fatalf("failed resume calls: %v", e.fake.Calls)
	}
}

func TestResumeErrors(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	e.svc.Start(ctx, StartOptions{Cwd: e.cwd})
	e.fake.Kill("agent-11111111", 0)

	e.fake.Errs["ListPanes"] = errors.New("x")
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrTmux) {
		t.Fatalf("list: %v", err)
	}
	delete(e.fake.Errs, "ListPanes")

	// Missing cwd leaves the record untouched.
	before, _ := e.svc.Store.Get(testID)
	os.RemoveAll(e.cwd)
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrCwdMissing) {
		t.Fatalf("cwd: %v", err)
	}
	after, _ := e.svc.Store.Get(testID)
	if !after.LastStartedAt.Equal(before.LastStartedAt) {
		t.Fatal("record changed")
	}
	os.MkdirAll(e.cwd, 0o755)

	e.svc.Store.Update(testID, func(r *store.Record) error { r.Agent = "ghost"; r.TmuxSession = ""; return nil })
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("agent: %v", err)
	}
	e.svc.Store.Update(testID, func(r *store.Record) error { r.Agent = "claude"; return nil })

	e.fake.Errs["NewSession"] = errors.New("x")
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrTmux) {
		t.Fatalf("new: %v", err)
	}
	delete(e.fake.Errs, "NewSession")
	if r, err := e.svc.Resume(ctx, testID, 0, 0); err != nil || r.TmuxSession != "agent-11111111" {
		t.Fatalf("empty session name restored: %+v %v", r, err)
	}

	e.fake.Kill("agent-11111111", 2)
	e.fake.Errs["KillSession"] = errors.New("x")
	if _, err := e.svc.Resume(ctx, testID, 0, 0); !errors.Is(err, ErrTmux) {
		t.Fatalf("kill failed: %v", err)
	}
}

func TestStopDeleteRename(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if err := e.svc.Stop(ctx, testID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stop missing: %v", err)
	}
	if err := e.svc.Delete(ctx, testID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
	e.svc.Start(ctx, StartOptions{Cwd: e.cwd})

	e.fake.Errs["ListPanes"] = errors.New("x")
	if err := e.svc.Stop(ctx, testID); !errors.Is(err, ErrTmux) {
		t.Fatalf("stop list: %v", err)
	}
	if err := e.svc.Delete(ctx, testID); !errors.Is(err, ErrTmux) {
		t.Fatalf("delete list: %v", err)
	}
	if _, _, _, _, err := e.svc.List(ctx); !errors.Is(err, ErrTmux) {
		t.Fatalf("list: %v", err)
	}
	delete(e.fake.Errs, "ListPanes")

	e.fake.Errs["KillSession"] = errors.New("x")
	if err := e.svc.Stop(ctx, testID); !errors.Is(err, ErrTmux) {
		t.Fatalf("stop kill: %v", err)
	}
	if err := e.svc.Delete(ctx, testID); !errors.Is(err, ErrTmux) {
		t.Fatalf("delete kill: %v", err)
	}
	e.fake.Errs["KillSession"] = tmux.ErrNoSession
	if err := e.svc.Stop(ctx, testID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop race: %v", err)
	}
	delete(e.fake.Errs, "KillSession")

	if err := e.svc.Stop(ctx, testID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Stop(ctx, testID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stop twice: %v", err)
	}
	if _, err := e.svc.Store.Get(testID); err != nil {
		t.Fatal("stop must keep the record")
	}

	r, err := e.svc.Rename(testID, strings.Repeat("я", 300))
	if err != nil || len([]rune(r.Title)) != MaxTitle {
		t.Fatalf("rename: %v %d", err, len([]rune(r.Title)))
	}

	groups, recs, panes, bad, err := e.svc.List(ctx)
	if err != nil || len(groups) != 1 || len(recs) != 1 || len(panes) != 0 || len(bad) != 0 {
		t.Fatalf("list: %v %v %v %v %v", groups, recs, panes, bad, err)
	}

	e.svc.Resume(ctx, testID, 0, 0)
	if err := e.svc.Delete(ctx, testID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.Get("agent-11111111"); ok {
		t.Fatal("delete must kill the session")
	}
	if _, err := e.svc.Store.Get(testID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("delete must remove the record")
	}

	e.svc.Store.Dir = filepath.Join(e.cwd, "f")
	os.WriteFile(e.svc.Store.Dir, nil, 0o644)
	if _, _, _, _, err := e.svc.List(ctx); err == nil {
		t.Fatal("store error must propagate")
	}
}

func TestDefaults(t *testing.T) {
	s := New(nil, nil, config.Default(), config.Paths{})
	if s.now().IsZero() || len(s.newID()) != 36 {
		t.Fatal("defaults")
	}
}

func TestStartArgsChecksAndCleanup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Args: []string{"--resume", "x"}}); !errors.Is(err, ErrDeniedArg) {
		t.Fatalf("denied argument: %v", err)
	}
	if len(e.fake.Calls) != 0 {
		t.Fatalf("a refused start must not touch tmux: %v", e.fake.Calls)
	}
	runDir := filepath.Join(e.svc.Paths.Home, "run", testID)
	e.fake.Errs["NewSession"] = errors.New("boom")
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Args: []string{"hi"}}); err == nil {
		t.Fatal("tmux failure must fail the start")
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("argument files must be removed after a failed start: %v", err)
	}
	delete(e.fake.Errs, "NewSession")
	os.MkdirAll(e.svc.Paths.Home, 0o700)
	os.RemoveAll(filepath.Join(e.svc.Paths.Home, "run"))
	os.WriteFile(filepath.Join(e.svc.Paths.Home, "run"), nil, 0o600) // a file blocks the directory
	if _, err := e.svc.Start(ctx, StartOptions{Cwd: e.cwd, Args: []string{"hi"}}); err == nil || !strings.Contains(err.Error(), "arguments") {
		t.Fatalf("unwritable argument dir: %v", err)
	}
}
