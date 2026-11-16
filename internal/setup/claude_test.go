package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bin = "/home/me/.nostromo/bin/agent"

func settingsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if content != "" {
		os.WriteFile(p, []byte(content), 0o644)
	}
	return p
}

func countOurs(t *testing.T, p string) map[string]int {
	t.Helper()
	data, _ := os.ReadFile(p)
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	n := map[string]int{}
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if strings.HasSuffix(h.Command, "/.nostromo/bin/agent hook claude") {
					n[ev]++
				}
			}
		}
	}
	return n
}

func TestInstallIntoMissingAndEmptyFile(t *testing.T) {
	for _, content := range []string{"", "{}", "  \n"} {
		p := settingsFile(t, content)
		changed, err := InstallClaudeHooks(p, bin)
		if err != nil || !changed {
			t.Fatalf("%q: %v %v", content, changed, err)
		}
		n := countOurs(t, p)
		if n["SessionStart"] != 1 || n["UserPromptSubmit"] != 1 {
			t.Fatalf("%q: %v", content, n)
		}
		if ok, _ := ClaudeHooksInstalled(p); !ok {
			t.Fatal("installed check")
		}
		data, _ := os.ReadFile(p)
		if !strings.Contains(string(data), `"command": "/home/me/.nostromo/bin/agent hook claude"`) || !strings.Contains(string(data), `"timeout": 5`) {
			t.Fatalf("content: %s", data)
		}
	}
}

