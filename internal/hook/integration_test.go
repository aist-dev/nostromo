//go:build integration

package hook_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/adapter"
	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/testutil"
)

// Integration test 14: the hook changes resume_id and the next Resume uses it.
func TestHookChangesResumeID(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	fake := adapter.ShellQuote(testutil.FakeAgent(t))
	cfg := config.Default()
	off := false
	cfg.Agents["fake"] = config.Agent{Start: fake + " --session-id {id}", Resume: fake + " --resume {resume_id}", ShellWrap: &off}
	cfg.DefaultAgent = "fake"
	paths := config.Paths{Home: home}
	svc := chat.New(store.New(paths.Chats()), tm, cfg, paths)
	rec, err := svc.Start(ctx, chat.StartOptions{Cwd: home})
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "agent start", func() bool { return len(testutil.FakeRuns(home)) == 1 })

	newID := "77777777-0000-4000-8000-000000000000"
	cmd := exec.Command(testutil.AgentBinary(t), "hook", "claude")
	cmd.Env = append(os.Environ(), "NOSTROMO_HOME="+home, "NOSTROMO_CHAT_ID="+rec.ID)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"` + newID + `","source":"clear"}`)
	out, err := cmd.Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("hook: %v, stdout %q", err, out)
	}
	if got, _ := svc.Store.Get(rec.ID); got.ResumeID != newID {
		t.Fatalf("resume_id = %s", got.ResumeID)
	}

	if err := svc.Stop(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resume(ctx, rec.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 5*time.Second, "agent resume", func() bool { return len(testutil.FakeRuns(home)) == 2 })
	if args := strings.Join(testutil.FakeRuns(home)[1].Args, " "); args != "--resume "+newID {
		t.Fatalf("resume args: %s", args)
	}
}
