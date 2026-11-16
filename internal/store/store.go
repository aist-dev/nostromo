// Package store keeps chat records, one JSON file per chat.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
)

// SchemaVersion is the record format written by this build.
const SchemaVersion = 1

var (
	ErrNotFound    = errors.New("chat not found")
	ErrAmbiguous   = errors.New("ambiguous chat id prefix")
	ErrNewerSchema = errors.New("chat record has a newer schema version")
	ErrLockTimeout = errors.New("timed out waiting for the chat lock")
	ErrInvalidID   = errors.New("invalid chat id")
)

// Record describes one chat. Unknown JSON fields are kept and written back.
type Record struct {
	V             int       `json:"v"`
	ID            string    `json:"id"`
	Agent         string    `json:"agent"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	LastStartedAt time.Time `json:"last_started_at"`
	ResumeID      string    `json:"resume_id"`
	TmuxSession   string    `json:"tmux_session"`

	extra map[string]json.RawMessage
}

var knownFields = map[string]bool{
	"v": true, "id": true, "agent": true, "cwd": true, "title": true, "created_at": true,
	"last_started_at": true, "resume_id": true, "tmux_session": true,
}

type plainRecord struct {
	V             int       `json:"v"`
	ID            string    `json:"id"`
	Agent         string    `json:"agent"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	LastStartedAt time.Time `json:"last_started_at"`
	ResumeID      string    `json:"resume_id"`
	TmuxSession   string    `json:"tmux_session"`
}

// UnmarshalJSON decodes a record and remembers unknown fields.
func (r *Record) UnmarshalJSON(data []byte) error {
	var p plainRecord
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*r = Record{
		V: p.V, ID: p.ID, Agent: p.Agent, Cwd: p.Cwd, Title: p.Title, CreatedAt: p.CreatedAt,
		LastStartedAt: p.LastStartedAt, ResumeID: p.ResumeID, TmuxSession: p.TmuxSession,
	}
	for k, v := range all {
		if !knownFields[k] {
			if r.extra == nil {
				r.extra = map[string]json.RawMessage{}
			}
			r.extra[k] = v
		}
	}
	return nil
}

// MarshalJSON encodes a record including the unknown fields it was read with.
func (r Record) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal(plainRecord{
		V: r.V, ID: r.ID, Agent: r.Agent, Cwd: r.Cwd, Title: r.Title, CreatedAt: r.CreatedAt,
		LastStartedAt: r.LastStartedAt, ResumeID: r.ResumeID, TmuxSession: r.TmuxSession,
	})
	if err != nil || len(r.extra) == 0 {
		return known, err
	}
	keys := make([]string, 0, len(r.extra))
	for k := range r.extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.Write(known[:len(known)-1])
	for _, k := range keys {
		kb, _ := json.Marshal(k)
		b.WriteByte(',')
		b.Write(kb)
		b.WriteByte(':')
		b.Write(r.extra[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Extra returns an unknown field preserved from disk, if present.
func (r Record) Extra(key string) (json.RawMessage, bool) {
	v, ok := r.extra[key]
	return v, ok
}

// SetExtra sets a field not modelled by this version (used by migrations and tests).
func (r *Record) SetExtra(key string, value json.RawMessage) {
	if r.extra == nil {
		r.extra = map[string]json.RawMessage{}
	}
	r.extra[key] = value
}

// SessionName returns the tmux session name for a chat id.
func SessionName(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return "agent-" + id
}

// BadFile is a record file that could not be used.
type BadFile struct {
	Path string
	Err  error
}

// Store manages the record directory.
type Store struct {
	Dir         string
	LockTimeout time.Duration
}

// New returns a store for dir with the default lock timeout.
func New(dir string) *Store {
	return &Store{Dir: dir, LockTimeout: 5 * time.Second}
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// Lock takes the exclusive record lock. The returned function releases it.
func (s *Store) Lock() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.LockTimeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) write(r Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.path(r.ID), append(data, '\n'), 0o600)
}

func (s *Store) read(id string) (Record, error) {
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("%s: %w", s.path(id), err)
	}
	return r, nil
}

// Create writes a new record. It fails if a record with the same id exists.
func (s *Store) Create(r Record) error {
	if !validID(r.ID) {
		return ErrInvalidID
	}
	if r.V == 0 {
		r.V = SchemaVersion
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(s.path(r.ID)); err == nil {
		return fmt.Errorf("chat %s already exists", r.ID)
	}
	return s.write(r)
}

// Get reads a record by full id.
func (s *Store) Get(id string) (Record, error) {
	if !validID(id) {
		return Record{}, ErrNotFound
	}
	return s.read(id)
}

// Update applies fn to a record under the lock and writes the result.
// Records written by a newer schema version are never rewritten.
func (s *Store) Update(id string, fn func(*Record) error) (Record, error) {
	if !validID(id) {
		return Record{}, ErrNotFound
	}
	unlock, err := s.Lock()
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	r, err := s.read(id)
	if err != nil {
		return Record{}, err
	}
	if r.V > SchemaVersion {
		return Record{}, ErrNewerSchema
	}
	if err := fn(&r); err != nil {
		return Record{}, err
	}
	r.ID = id
	return r, s.write(r)
}

// Replace overwrites a record unconditionally (used by migrations).
func (s *Store) Replace(r Record) error {
	if !validID(r.ID) {
		return ErrInvalidID
	}
	return s.write(r)
}

// Delete removes a record. Deleting a missing record returns ErrNotFound.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// List returns all readable records of a known schema version, sorted by id,
// plus the files that were skipped. A missing directory is an empty list.
func (s *Store) List() ([]Record, []BadFile, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var recs []Record
	var bad []BadFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		p := filepath.Join(s.Dir, name)
		if !validID(id) {
			bad = append(bad, BadFile{Path: p, Err: ErrInvalidID})
			continue
		}
		r, err := s.read(id)
		if err != nil {
			bad = append(bad, BadFile{Path: p, Err: err})
			continue
		}
		if r.ID != id {
			bad = append(bad, BadFile{Path: p, Err: fmt.Errorf("id field %q does not match file name", r.ID)})
			continue
		}
		if r.V > SchemaVersion {
			bad = append(bad, BadFile{Path: p, Err: ErrNewerSchema})
			continue
		}
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	return recs, bad, nil
}

// Find resolves a full id or an unambiguous id prefix.
func (s *Store) Find(prefix string) (Record, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return Record{}, ErrNotFound
	}
	recs, _, err := s.List()
	if err != nil {
		return Record{}, err
	}
	var match []Record
	for _, r := range recs {
		if r.ID == prefix {
			return r, nil
		}
		if strings.HasPrefix(r.ID, prefix) {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		return Record{}, ErrNotFound
	case 1:
		return match[0], nil
	default:
		return Record{}, fmt.Errorf("%w %q: %d chats match", ErrAmbiguous, prefix, len(match))
	}
}
