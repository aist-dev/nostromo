//go:build integration

package chat_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/adapter"
	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/testutil"
	"github.com/aist-dev/nostromo/internal/tmux"
)

type rig struct {
	svc  *chat.Service
	tm   *tmux.Client
	home string
}

func newRig(t *testing.T, shellWrap bool) *rig {
	t.Helper()
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	fake := adapter.ShellQuote(testutil.FakeAgent(t))
	cfg := config.Default()
	wrap := shellWrap
	cfg.Agents["fake"] = config.Agent{Start: fake + " --session-id {id}", Resume: fake + " --resume {resume_id}", ShellWrap: &wrap}
	cfg.Agents["missing"] = config.Agent{Start: "/nonexistent/agent-binary {id}", Resume: "/nonexistent/agent-binary {resume_id}", ShellWrap: &wrap}
	cfg.DefaultAgent = "fake"
	paths := config.Paths{Home: home}
	svc := chat.New(store.New(paths.Chats()), tm, cfg, paths)
	svc.Shell = "/bin/sh"
	return &rig{svc: svc, tm: tm, home: home}
}

func (r *rig) status(t *testing.T, id string) state.Live {
	t.Helper()
	l, err := r.svc.Live(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (r *rig) waitStatus(t *testing.T, id string, want state.Status) state.Live {
	t.Helper()
	var l state.Live
	testutil.Eventually(t, 5*time.Second, "status "+string(want), func() bool {
		l = r.status(t, id)
		return l.Status == want
	})
	return l
}

func (r *rig) waitRuns(t *testing.T, n int) []testutil.FakeRun {
	t.Helper()
	testutil.Eventually(t, 5*time.Second, "fake agent start", func() bool { return len(testutil.FakeRuns(r.home)) >= n })
	return testutil.FakeRuns(r.home)
}

func (r *rig) waitScreen(t *testing.T, session, text string) {
	t.Helper()
	testutil.Eventually(t, 5*time.Second, "screen text "+text, func() bool {
		return strings.Contains(testutil.CapturePane(r.tm, session), text)
	})
}

// Integration test 1 (new chat) and 16 (path with spaces and non-ASCII), with and without the login-shell wrapper.
func TestNewChat(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		r := newRig(t, wrap)
		cwd := filepath.Join(t.TempDir(), "my dir ü")
		os.MkdirAll(cwd, 0o755)
		rec, err := r.svc.Start(context.Background(), chat.StartOptions{Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if r.status(t, rec.ID).Status != state.Running {
			t.Fatal("not running")
		}
		run := r.waitRuns(t, 1)[0]
		if strings.Join(run.Args, " ") != "--session-id "+rec.ID {
			t.Fatalf("args: %v", run.Args)
		}
		if run.Env["NOSTROMO_CHAT_ID"] != rec.ID || run.Env["NOSTROMO_HOME"] != r.home {
			t.Fatalf("env: %v", run.Env)
		}
		real, _ := filepath.EvalSymlinks(cwd)
		got, _ := filepath.EvalSymlinks(run.Cwd)
		if got != real {
			t.Fatalf("cwd: %q want %q", run.Cwd, cwd)
		}
		r.waitScreen(t, rec.TmuxSession, "fakeagent ready>")
	}
}

// Integration tests 2 and 3: exit 0 stops, exit 3 fails and keeps the screen.
func TestAgentExit(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	a, _ := r.svc.Start(ctx, chat.StartOptions{Cwd: t.TempDir()})
	r.waitScreen(t, a.TmuxSession, "ready>")
	testutil.SendKeys(r.tm, a.TmuxSession, "exit 0")
	r.waitStatus(t, a.ID, state.Stopped)
	if _, err := r.svc.Store.Get(a.ID); err != nil {
		t.Fatal("record must remain after a clean exit")
	}

	r.svc.NewID = func() string { return "22222222-0000-4000-8000-000000000000" }
	b, _ := r.svc.Start(ctx, chat.StartOptions{Cwd: t.TempDir()})
	r.waitScreen(t, b.TmuxSession, "ready>")
	testutil.SendKeys(r.tm, b.TmuxSession, "exit 3")
	l := r.waitStatus(t, b.ID, state.Failed)
	if l.ExitCode == nil || *l.ExitCode != 3 {
		t.Fatalf("exit code: %+v", l)
	}
	r.waitScreen(t, b.TmuxSession, "bye 3")
}

// Integration test 4: a missing agent command fails visibly.
func TestMissingAgentCommand(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		r := newRig(t, wrap)
		rec, err := r.svc.Start(context.Background(), chat.StartOptions{Cwd: t.TempDir(), Agent: "missing"})
		if err != nil {
			t.Fatal(err)
		}
		r.waitStatus(t, rec.ID, state.Failed)
		screen := testutil.CapturePane(r.tm, rec.TmuxSession)
		if !strings.Contains(screen, "agent-binary") {
			t.Fatalf("error text not visible: %q", screen)
		}
	}
}

// Integration tests 5 and 7: resume after the agent stops and after the tmux server dies.
func TestResumeAndReboot(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	rec, _ := r.svc.Start(ctx, chat.StartOptions{Cwd: t.TempDir()})
	r.waitRuns(t, 1)

	if err := r.tm.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	if r.status(t, rec.ID).Status != state.Stopped {
		t.Fatal("after a reboot every chat is stopped")
	}
	newResume := "33333333-0000-4000-8000-000000000000"
	r.svc.Store.Update(rec.ID, func(x *store.Record) error { x.ResumeID = newResume; return nil })
	time.Sleep(10 * time.Millisecond)
	resumed, err := r.svc.Resume(ctx, rec.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.LastStartedAt.After(rec.LastStartedAt) {
		t.Fatal("last_started_at not updated")
	}
	runs := r.waitRuns(t, 2)
	if strings.Join(runs[1].Args, " ") != "--resume "+newResume {
		t.Fatalf("resume args: %v", runs[1].Args)
	}
	r.waitStatus(t, rec.ID, state.Running)

	if err := r.svc.Stop(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	r.waitStatus(t, rec.ID, state.Stopped)
	if err := r.svc.Delete(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
}
