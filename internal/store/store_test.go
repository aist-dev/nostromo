package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	id1 = "550e8400-e29b-41d4-a716-446655440000"
	id2 = "550e8400-ffff-41d4-a716-446655440000"
	id3 = "a1b2c3d4-0000-4000-8000-000000000000"
)

func newRecord(id string) Record {
	t := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	return Record{ID: id, Agent: "claude", Cwd: "/tmp", CreatedAt: t, LastStartedAt: t, ResumeID: id, TmuxSession: SessionName(id)}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "chats"))
	r := newRecord(id1)
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(r); err == nil {
		t.Fatal("duplicate create must fail")
	}
	got, err := s.Get(id1)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != SchemaVersion || got.TmuxSession != "agent-550e8400" || got.Agent != "claude" {
		t.Fatalf("got %+v", got)
	}
	upd, err := s.Update(id1, func(r *Record) error { r.Title = "hello"; r.ID = "ignored"; return nil })
	if err != nil || upd.Title != "hello" || upd.ID != id1 {
		t.Fatalf("update: %v %+v", err, upd)
	}
	if _, err := s.Update(id1, func(*Record) error { return errors.New("boom") }); err == nil {
		t.Fatal("fn error must propagate")
	}
	if err := s.Delete(id1); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.Get(id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := s.Update(id1, func(*Record) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestInvalidIDs(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"", "../etc/passwd", "ABC", strings.Repeat("a", 65), "a b"} {
		r := newRecord(id)
		if err := s.Create(r); !errors.Is(err, ErrInvalidID) {
			t.Errorf("create %q: %v", id, err)
		}
		if err := s.Replace(r); !errors.Is(err, ErrInvalidID) {
			t.Errorf("replace %q: %v", id, err)
		}
		if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("get %q: %v", id, err)
		}
		if _, err := s.Update(id, func(*Record) error { return nil }); !errors.Is(err, ErrNotFound) {
			t.Errorf("update %q: %v", id, err)
		}
		if err := s.Delete(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete %q: %v", id, err)
		}
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Create(newRecord(id1))
	for i := 0; i < 5; i++ {
		s.Update(id1, func(r *Record) error { r.Title = fmt.Sprint(i); return nil })
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
	info, _ := os.Stat(filepath.Join(dir, id1+".json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v", info.Mode().Perm())
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s := New(t.TempDir())
	r := newRecord(id1)
	r.Title = "0"
	s.Create(r)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Update(id1, func(r *Record) error {
				var n int
				fmt.Sscan(r.Title, &n)
				r.Title = fmt.Sprint(n + 1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := s.Get(id1)
	if got.Title != "20" {
		t.Fatalf("lost updates: title = %s", got.Title)
	}
}

func TestLockTimeout(t *testing.T) {
	s := New(t.TempDir())
	s.LockTimeout = 50 * time.Millisecond
	unlock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other := &Store{Dir: s.Dir, LockTimeout: 50 * time.Millisecond}
	if _, err := other.Lock(); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
	if err := other.Create(newRecord(id1)); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("create: %v", err)
	}
	if _, err := other.Update(id1, func(*Record) error { return nil }); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("update: %v", err)
	}
	if err := other.Delete(id1); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("delete: %v", err)
	}
}

func TestLockErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o644)
	if _, err := New(filepath.Join(file, "chats")).Lock(); err == nil {
		t.Fatal("mkdir under a file must fail")
	}
	d := filepath.Join(dir, "chats")
	os.MkdirAll(filepath.Join(d, ".lock"), 0o755)
	if _, err := New(d).Lock(); err == nil {
		t.Fatal("lock path is a directory: must fail")
	}
}

func TestUnknownFieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	raw := `{"v":1,"id":"` + id1 + `","agent":"claude","cwd":"/x","title":"","created_at":"2026-11-16T10:00:00Z",` +
		`"last_started_at":"2026-11-16T10:00:00Z","resume_id":"` + id1 + `","tmux_session":"agent-550e8400",` +
		`"state":"waiting","zeta":{"a":1}}`
	os.WriteFile(filepath.Join(dir, id1+".json"), []byte(raw), 0o600)
	if _, err := s.Update(id1, func(r *Record) error { r.Title = "t"; return nil }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, id1+".json"))
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["state"] != "waiting" || m["zeta"] == nil || m["title"] != "t" {
		t.Fatalf("unknown fields lost: %s", data)
	}
	r, _ := s.Get(id1)
	if v, ok := r.Extra("state"); !ok || string(v) != `"waiting"` {
		t.Fatalf("Extra: %s %v", v, ok)
	}
	r.SetExtra("new", json.RawMessage(`true`))
	var r2 Record
	r2.SetExtra("x", json.RawMessage(`1`))
	if _, ok := r2.Extra("x"); !ok {
		t.Fatal("SetExtra on empty record")
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"new":true`) {
		t.Fatalf("SetExtra not marshalled: %s", b)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	var r Record
	if err := r.UnmarshalJSON([]byte(`[]`)); err == nil {
		t.Fatal("array must fail")
	}
	if err := r.UnmarshalJSON([]byte(`{"v":"x"}`)); err == nil {
		t.Fatal("bad type must fail")
	}
}

func TestListSkipsBadFiles(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	s.Create(newRecord(id2))
	s.Create(newRecord(id1))
	os.WriteFile(filepath.Join(dir, id3+".json"), []byte("{broken"), 0o600)
	os.WriteFile(filepath.Join(dir, "NOT-AN-ID.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600)
	os.Mkdir(filepath.Join(dir, "sub.json"), 0o755)
	mismatch := newRecord(id2)
	b, _ := json.Marshal(mismatch)
	os.WriteFile(filepath.Join(dir, "bbbbbbbb-0000-4000-8000-000000000000.json"), b, 0o600)
	newer := newRecord("cccccccc-0000-4000-8000-000000000000")
	newer.V = SchemaVersion + 1
	b, _ = json.Marshal(newer)
	os.WriteFile(filepath.Join(dir, newer.ID+".json"), b, 0o600)

	recs, bad, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != id1 || recs[1].ID != id2 {
		t.Fatalf("records: %+v", recs)
	}
	if len(bad) != 4 {
		t.Fatalf("bad files: %+v", bad)
	}
	if _, err := s.Update(newer.ID, func(*Record) error { return nil }); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema must not be rewritten: %v", err)
	}
}

func TestListMissingAndUnreadableDir(t *testing.T) {
	recs, bad, err := New(filepath.Join(t.TempDir(), "none")).List()
	if err != nil || recs != nil || bad != nil {
		t.Fatalf("missing dir: %v %v %v", recs, bad, err)
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	if _, _, err := New(f).List(); err == nil {
		t.Fatal("file as dir must fail")
	}
	if _, err := New(f).Find("abc"); err == nil {
		t.Fatal("find must propagate list error")
	}
}

func TestReadError(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, id1+".json"), 0o755)
	if _, err := New(dir).Get(id1); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("directory instead of file: %v", err)
	}
}

func TestFind(t *testing.T) {
	s := New(t.TempDir())
	s.Create(newRecord(id1))
	s.Create(newRecord(id2))
	s.Create(newRecord(id3))
	if r, err := s.Find("a1b"); err != nil || r.ID != id3 {
		t.Fatalf("prefix: %v %v", r.ID, err)
	}
	if r, err := s.Find(" " + strings.ToUpper(id1) + " "); err != nil || r.ID != id1 {
		t.Fatalf("full id: %v %v", r.ID, err)
	}
	if _, err := s.Find("550e8400"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := s.Find("ffff"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none: %v", err)
	}
	if _, err := s.Find(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
}

func TestSessionName(t *testing.T) {
	if SessionName(id1) != "agent-550e8400" || SessionName("abc") != "agent-abc" {
		t.Fatal("SessionName")
	}
}
