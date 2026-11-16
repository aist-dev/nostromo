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

// The hook groups of one event, kept as ordered objects with raw values, so other
// tools' entries are written back byte for byte (apart from indentation).
type group struct {
	obj     *object
	entries []json.RawMessage
	ok      bool // the "hooks" field is an array we understand
}

func parseGroups(raw json.RawMessage) ([]*group, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	groups := make([]*group, 0, len(items))
	for _, it := range items {
		obj, err := parseObject(it)
		if err != nil {
			return nil, err
		}
		g := &group{obj: obj}
		if h, ok := obj.get("hooks"); ok && json.Unmarshal(h, &g.entries) == nil {
			g.ok = true
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func marshalArray(items []json.RawMessage) json.RawMessage {
	var b []byte
	b = append(b, '[')
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	return append(b, ']')
}

func marshalGroups(groups []*group) json.RawMessage {
	items := make([]json.RawMessage, len(groups))
	for i, g := range groups {
		if g.ok {
			g.obj.set("hooks", marshalArray(g.entries))
		}
		items[i] = g.obj.marshal()
	}
	return marshalArray(items)
}

func entryCommand(e json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
	}
	json.Unmarshal(e, &v)
	return v.Command
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
		groups, err := parseGroups(raw)
		if err != nil {
			return false, fmt.Errorf("%w: %s: hooks.%s has an unexpected shape", ErrInvalidSettings, path, ev)
		}
		found := false
		for _, g := range groups {
			for i, e := range g.entries {
				cmd := entryCommand(e)
				if !isOurHook(cmd, "claude") {
					continue
				}
				found = true
				if cmd != want {
					entry, _ := parseObject(e)
					c, _ := json.Marshal(want)
					entry.set("command", c)
					g.entries[i] = entry.marshal()
					changed = true
				}
			}
		}
		if !found {
			entry, _ := json.Marshal(map[string]any{"hooks": []hookEntry{{Type: "command", Command: want, Timeout: 5}}})
			obj, _ := parseObject(entry)
			g := &group{obj: obj, ok: true}
			json.Unmarshal(mustGet(obj, "hooks"), &g.entries)
			groups = append(groups, g)
			changed = true
		}
		hooks.set(ev, marshalGroups(groups))
	}
	if !changed {
		return false, nil
	}
	o.set("hooks", hooks.marshal())
	return true, writeSettings(path, o, existed)
}

func mustGet(o *object, key string) json.RawMessage {
	v, _ := o.get(key)
	return v
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
		groups, err := parseGroups(v)
		if err != nil {
			continue
		}
		var kept []*group
		evChanged := false
		for _, g := range groups {
			if !g.ok {
				kept = append(kept, g)
				continue
			}
			var rest []json.RawMessage
			for _, e := range g.entries {
				if isOurHook(entryCommand(e), "claude") {
					evChanged = true
					continue
				}
				rest = append(rest, e)
			}
			if len(rest) == 0 && len(g.entries) > 0 {
				continue
			}
			g.entries = rest
			kept = append(kept, g)
		}
		if !evChanged {
			continue
		}
		changed = true
		if len(kept) == 0 {
			hooks.del(ev)
		} else {
			hooks.set(ev, marshalGroups(kept))
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
		groups, _ := parseGroups(v)
		found := false
		for _, g := range groups {
			for _, e := range g.entries {
				if isOurHook(entryCommand(e), "claude") {
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
