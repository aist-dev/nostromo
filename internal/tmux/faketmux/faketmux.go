// Package faketmux is an in-memory tmux.API for unit tests.
package faketmux

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/aist-dev/nostromo/internal/tmux"
)

// Session is a fake tmux session.
type Session struct {
	Spec       tmux.SessionSpec
	Dead       bool
	DeadStatus int
	Attached   int
}

// Fake records calls and keeps sessions in memory.
type Fake struct {
	mu       sync.Mutex
	Sessions map[string]*Session
	Ver      string
	Calls    []string
	Errs     map[string]error // per method name: Version, NewSession, ListPanes, KillSession, Scroll, SwitchClient
	Switched []string
	Scrolls  []string
	// AttachCmd, when set, replaces the argv returned by AttachArgs.
	AttachCmd []string
	// Screen is returned by Capture.
	Screen string
}

// New returns a fake reporting tmux 3.4.
func New() *Fake {
	return &Fake{Sessions: map[string]*Session{}, Ver: "tmux 3.4", Errs: map[string]error{}}
}

func (f *Fake) call(name string) error {
	f.Calls = append(f.Calls, name)
	return f.Errs[name]
}

// Version implements tmux.API.
func (f *Fake) Version(context.Context) (tmux.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Version"); err != nil {
		return tmux.Version{}, err
	}
	return tmux.ParseVersion(f.Ver)
}

// NewSession implements tmux.API.
func (f *Fake) NewSession(_ context.Context, spec tmux.SessionSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("NewSession"); err != nil {
		return err
	}
	if _, ok := f.Sessions[spec.Name]; ok {
		return errors.New("duplicate session: " + spec.Name)
	}
	f.Sessions[spec.Name] = &Session{Spec: spec}
	return nil
}

// ListPanes implements tmux.API.
func (f *Fake) ListPanes(context.Context) ([]tmux.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListPanes"); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f.Sessions))
	for n := range f.Sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	var panes []tmux.Pane
	for i, n := range names {
		s := f.Sessions[n]
		panes = append(panes, tmux.Pane{Session: n, ChatID: s.Spec.ChatID, Dead: s.Dead, DeadStatus: s.DeadStatus, Attached: s.Attached, PID: 1000 + i})
	}
	return panes, nil
}

// KillSession implements tmux.API.
func (f *Fake) KillSession(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("KillSession"); err != nil {
		return err
	}
	if _, ok := f.Sessions[name]; !ok {
		return tmux.ErrNoSession
	}
	delete(f.Sessions, name)
	return nil
}

// Scroll implements tmux.API.
func (f *Fake) Scroll(_ context.Context, name, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Scroll"); err != nil {
		return err
	}
	if _, ok := f.Sessions[name]; !ok {
		return tmux.ErrNoSession
	}
	f.Scrolls = append(f.Scrolls, name+":"+dir)
	return nil
}

// AttachArgs implements tmux.API.
func (f *Fake) AttachArgs(name string) []string {
	if f.AttachCmd != nil {
		return f.AttachCmd
	}
	return []string{"tmux", "-L", "fake", "attach-session", "-t", "=" + name}
}

// SwitchClient implements tmux.API.
func (f *Fake) SwitchClient(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("SwitchClient"); err != nil {
		return err
	}
	f.Switched = append(f.Switched, name)
	return nil
}

// Kill marks a session's pane as dead with the given exit status (code 0 removes it,
// like tmux does with remain-on-exit failed).
func (f *Fake) Kill(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == 0 {
		delete(f.Sessions, name)
		return
	}
	if s, ok := f.Sessions[name]; ok {
		s.Dead = true
		s.DeadStatus = code
	}
}

// Get returns a session by name.
func (f *Fake) Get(name string) (*Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Sessions[name]
	return s, ok
}

// Capture implements tmux.API.
func (f *Fake) Capture(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("Capture"); err != nil {
		return "", err
	}
	if _, ok := f.Sessions[name]; !ok {
		return "", tmux.ErrNoSession
	}
	return f.Screen, nil
}
