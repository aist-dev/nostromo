// Package migrate upgrades chat records to the current schema version.
//
// A migration moves records from version From to From+1. Before the first change
// the chats directory is copied to chats.bak-v<From>. Running migrations twice
// changes nothing, and records of a newer schema are never touched.
package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/store"
)

// Migration upgrades one record from version From to From+1.
type Migration struct {
	From  int
	Name  string
	Apply func(r *store.Record) error
}

// All is the list of record migrations, in order. Schema version 1 is the first,
// so there are none yet.
var All []Migration

// Run applies migrations up to target to every record and returns what it did.
func Run(paths config.Paths, migrations []Migration, target int) ([]string, error) {
	st := store.New(paths.Chats())
	recs, _, err := st.List()
	if err != nil {
		return nil, err
	}
	var done []string
	backedUp := map[int]bool{}
	for _, r := range recs {
		if r.V == 0 {
			r.V = 1
		}
		changed := false
		for r.V < target {
			m, ok := find(migrations, r.V)
			if !ok {
				return done, fmt.Errorf("no migration from schema version %d", r.V)
			}
			if !backedUp[m.From] {
				if err := backup(paths.Chats(), paths.Chats()+".bak-v"+fmt.Sprint(m.From)); err != nil {
					return done, fmt.Errorf("back up chats: %w", err)
				}
				backedUp[m.From] = true
			}
			if err := m.Apply(&r); err != nil {
				return done, fmt.Errorf("%s: chat %s: %w", m.Name, r.ID, err)
			}
			r.V = m.From + 1
			changed = true
		}
		if changed {
			if err := st.Replace(r); err != nil {
				return done, err
			}
			done = append(done, fmt.Sprintf("chat %s to schema v%d", r.ID[:8], r.V))
		}
	}
	return done, nil
}

func find(ms []Migration, from int) (Migration, bool) {
	for _, m := range ms {
		if m.From == from {
			return m, true
		}
	}
	return Migration{}, false
}

// backup copies the record files of src into dst once; an existing dst is kept.
func backup(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == ".lock" {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
