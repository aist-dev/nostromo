//go:build integration

package tmux_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/adapter"
	"github.com/aist-dev/nostromo/internal/testutil"
	"github.com/aist-dev/nostromo/internal/tmux"
)

func TestRealTmuxSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	c := testutil.Tmux(t, home)
	agent := testutil.FakeAgent(t)

	if _, err := tmux.CheckVersion(ctx, c); err != nil {
		t.Fatal(err)
	}
	panes, err := c.ListPanes(ctx)
	if err != nil || len(panes) != 0 {
		t.Fatalf("no server yet: %v %v", panes, err)
	}

	spec := tmux.SessionSpec{
		Name: "agent-test0001", ChatID: "test0001-full", Cwd: home,
		Command: adapter.ShellQuote(agent) + " --flag",
		Env:     []string{"NOSTROMO_HOME=" + home, "NOSTROMO_CHAT_ID=test0001-full"},
	}
	if err := c.NewSession(ctx, spec); err != nil {
		t.Fatal(err)
	}
	panes, err = c.ListPanes(ctx)
	if err != nil || len(panes) != 1 || panes[0].ChatID != "test0001-full" || panes[0].Dead || panes[0].PID == 0 {
		t.Fatalf("panes: %+v %v", panes, err)
	}
	testutil.Eventually(t, 5*time.Second, "fake agent start", func() bool { return len(testutil.FakeRuns(home)) == 1 })
	run := testutil.FakeRuns(home)[0]
	if run.Env["NOSTROMO_CHAT_ID"] != "test0001-full" || strings.Join(run.Args, " ") != "--flag" {
		t.Fatalf("run: %+v", run)
	}

	if err := c.Scroll(ctx, spec.Name, "up"); err != nil {
		t.Fatal(err)
	}
	if err := c.Scroll(ctx, spec.Name, "exit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Scroll(ctx, spec.Name, "exit"); err != nil {
		t.Fatalf("exit twice: %v", err)
	}

	// A failing command keeps its pane (remain-on-exit failed).
	failing := tmux.SessionSpec{Name: "agent-test0002", ChatID: "test0002-full", Cwd: home, Command: "exit 3"}
	if err := c.NewSession(ctx, failing); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "dead pane", func() bool {
		ps, _ := c.ListPanes(ctx)
		for _, p := range ps {
			if p.ChatID == "test0002-full" && p.Dead && p.DeadStatus == 3 {
				return true
			}
		}
		return false
	})

	if err := c.KillSession(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := c.KillSession(ctx, spec.Name); !errors.Is(err, tmux.ErrNoSession) {
		t.Fatalf("second kill: %v", err)
	}
	if err := c.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.KillServer(ctx); err != nil {
		t.Fatalf("kill without server: %v", err)
	}
}
