package state

import (
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
)

var base = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func rec(id, cwd string, minutes int) store.Record {
	return store.Record{ID: id, Cwd: cwd, Agent: "claude", LastStartedAt: base.Add(time.Duration(minutes) * time.Minute), CreatedAt: base}
}

func TestOf(t *testing.T) {
	idx := Index([]tmux.Pane{
		{Session: "agent-a", ChatID: "a", Attached: 2, PID: 7},
		{Session: "agent-b", ChatID: "b", Dead: true, DeadStatus: 3},
		{Session: "agent-b2", ChatID: "b"},
		{Session: "agent-z", ChatID: "z", Dead: true, Unreaped: true},
		{Session: "mine"},
	})
	if l := Of("z", idx); l.Status != Failed || l.ExitCode != nil {
		t.Fatalf("unreaped: %+v", l)
	}
	if len(idx) != 3 {
		t.Fatalf("index: %+v", idx)
	}
	if l := Of("a", idx); l.Status != Running || l.Attached != 2 || l.ExitCode != nil || l.Session != "agent-a" || l.PID != 7 {
		t.Fatalf("running: %+v", l)
	}
	if l := Of("b", idx); l.Status != Failed || *l.ExitCode != 3 {
		t.Fatalf("failed: %+v", l)
	}
	if l := Of("c", idx); l.Status != Stopped {
		t.Fatalf("stopped: %+v", l)
	}
}

func TestBuildGroupsAndSorting(t *testing.T) {
	recs := []store.Record{
		rec("s-old", "/p/api", 1),
		rec("r-new", "/p/api", 3),
		rec("s-new", "/p/api", 9),
		rec("f-mid", "/p/api", 5),
		rec("web1", "/p/web", 20),
		rec("lone", "/p/old", 0),
		rec("tie-b", "/p/tie", 0),
		rec("tie-a", "/p/tie", 0),
	}
	panes := []tmux.Pane{
		{Session: "x", ChatID: "r-new"},
		{Session: "y", ChatID: "f-mid", Dead: true, DeadStatus: 1},
	}
	g := Build(recs, panes)
	var order []string
	for _, gr := range g {
		order = append(order, gr.Cwd)
	}
	if strings.Join(order, ",") != "/p/web,/p/api,/p/old,/p/tie" {
		t.Fatalf("group order: %v", order)
	}
	api := g[1]
	if api.Name != "api" {
		t.Fatalf("name: %s", api.Name)
	}
	var ids []string
	for _, c := range api.Chats {
		ids = append(ids, c.ID+":"+string(c.Status))
	}
	if strings.Join(ids, ",") != "f-mid:failed,r-new:running,s-new:stopped,s-old:stopped" {
		t.Fatalf("chat order: %v", ids)
	}
	if g[3].Chats[0].ID != "tie-a" {
		t.Fatalf("tie break by id: %+v", g[3].Chats)
	}
	if dirs := RecentDirs(g, 2); strings.Join(dirs, ",") != "/p/web,/p/api" {
		t.Fatalf("recent: %v", dirs)
	}
	if dirs := RecentDirs(g, 10); len(dirs) != 4 {
		t.Fatalf("recent all: %v", dirs)
	}
	if len(Build(nil, nil)) != 0 {
		t.Fatal("empty")
	}
}

func TestOrphans(t *testing.T) {
	o := Orphans([]store.Record{rec("a", "/", 0)}, []tmux.Pane{{ChatID: "a"}, {ChatID: "ghost", Session: "agent-ghost"}, {Session: "mine"}})
	if len(o) != 1 || o[0].ChatID != "ghost" {
		t.Fatalf("%+v", o)
	}
}

func TestEncodeETag(t *testing.T) {
	s := Snapshot{Host: "h", Version: "1", Groups: Build([]store.Record{rec("a", "/x", 0)}, nil)}
	b1, e1 := Encode(s)
	b2, e2 := Encode(s)
	if e1 != e2 || string(b1) != string(b2) || !strings.HasPrefix(e1, `"`) {
		t.Fatal("ETag must be stable")
	}
	s.Groups[0].Chats[0].Title = "changed"
	if _, e3 := Encode(s); e3 == e1 {
		t.Fatal("ETag must change with content")
	}
	if !strings.Contains(string(b1), `"exit_code":null`) {
		t.Fatalf("body: %s", b1)
	}
}
