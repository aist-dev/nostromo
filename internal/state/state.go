// Package state computes chat statuses and the grouped, sorted view shown to users.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"time"

	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
)

// Status of a chat.
type Status string

const (
	Running Status = "running"
	Failed  Status = "failed"
	Stopped Status = "stopped"
)

// Live is what tmux says about one chat.
type Live struct {
	Status   Status
	ExitCode *int
	Attached int
	Session  string
	PID      int
}

// Index maps chat ids to their tmux panes. Panes without the chat option are ignored.
func Index(panes []tmux.Pane) map[string]tmux.Pane {
	idx := make(map[string]tmux.Pane, len(panes))
	for _, p := range panes {
		if p.ChatID == "" {
			continue
		}
		if _, seen := idx[p.ChatID]; !seen {
			idx[p.ChatID] = p
		}
	}
	return idx
}

// Of computes the live state of a chat from the pane index.
func Of(id string, idx map[string]tmux.Pane) Live {
	p, ok := idx[id]
	if !ok {
		return Live{Status: Stopped}
	}
	l := Live{Status: Running, Attached: p.Attached, Session: p.Session, PID: p.PID}
	if p.Dead {
		l.Status = Failed
		if !p.Unreaped {
			code := p.DeadStatus
			l.ExitCode = &code
		}
	}
	return l
}

// Chat is a chat as shown in lists.
type Chat struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Agent         string    `json:"agent"`
	Cwd           string    `json:"cwd"`
	Status        Status    `json:"status"`
	ExitCode      *int      `json:"exit_code"`
	Attached      int       `json:"attached"`
	CreatedAt     time.Time `json:"created_at"`
	LastStartedAt time.Time `json:"last_started_at"`
}

// Group is the chats of one working directory.
type Group struct {
	Cwd   string `json:"cwd"`
	Name  string `json:"name"`
	Chats []Chat `json:"chats"`
}

func activeRank(s Status) int {
	if s == Stopped {
		return 1
	}
	return 0
}

// NewChat combines a record with its live state.
func NewChat(r store.Record, l Live) Chat {
	return Chat{
		ID: r.ID, Title: r.Title, Agent: r.Agent, Cwd: r.Cwd, Status: l.Status, ExitCode: l.ExitCode,
		Attached: l.Attached, CreatedAt: r.CreatedAt, LastStartedAt: r.LastStartedAt,
	}
}

// Build groups chats by directory. Groups are ordered by their latest activity,
// newest first; inside a group running and failed chats come before stopped ones,
// each block ordered by last start, newest first.
func Build(recs []store.Record, panes []tmux.Pane) []Group {
	idx := Index(panes)
	byCwd := map[string]*Group{}
	latest := map[string]time.Time{}
	for _, r := range recs {
		g, ok := byCwd[r.Cwd]
		if !ok {
			g = &Group{Cwd: r.Cwd, Name: filepath.Base(r.Cwd)}
			byCwd[r.Cwd] = g
		}
		g.Chats = append(g.Chats, NewChat(r, Of(r.ID, idx)))
		if r.LastStartedAt.After(latest[r.Cwd]) {
			latest[r.Cwd] = r.LastStartedAt
		}
	}
	groups := make([]Group, 0, len(byCwd))
	for _, g := range byCwd {
		sort.Slice(g.Chats, func(i, j int) bool {
			a, b := g.Chats[i], g.Chats[j]
			if ra, rb := activeRank(a.Status), activeRank(b.Status); ra != rb {
				return ra < rb
			}
			if !a.LastStartedAt.Equal(b.LastStartedAt) {
				return a.LastStartedAt.After(b.LastStartedAt)
			}
			return a.ID < b.ID
		})
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := latest[groups[i].Cwd], latest[groups[j].Cwd]
		if !a.Equal(b) {
			return a.After(b)
		}
		return groups[i].Cwd < groups[j].Cwd
	})
	return groups
}

// RecentDirs returns up to n distinct directories ordered by latest activity.
func RecentDirs(groups []Group, n int) []string {
	dirs := make([]string, 0, n)
	for _, g := range groups {
		if len(dirs) == n {
			break
		}
		dirs = append(dirs, g.Cwd)
	}
	return dirs
}

// Orphans returns tmux panes that carry a chat id with no matching record.
func Orphans(recs []store.Record, panes []tmux.Pane) []tmux.Pane {
	known := make(map[string]bool, len(recs))
	for _, r := range recs {
		known[r.ID] = true
	}
	var out []tmux.Pane
	for _, p := range panes {
		if p.ChatID != "" && !known[p.ChatID] {
			out = append(out, p)
		}
	}
	return out
}

// Peer is another machine shown in the UI switcher.
type Peer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Snapshot is the body of GET /api/state.
type Snapshot struct {
	Host            string   `json:"host"`
	Version         string   `json:"version"`
	UpdateAvailable string   `json:"update_available"`
	Agents          []string `json:"agents"`
	DefaultAgent    string   `json:"default_agent"`
	Peers           []Peer   `json:"peers"`
	RecentDirs      []string `json:"recent_dirs"`
	Groups          []Group  `json:"groups"`
}

// Encode returns the JSON body and its strong ETag. The ETag changes only when the content does.
func Encode(s Snapshot) ([]byte, string) {
	body, _ := json.Marshal(s)
	sum := sha256.Sum256(body)
	return body, `"` + hex.EncodeToString(sum[:12]) + `"`
}