func TestInstallPreservesOtherSettingsAndIsIdempotent(t *testing.T) {
	orig := `{
  "permissions": {"defaultMode": "bypassPermissions"},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "~/.claude/hooks/guard.sh"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "echo hi"}]}]
  },
  "model": "x"
}`
	p := settingsFile(t, orig)
	if changed, err := InstallClaudeHooks(p, bin); err != nil || !changed {
		t.Fatal(changed, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{"guard.sh", `"matcher": "Bash"`, "echo hi", "bypassPermissions", `"model": "x"`} {
		if !strings.Contains(s, want) {
			t.Errorf("lost %q", want)
		}
	}
	if strings.Index(s, "permissions") > strings.Index(s, "hooks") || strings.Index(s, "hooks") > strings.Index(s, `"model"`) {
		t.Fatalf("key order changed:\n%s", s)
	}
	bak, err := os.ReadFile(p + ".nostromo.bak")
	if err != nil || string(bak) != orig {
		t.Fatal("backup of the original must be written")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	changed, err := InstallClaudeHooks(p, bin)
	if err != nil || changed {
		t.Fatalf("second install must change nothing: %v %v", changed, err)
	}
	if n := countOurs(t, p); n["SessionStart"] != 1 || n["UserPromptSubmit"] != 1 {
		t.Fatalf("duplicated: %v", n)
	}

	// A moved home directory updates the existing command instead of adding one.
	if changed, _ := InstallClaudeHooks(p, "/Users/me/.nostromo/bin/agent"); !changed {
		t.Fatal("path change must update")
	}
	data, _ = os.ReadFile(p)
	if strings.Contains(string(data), "/home/me/") || countOurs(t, p)["SessionStart"] != 1 {
		t.Fatalf("not updated in place:\n%s", data)
	}
	bak2, _ := os.ReadFile(p + ".nostromo.bak")
	if string(bak2) != orig {
		t.Fatal("the first backup must be kept")
	}
}

func TestRemove(t *testing.T) {
	p := settingsFile(t, `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"guard"}]}],"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"},{"type":"command","command":"/x/.nostromo/bin/agent hook claude"}]}]},"other":1}`)
	InstallClaudeHooks(p, bin)
	changed, err := RemoveClaudeHooks(p)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	if strings.Contains(s, "hook claude") || !strings.Contains(s, "echo hi") || !strings.Contains(s, "guard") || strings.Contains(s, "UserPromptSubmit") {
		t.Fatalf("after remove:\n%s", s)
	}
	if changed, _ := RemoveClaudeHooks(p); changed {
		t.Fatal("second remove must change nothing")
	}
	if ok, _ := ClaudeHooksInstalled(p); ok {
		t.Fatal("must report not installed")
	}

	only := settingsFile(t, "")
	InstallClaudeHooks(only, bin)
	RemoveClaudeHooks(only)
	data, _ = os.ReadFile(only)
	if strings.TrimSpace(string(data)) != "{}" {
		t.Fatalf("empty hooks object must be dropped: %s", data)
	}
	if changed, err := RemoveClaudeHooks(filepath.Join(t.TempDir(), "missing.json")); changed || err != nil {
		t.Fatal("missing file")
	}
	if changed, _ := RemoveClaudeHooks(settingsFile(t, `{"a":1}`)); changed {
		t.Fatal("no hooks key")
	}
	odd := settingsFile(t, `{"hooks":{"X":"weird","Y":[{"hooks":"nope"}]}}`)
	if changed, err := RemoveClaudeHooks(odd); changed || err != nil {
		t.Fatal("odd shapes are left alone", changed, err)
	}
}

func TestInvalidSettingsAreLeftAlone(t *testing.T) {
	for _, content := range []string{`{"a":`, `[1,2]`, `{"a":1} {"b":2}`, `{"a":1} x`, `{"hooks": []}`, `{"hooks": {"SessionStart": {"a":1}}}`, `{1:2}`} {
		p := settingsFile(t, content)
		_, err := InstallClaudeHooks(p, bin)
		if !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%q: %v", content, err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != content {
			t.Errorf("%q: file was modified", content)
		}
	}
	if _, err := RemoveClaudeHooks(settingsFile(t, `{"hooks": 1}`)); !errors.Is(err, ErrInvalidSettings) {
		t.Fatal("remove with bad hooks")
	}
	if _, err := RemoveClaudeHooks(settingsFile(t, `nope`)); !errors.Is(err, ErrInvalidSettings) {
		t.Fatal("remove with bad json")
	}
	if ok, err := ClaudeHooksInstalled(settingsFile(t, `{"hooks": 1}`)); ok || err != nil {
		t.Fatal("installed check on bad hooks")
	}
	if _, err := ClaudeHooksInstalled(settingsFile(t, `nope`)); err == nil {
		t.Fatal("installed check on bad json")
	}
	if ok, _ := ClaudeHooksInstalled(settingsFile(t, `{}`)); ok {
		t.Fatal("no hooks")
	}
	if _, err := InstallClaudeHooks(t.TempDir(), bin); err == nil {
		t.Fatal("directory path must fail")
	}
}

func TestSnippet(t *testing.T) {
	s := ClaudeHooksSnippet(bin)
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil || !strings.Contains(s, "UserPromptSubmit") || !strings.Contains(s, bin+" hook claude") {
		t.Fatalf("snippet: %s", s)
	}
}

func TestObjectHelpers(t *testing.T) {
	o := newObject()
	o.set("a", json.RawMessage(`1`))
	o.set("b", json.RawMessage(`2`))
	o.set("a", json.RawMessage(`3`))
	o.del("missing")
	o.del("a")
	if string(o.marshal()) != `{"b":2}` {
		t.Fatal(string(o.marshal()))
	}
	o.set("bad", json.RawMessage(`{`))
	if _, err := o.indent(); err == nil {
		t.Fatal("indent of broken raw must fail")
	}
	if _, err := parseObject([]byte(`{"a" 1}`)); err == nil {
		t.Fatal("bad token")
	}
	if _, err := parseObject([]byte(`"str"`)); err == nil {
		t.Fatal("not object")
	}
	if _, err := parseObject([]byte(`{"a":1`)); err == nil {
		t.Fatal("unterminated")
	}
}

func TestWriteSettingsErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{}`), 0o644)
	os.Mkdir(p+".nostromo.bak", 0o755)
	os.WriteFile(filepath.Join(p+".nostromo.bak", "x"), nil, 0o644)
	// An existing backup path (even a directory) is never overwritten.
	if _, err := InstallClaudeHooks(p, bin); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p2, []byte(`{}`), 0o200)
	if os.Getuid() != 0 {
		if _, err := InstallClaudeHooks(p2, bin); err == nil {
			t.Fatal("unreadable file must fail")
		}
	}
	ro := t.TempDir()
	p3 := filepath.Join(ro, "settings.json")
	os.WriteFile(p3, []byte(`{}`), 0o644)
	os.Chmod(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	if os.Getuid() != 0 {
		if _, err := InstallClaudeHooks(p3, bin); err == nil {
			t.Fatal("read-only directory must fail")
		}
	}
}

func TestInstallRemoveRoundTripIsByteExact(t *testing.T) {
	orig := `{
  "permissions": {
    "defaultMode": "bypassPermissions"
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Read",
        "hooks": [
          {
            "type": "command",
            "command": "~/.claude/hooks/guard.sh",
            "statusMessage": "Checking..."
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo start",
            "zeta": 1,
            "alpha": 2
          }
        ]
      }
    ]
  },
  "model": "x"
}
`
	p := settingsFile(t, orig)
	if changed, err := InstallClaudeHooks(p, bin); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := RemoveClaudeHooks(p); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Fatalf("round trip changed the file:\n%s", got)
	}
}

func TestMovedBinaryUpdatesOnlyOurEntry(t *testing.T) {
	p := settingsFile(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/old/.nostromo/bin/agent hook claude","timeout":5,"z":1}]}]}}`)
	if changed, err := InstallClaudeHooks(p, bin); err != nil || !changed {
		t.Fatal(changed, err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), bin+" hook claude") || strings.Contains(string(data), "/old/") || !strings.Contains(string(data), `"z": 1`) {
		t.Fatalf("%s", data)
	}
}

func TestHookCommandQuoting(t *testing.T) {
	spaced := "/home/a b/.nostromo/bin/agent"
	if got := HookCommand(spaced, "claude"); got != "'/home/a b/.nostromo/bin/agent' hook claude" {
		t.Fatal(got)
	}
	p := settingsFile(t, "")
	InstallClaudeHooks(p, spaced)
	if ok, _ := ClaudeHooksInstalled(p); !ok {
		t.Fatal("quoted command must be recognized as ours")
	}
	if changed, _ := InstallClaudeHooks(p, spaced); changed {
		t.Fatal("idempotent with a quoted path")
	}
}
