// Package setup installs, checks and removes nostromo on a machine.
package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aist-dev/nostromo/internal/config"
)

// ClaudeHookEvents are the Claude Code hook events nostromo listens to.
var ClaudeHookEvents = []string{"SessionStart", "UserPromptSubmit"}

// ErrInvalidSettings means settings.json is not valid JSON and was left untouched.
var ErrInvalidSettings = errors.New("settings file is not valid JSON")

// HookCommand is the command written into agent settings for an adapter.
func HookCommand(binary, adapter string) string {
	return binary + " hook " + adapter
}

func isOurHook(cmd, adapter string) bool {
	return strings.HasSuffix(cmd, "/.nostromo/bin/agent hook "+adapter)
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// ClaudeHooksSnippet is the JSON a user can paste when the settings file cannot be edited.
func ClaudeHooksSnippet(binary string) string {
	hooks := map[string]any{}
	for _, ev := range ClaudeHookEvents {
		hooks[ev] = []any{map[string]any{"hooks": []hookEntry{{Type: "command", Command: HookCommand(binary, "claude"), Timeout: 5}}}}
	}
	b, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	return string(b)
}

// readSettings loads settings.json. A missing file is an empty object.
func readSettings(path string) (*object, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newObject(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	o, err := parseObject(data)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %s: %v", ErrInvalidSettings, path, err)
	}
	return o, true, nil
}

func writeSettings(path string, o *object, existed bool) error {
	data, err := o.indent()
	if err != nil {
		return err
	}
	if existed {
		bak := path + ".nostromo.bak"
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			orig, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(bak, orig, 0o600); err != nil {
				return err
			}
		}
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return config.WriteFileAtomic(path, data, mode)
}

// eventGroups reads hooks.<event> as a list of matcher groups.
func eventGroups(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	var groups []map[string]json.RawMessage
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// InstallClaudeHooks merges nostromo's hooks into a Claude Code settings file.
// Other settings and hooks are preserved; running it again changes nothing.
// It reports whether the file was modified.
func InstallClaudeHooks(path, binary string) (bool, error) {
	o, existed, err := readSettings(path)
	if err != nil {
		return false, err
	}
	hooks := newObject()
	if raw, ok := o.get("hooks"); ok {
		if hooks, err = parseObject(raw); err != nil {
			return false, fmt.Errorf("%w: %s: \"hooks\" is not an object", ErrInvalidSettings, path)
		}
	}
	want := HookCommand(binary, "claude")
	changed := false
	for _, ev := range ClaudeHookEvents {
		raw, _ := hooks.get(ev)
		groups, err := eventGroups(raw)
		if err != nil {
			return false, fmt.Errorf("%w: %s: hooks.%s has an unexpected shape", ErrInvalidSettings, path, ev)
		}
		found := false
		for gi, g := range groups {
			var entries []map[string]any
			if json.Unmarshal(g["hooks"], &entries) != nil {
				continue
			}
			for ei, e := range entries {
				cmd, _ := e["command"].(string)
				if !isOurHook(cmd, "claude") {
					continue
				}
				found = true
				if cmd != want {
					entries[ei]["command"] = want
					b, _ := json.Marshal(entries)
					groups[gi]["hooks"] = b
					changed = true
				}
			}
		}
		if !found {
			entry, _ := json.Marshal([]hookEntry{{Type: "command", Command: want, Timeout: 5}})
			groups = append(groups, map[string]json.RawMessage{"hooks": entry})
			changed = true
		}
		b, _ := json.Marshal(groups)
		hooks.set(ev, b)
	}
	if !changed {
		return false, nil
	}
	o.set("hooks", hooks.marshal())
	return true, writeSettings(path, o, existed)
}

// RemoveClaudeHooks deletes nostromo's hooks from a settings file, leaving everything else.
func RemoveClaudeHooks(path string) (bool, error) {
	o, existed, err := readSettings(path)
	if err != nil || !existed {
		return false, err
	}
	raw, ok := o.get("hooks")
	if !ok {
		return false, nil
	}
	hooks, err := parseObject(raw)
	if err != nil {
		return false, fmt.Errorf("%w: %s: \"hooks\" is not an object", ErrInvalidSettings, path)
	}
	changed := false
	for _, ev := range append([]string{}, hooks.keys...) {
		v, _ := hooks.get(ev)
		groups, err := eventGroups(v)
		if err != nil {
			continue
		}
		var kept []map[string]json.RawMessage
		for _, g := range groups {
			var entries []map[string]any
			if json.Unmarshal(g["hooks"], &entries) != nil {
				kept = append(kept, g)
				continue
			}
			var rest []map[string]any
			for _, e := range entries {
				cmd, _ := e["command"].(string)
				if isOurHook(cmd, "claude") {
					changed = true
					continue
				}
				rest = append(rest, e)
			}
			if len(rest) == 0 {
				continue
			}
			b, _ := json.Marshal(rest)
			g["hooks"] = b
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			hooks.del(ev)
		} else {
			b, _ := json.Marshal(kept)
			hooks.set(ev, b)
		}
	}
	if !changed {
		return false, nil
	}
	if hooks.len() == 0 {
		o.del("hooks")
	} else {
		o.set("hooks", hooks.marshal())
	}
	return true, writeSettings(path, o, existed)
}

// ClaudeHooksInstalled reports whether every nostromo hook is present.
func ClaudeHooksInstalled(path string) (bool, error) {
	o, _, err := readSettings(path)
	if err != nil {
		return false, err
	}
	raw, ok := o.get("hooks")
	if !ok {
		return false, nil
	}
	hooks, err := parseObject(raw)
	if err != nil {
		return false, nil
	}
	for _, ev := range ClaudeHookEvents {
		v, _ := hooks.get(ev)
		groups, _ := eventGroups(v)
		found := false
		for _, g := range groups {
			var entries []map[string]any
			json.Unmarshal(g["hooks"], &entries)
			for _, e := range entries {
				if cmd, _ := e["command"].(string); isOurHook(cmd, "claude") {
					found = true
				}
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}
