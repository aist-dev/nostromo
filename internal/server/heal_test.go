package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tailscale"
)

type healTS struct {
	st       tailscale.Status
	stErr    error
	sc       tailscale.ServeConfig
	serveErr error
}

func (f *healTS) Status(context.Context) (tailscale.Status, error) { return f.st, f.stErr }
func (f *healTS) ServeStatus(context.Context) (tailscale.ServeConfig, error) {
	return f.sc, nil
}
func (f *healTS) Serve(_ context.Context, p int, target string) error {
	if f.serveErr != nil {
		return f.serveErr
	}
	f.sc.HTTPS[p] = target
	return nil
}
func (f *healTS) ServeOff(context.Context, int) error { return nil }

func TestHealer(t *testing.T) {
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(cfgPath, []byte(config.DefaultFile), 0o644)
	var logs bytes.Buffer
	var applied []string
	ts := &healTS{stErr: errors.New("stopped"), sc: tailscale.ServeConfig{HTTPS: map[int]string{}}}
	h := &Healer{Find: func() (tailscale.API, error) { return ts, nil }, ConfigPath: cfgPath, Port: 8787, Logger: log.New(&logs, "", 0), SetUsers: func(u []string) { applied = u }}

	logs.Reset()
	none := &Healer{Find: func() (tailscale.API, error) { return nil, errors.New("not found") }, Logger: log.New(&logs, "", 0)}
	if none.Once(ctx) || !strings.Contains(logs.String(), "to be installed") {
		t.Fatal(logs.String())
	}
	steps := []struct {
		change func()
		done   bool
		log    string
	}{
		{func() {}, false, "waiting for Tailscale"},
		{func() {
			ts.stErr = nil
			ts.st = tailscale.Status{BackendState: "Running", DNSName: "box.t.ts.net", Login: "me@example.com"}
		}, false, "waiting for MagicDNS"},
		{func() {
			ts.st.MagicDNSEnabled = true
			ts.st.CertDomains = []string{"box.t.ts.net"}
			ts.serveErr = errors.New("Access denied: serve config denied")
		}, false, "sudo tailscale set --operator"},
		{func() { ts.serveErr = errors.New("something odd") }, false, "something odd"},
		{func() { ts.serveErr = nil }, true, "open https://box.t.ts.net"},
	}
	for i, s := range steps {
		s.change()
		logs.Reset()
		if got := h.Once(ctx); got != s.done || !strings.Contains(logs.String(), s.log) {
			t.Fatalf("step %d: done=%v log=%q", i, got, logs.String())
		}
	}
	if ts.sc.HTTPS[443] != "http://127.0.0.1:8787" || strings.Join(applied, ",") != "me@example.com" {
		t.Fatalf("serve %v users %v", ts.sc.HTTPS, applied)
	}
	if cfg, _ := config.Load(cfgPath); len(cfg.AllowedUsers) != 1 {
		t.Fatal("allowed_users not written")
	}
	// The same waiting message is logged once, not every minute.
	ts.stErr = errors.New("x")
	h.Once(ctx)
	logs.Reset()
	h.Once(ctx)
	if logs.Len() != 0 {
		t.Fatalf("repeated log: %q", logs.String())
	}
	// Already done: nothing to change, users kept.
	ts.stErr = nil
	applied = nil
	if !h.Once(ctx) || applied != nil {
		t.Fatal("second pass must change nothing")
	}
	// Port 8443 when 443 is taken; broken config is reported.
	ts2 := &healTS{st: ts.st, sc: tailscale.ServeConfig{HTTPS: map[int]string{443: "other"}}}
	logs.Reset()
	h2 := &Healer{Find: func() (tailscale.API, error) { return ts2, nil }, ConfigPath: cfgPath, Port: 8787, Logger: log.New(&logs, "", 0), SetUsers: func([]string) {}}
	if !h2.Once(ctx) || !strings.Contains(logs.String(), ":8443") {
		t.Fatalf("8443: %q", logs.String())
	}
	os.WriteFile(cfgPath, []byte("port = "), 0o644)
	if h2.Once(ctx) {
		t.Fatal("broken config")
	}
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(config.DefaultFile), 0o644)
	os.Chmod(cfgDir, 0o500)
	defer os.Chmod(cfgDir, 0o700)
	h3 := &Healer{Find: func() (tailscale.API, error) { return ts2, nil }, ConfigPath: filepath.Join(cfgDir, "config.toml"), Port: 8787, Logger: log.New(&logs, "", 0), SetUsers: func([]string) {}}
	if os.Getuid() != 0 && h3.Once(ctx) {
		t.Fatal("unwritable config")
	}
}

func TestHealerLoop(t *testing.T) {
	ts := &healTS{stErr: errors.New("x"), sc: tailscale.ServeConfig{HTTPS: map[int]string{}}}
	h := &Healer{Find: func() (tailscale.API, error) { return ts, nil }, Logger: log.New(&bytes.Buffer{}, "", 0)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	h.Loop(ctx, 5*time.Millisecond)
}

func TestSetAllowedUsersLive(t *testing.T) {
	r := newRig(t, nil)
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{"Host": "box.tail1.ts.net"}); res.code != 403 {
		t.Fatal(res.code)
	}
	r.srv.SetAllowedUsers([]string{"me@example.com"})
	if res := r.do(t, "GET", "/api/state", nil, map[string]string{"Host": "box.tail1.ts.net", IdentityHeader: "me@example.com"}); res.code != 200 {
		t.Fatal(res.code)
	}
}
