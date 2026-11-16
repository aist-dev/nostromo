// Package hook handles agent hook events for chats started by nostromo.
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aist-dev/nostromo/internal/chat"
	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/store"
)

// TitleLen is the length of a title taken from the first prompt, in runes.
const TitleLen = 60

// ClaudeEvent is the subset of the Claude Code hook input nostromo reads.
type ClaudeEvent struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Source        string `json:"source"`
	Prompt        string `json:"prompt"`
}

// Claude applies one Claude Code hook event to the chat record. It returns an
// error for logging only; the hook process always exits 0.
func Claude(st *store.Store, chatID string, in io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return err
	}
	var ev ClaudeEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("invalid hook input: %w", err)
	}
	switch ev.HookEventName {
	case "SessionStart":
		if ev.SessionID == "" || ev.Source == "fork" {
			return nil
		}
		_, err = st.Update(chatID, func(r *store.Record) error {
			r.ResumeID = ev.SessionID
			return nil
		})
		return err
	case "UserPromptSubmit":
		title := chat.CleanTitle(ev.Prompt)
		if r := []rune(title); len(r) > TitleLen {
			title = strings.TrimSpace(string(r[:TitleLen]))
		}
		if title == "" {
			return nil
		}
		_, err = st.Update(chatID, func(r *store.Record) error {
			if r.Title == "" {
				r.Title = title
			}
			return nil
		})
		return err
	default:
		return nil
	}
}

// Command implements `agent hook <adapter>`. It never writes to stdout and always
// exits 0: hook output may reach the model, and a failing hook must not get in the
// agent's way.
func Command(ctx context.Context, args []string, env cli.Env) int {
	chatID := env.Getenv(chat.EnvChatID)
	if chatID == "" {
		return 0
	}
	paths, err := env.Paths()
	if err != nil {
		return 0
	}
	logf := func(format string, a ...any) {
		os.MkdirAll(paths.Logs(), 0o700)
		f, err := os.OpenFile(paths.HookLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		log.New(f, "", log.LstdFlags).Printf(format, a...)
	}
	if len(args) != 1 || args[0] != "claude" {
		logf("unknown hook adapter %q", strings.Join(args, " "))
		return 0
	}
	st := store.New(filepath.Join(paths.Chats()))
	st.LockTimeout = time.Second
	if err := Claude(st, chatID, env.Stdin); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			logf("chat %s: no record", chatID)
		} else {
			logf("chat %s: %v", chatID, err)
		}
	}
	return 0
}
