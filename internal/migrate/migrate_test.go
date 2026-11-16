package migrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/store"
)

func sample(t *testing.T, paths config.Paths, id string, v int) {
	t.Helper()
	os.MkdirAll(paths.Chats(), 0o700)
	b, _ := json.Marshal(map[string]any{"v": v, "id": id, "agent": "claude", "cwd": "/x", "title": "old", "resume_id": id, "legacy": true})
	os.WriteFile(filepath.Join(paths.Chats(), id+".json"), b, 0o600)
}

// A hypothetical v1 -> v2 migration that upper-cases titles.
var upper = []Migration{{From: 1, Name: "upper titles", Apply: func(r *store.Record) error {
	r.Title = strings.ToUpper(r.Title)
	return nil
}}}

func TestRunMigratesOnceWithBackup(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	sample(t, paths, "aaaaaaaa-0000-4000-8000-000000000000", 1)
	sample(t, paths, "bbbbbbbb-0000-4000-8000-000000000000", 0)
	done, err := Run(paths, upper, 2)
	if err != nil || len(done) != 2 {
		t.Fatalf("%v %v", done, err)
	}
	data, _ := os.ReadFile(filepath.Join(paths.Chats(), "aaaaaaaa-0000-4000-8000-000000000000.json"))
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["title"] != "OLD" || m["v"].(float64) != 2 || m["legacy"] != true {
		t.Fatalf("migrated record: %s", data)
	}
	bak, err := os.ReadFile(filepath.Join(paths.Home, "chats.bak-v1", "aaaaaaaa-0000-4000-8000-000000000000.json"))
	if err != nil || !strings.Contains(string(bak), `"title":"old"`) {
		t.Fatalf("backup: %s %v", bak, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "chats.bak-v1", ".lock")); err == nil {
		t.Fatal("lock file must not be copied")
	}
	// Second run: nothing to do. (Records at v2 are newer than the store's schema,
	// so List skips them; that is the "old version never rewrites" rule.)
	done, err = Run(paths, upper, 2)
	if err != nil || len(done) != 0 {
		t.Fatalf("second run: %v %v", done, err)
	}
}

func TestRunNothingToDo(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	if done, err := Run(paths, All, store.SchemaVersion); err != nil || done != nil {
		t.Fatal(done, err)
	}
	sample(t, paths, "aaaaaaaa-0000-4000-8000-000000000000", 1)
	if done, err := Run(paths, All, store.SchemaVersion); err != nil || done != nil {
		t.Fatal(done, err)
	}
}

func TestRunErrors(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	sample(t, paths, "aaaaaaaa-0000-4000-8000-000000000000", 1)
	if _, err := Run(paths, nil, 2); err == nil {
		t.Fatal("missing migration")
	}
	failing := []Migration{{From: 1, Name: "boom", Apply: func(*store.Record) error { return errors.New("x") }}}
	if _, err := Run(paths, failing, 2); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	if _, err := Run(config.Paths{Home: file}, upper, 2); err == nil {
		t.Fatal("unreadable chats")
	}
	// Backup directory cannot be created.
	p2 := config.Paths{Home: t.TempDir()}
	sample(t, p2, "aaaaaaaa-0000-4000-8000-000000000000", 1)
	os.WriteFile(p2.Chats()+".bak-v1", nil, 0o644)
	os.Remove(p2.Chats() + ".bak-v1")
	os.Chmod(p2.Home, 0o500)
	defer os.Chmod(p2.Home, 0o700)
	if os.Getuid() != 0 {
		if _, err := Run(p2, upper, 2); err == nil {
			t.Fatal("read-only home must fail the backup")
		}
	}
}

func TestBackupHelpers(t *testing.T) {
	dir := t.TempDir()
	if err := backup(filepath.Join(dir, "missing"), filepath.Join(dir, "dst")); err == nil {
		t.Fatal("missing source")
	}
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o700)
	os.WriteFile(filepath.Join(src, "a.json"), []byte("1"), 0o600)
	dst := filepath.Join(dir, "dst")
	if err := backup(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := backup(src, dst); err != nil {
		t.Fatal("existing backup is kept")
	}
	if err := copyFile(filepath.Join(src, "a.json"), filepath.Join(dst, "a.json")); err != nil {
		t.Fatal("existing file is kept")
	}
	if err := copyFile(filepath.Join(src, "nope"), filepath.Join(dst, "b")); err == nil {
		t.Fatal("missing file")
	}
	if err := copyFile(filepath.Join(src, "a.json"), filepath.Join(dir, "no", "b")); err == nil {
		t.Fatal("missing dir")
	}
	if err := copyFile(src, filepath.Join(dst, "c")); err == nil {
		t.Fatal("copying a directory fails")
	}
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, nil, 0o644)
	if err := backup(src, filepath.Join(blocker, "dst")); err == nil {
		t.Fatal("mkdir under a file")
	}
	os.WriteFile(filepath.Join(src, "zz.json"), []byte("2"), 0o000)
	if os.Getuid() != 0 {
		if err := backup(src, filepath.Join(dir, "dst2")); err == nil {
			t.Fatal("unreadable file")
		}
	}
}
