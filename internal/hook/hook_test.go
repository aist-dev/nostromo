package hook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/store"
)

const id = "11111111-2222-4333-8444-555555555555"

func setup(t *testing.T) (*store.Store, cli.Env, *bytes.Buffer, string) {
	t.Helper()
	home := t.TempDir()
	st := store.New(filepath.Join(home, "chats"))
	st.Create(store.Record{ID: id, Agent: "claude", Cwd: "/", ResumeID: id, CreatedAt: time.Now()})
	out := &bytes.Buffer{}
	vars := map[string]string{"NOSTROMO_HOME": home, "NOSTROMO_CHAT_ID": id}
	env := cli.Env{Stdout: out, Stderr: out, Getenv: func(k string) string { return vars[k] }}
	return st, env, out, home
}

func run(env cli.Env, input string, args ...string) int {
	env.Stdin = strings.NewReader(input)
	return Command(context.Background(), args, env)
}

func TestSessionStartSources(t *testing.T) {
	for source, updates := range map[string]bool{"startup": true, "resume": true, "clear": true, "compact": true, "fork": false} {
		st, env, out, _ := setup(t)
		in := `{"hook_event_name":"SessionStart","session_id":"99999999-0000-4000-8000-000000000000","source":"` + source + `"}`
		if code := run(env, in, "claude"); code != 0 || out.Len() != 0 {
			t.Fatalf("%s: code %d, output %q", source, code, out)
		}
		r, _ := st.Get(id)
		if got := r.ResumeID != id; got != updates {
			t.Errorf("%s: resume_id updated = %v, want %v", source, got, updates)
		}
	}
}

func TestUserPromptSubmitSetsTitleOnce(t *testing.T) {
	st, env, out, _ := setup(t)
	long := "Please fix\nthe   payment retry logic in the checkout service and add tests for it"
	run(env, `{"hook_event_name":"UserPromptSubmit","prompt":`+quote(long)+`}`, "claude")
	r, _ := st.Get(id)
	if r.Title != "Please fix the payment retry logic in the checkout service a" || len([]rune(r.Title)) > TitleLen {
		t.Fatalf("title %q", r.Title)
	}
	run(env, `{"hook_event_name":"UserPromptSubmit","prompt":"second"}`, "claude")
	r, _ = st.Get(id)
	if !strings.HasPrefix(r.Title, "Please fix") {
		t.Fatal("title must be set only once")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty: %q", out)
	}
	st.Update(id, func(r *store.Record) error { r.Title = ""; return nil })
	run(env, `{"hook_event_name":"UserPromptSubmit","prompt":"   "}`, "claude")
	if r, _ := st.Get(id); r.Title != "" {
		t.Fatal("blank prompt must not set a title")
	}
}

func quote(s string) string {
	b := strings.ReplaceAll(s, "\n", `\n`)
	return `"` + b + `"`
}

func TestNoChatIDDoesNothing(t *testing.T) {
	st, env, out, home := setup(t)
	env.Getenv = func(k string) string {
		if k == "NOSTROMO_HOME" {
			return home
		}
		return ""
	}
	if code := run(env, `{"hook_event_name":"SessionStart","session_id":"x","source":"clear"}`, "claude"); code != 0 || out.Len() != 0 {
		t.Fatal("must be a no-op")
	}
	if r, _ := st.Get(id); r.ResumeID != id {
		t.Fatal("record changed")
	}
	if _, err := os.Stat(filepath.Join(home, "logs")); err == nil {
		t.Fatal("nothing should be logged")
	}
}

func TestErrorsAreLoggedNotReturned(t *testing.T) {
	_, env, out, home := setup(t)
	cases := []struct {
		args  []string
		input string
		log   string
	}{
		{[]string{"claude"}, "{broken", "invalid hook input"},
		{[]string{"codex"}, "{}", "unknown hook adapter"},
		{nil, "{}", "unknown hook adapter"},
	}
	for _, c := range cases {
		if code := run(env, c.input, c.args...); code != 0 {
			t.Fatalf("%v: code %d", c.args, code)
		}
		data, _ := os.ReadFile(filepath.Join(home, "logs", "hook.log"))
		if !strings.Contains(string(data), c.log) {
			t.Fatalf("%v: log lacks %q:\n%s", c.args, c.log, data)
		}
	}
	vars := map[string]string{"NOSTROMO_HOME": home, "NOSTROMO_CHAT_ID": "aaaaaaaa-0000-4000-8000-000000000000"}
	env.Getenv = func(k string) string { return vars[k] }
	run(env, `{"hook_event_name":"SessionStart","session_id":"x","source":"clear"}`, "claude")
	data, _ := os.ReadFile(filepath.Join(home, "logs", "hook.log"))
	if !strings.Contains(string(data), "no record") {
		t.Fatalf("missing record not logged:\n%s", data)
	}
	if out.Len() != 0 {
		t.Fatal("stdout must stay empty")
	}
	// Other events and missing session ids are ignored.
	run(env, `{"hook_event_name":"Stop"}`, "claude")
	run(env, `{"hook_event_name":"SessionStart","source":"clear"}`, "claude")
}

func TestLockTimeoutAndUnwritableLog(t *testing.T) {
	st, env, _, home := setup(t)
	unlock, _ := st.Lock()
	start := time.Now()
	if code := run(env, `{"hook_event_name":"SessionStart","session_id":"x","source":"clear"}`, "claude"); code != 0 {
		t.Fatal("code")
	}
	unlock()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("hook waited %v for the lock", d)
	}
	data, _ := os.ReadFile(filepath.Join(home, "logs", "hook.log"))
	if !strings.Contains(string(data), "timed out") {
		t.Fatalf("lock timeout not logged:\n%s", data)
	}
	os.RemoveAll(filepath.Join(home, "logs"))
	os.WriteFile(filepath.Join(home, "logs"), nil, 0o644)
	if code := run(env, "{broken", "claude"); code != 0 {
		t.Fatal("unwritable log must not fail the hook")
	}
	env.Getenv = func(k string) string {
		if k == "NOSTROMO_CHAT_ID" {
			return id
		}
		return ""
	}
	if code := run(env, "{}", "claude"); code != 0 {
		t.Fatal("no home must not fail the hook")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func TestReadError(t *testing.T) {
	st, _, _, _ := setup(t)
	if err := Claude(st, id, errReader{}); err == nil {
		t.Fatal("read error must be returned")
	}
}
