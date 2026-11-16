package tailscale

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	out    map[string]string
	stderr map[string]string
	errs   map[string]bool
	calls  []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, name+" "+key)
	if f.errs[key] {
		return []byte(f.out[key]), []byte(f.stderr[key]), errors.New("exit status 1")
	}
	return []byte(f.out[key]), nil, nil
}

const statusJSON = `{
  "BackendState": "Running",
  "Self": {"DNSName": "box.tail1234.ts.net.", "HostName": "box", "UserID": 1234567890123456},
  "User": {"1234567890123456": {"LoginName": "me@example.com"}},
  "CurrentTailnet": {"MagicDNSEnabled": true},
  "CertDomains": ["box.tail1234.ts.net"]
}`

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus([]byte(statusJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running() || s.DNSName != "box.tail1234.ts.net" || s.HostName != "box" || s.Login != "me@example.com" ||
		!s.MagicDNSEnabled || len(s.CertDomains) != 1 {
		t.Fatalf("%+v", s)
	}
	s, err = ParseStatus([]byte(`{"BackendState":"NeedsLogin","Self":{}}`))
	if err != nil || s.Running() || s.Login != "" || s.MagicDNSEnabled {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := ParseStatus([]byte("nope")); err == nil {
		t.Fatal("bad json")
	}
}

func TestParseServe(t *testing.T) {
	sc, err := ParseServe([]byte(`{
	  "TCP": {"443": {"HTTPS": true}, "8443": {"HTTPS": true}},
	  "Web": {
	    "box.tail1234.ts.net:443": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:8787"}}},
	    "box.tail1234.ts.net:8443": {"Handlers": {"/files": {"Path": "/srv"}}}
	  },
	  "AllowFunnel": {"box.tail1234.ts.net:443": true, "box.tail1234.ts.net:10000": false}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.HTTPS[443] != "http://127.0.0.1:8787" || len(sc.HTTPS) != 1 || len(sc.Funnel) != 1 || sc.Funnel[0] != 443 {
		t.Fatalf("%+v", sc)
	}
	for _, empty := range []string{"", "{}", "  \n"} {
		sc, err := ParseServe([]byte(empty))
		if err != nil || len(sc.HTTPS) != 0 {
			t.Fatalf("%q: %+v %v", empty, sc, err)
		}
	}
	if _, err := ParseServe([]byte("{")); err == nil {
		t.Fatal("bad json")
	}
	if portOf("noport") != 0 {
		t.Fatal("portOf")
	}
}

func TestClient(t *testing.T) {
	r := &fakeRunner{out: map[string]string{
		"status --json":       statusJSON,
		"serve status --json": `{"Web":{"box:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:1"}}}}}`,
	}, stderr: map[string]string{}, errs: map[string]bool{}}
	c := &Client{Bin: "ts", Run: r}
	ctx := context.Background()
	if s, err := c.Status(ctx); err != nil || s.Login != "me@example.com" {
		t.Fatalf("%+v %v", s, err)
	}
	if sc, err := c.ServeStatus(ctx); err != nil || sc.HTTPS[443] != "http://127.0.0.1:1" {
		t.Fatalf("%+v %v", sc, err)
	}
	if err := c.Serve(ctx, 443, "http://127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	if err := c.ServeOff(ctx, 443); err != nil {
		t.Fatal(err)
	}
	want := []string{"ts status --json", "ts serve status --json", "ts serve --bg --https=443 http://127.0.0.1:8787", "ts serve --https=443 off"}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls: %v", r.calls)
	}

	r.errs["status --json"] = true
	r.stderr["status --json"] = "Tailscale is stopped."
	if s, err := c.Status(ctx); err != nil || !s.Running() {
		t.Fatalf("status with output still parses: %+v %v", s, err)
	}
	r.out["status --json"] = ""
	if _, err := c.Status(ctx); err == nil || !strings.Contains(err.Error(), "Tailscale is stopped") {
		t.Fatalf("status error: %v", err)
	}
	r.errs["serve status --json"] = true
	if _, err := c.ServeStatus(ctx); err == nil {
		t.Fatal("serve status error")
	}
	r.errs["serve --https=443 off"] = true
	if err := c.ServeOff(ctx, 443); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error text falls back to the exit status: %v", err)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", dir)
	if c, err := Find(); err != nil || c.Bin != filepath.Join(dir, "tailscale") {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("PATH", t.TempDir())
	c, err := Find()
	if err == nil && c.Bin != MacAppBinary {
		t.Fatalf("unexpected: %+v", c)
	}
}

type slowRunner struct{}

func (slowRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func TestTimeout(t *testing.T) {
	old := Timeout
	Timeout = 50 * time.Millisecond
	defer func() { Timeout = old }()
	c := &Client{Bin: "ts", Run: slowRunner{}}
	if err := c.Serve(context.Background(), 443, "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatal(err)
	}
}

type fakeAPI struct {
	sc       ServeConfig
	statErr  error
	serveErr error
	served   []int
}

func (f *fakeAPI) Status(context.Context) (Status, error) { return Status{}, nil }
func (f *fakeAPI) ServeStatus(context.Context) (ServeConfig, error) {
	return f.sc, f.statErr
}
func (f *fakeAPI) Serve(_ context.Context, p int, target string) error {
	if f.serveErr != nil {
		return f.serveErr
	}
	f.served = append(f.served, p)
	f.sc.HTTPS[p] = target
	return nil
}
func (f *fakeAPI) ServeOff(context.Context, int) error { return nil }

func TestEnsure(t *testing.T) {
	ctx := context.Background()
	const target = "http://127.0.0.1:8787"
	f := &fakeAPI{sc: ServeConfig{HTTPS: map[int]string{}}}
	if p, changed, err := Ensure(ctx, f, target); p != 443 || !changed || err != nil {
		t.Fatal(p, changed, err)
	}
	if p, changed, err := Ensure(ctx, f, target); p != 443 || changed || err != nil {
		t.Fatal("already configured", p, changed, err)
	}
	f = &fakeAPI{sc: ServeConfig{HTTPS: map[int]string{443: "http://127.0.0.1:3000"}}}
	if p, _, _ := Ensure(ctx, f, target); p != 8443 {
		t.Fatal("443 taken", p)
	}
	f = &fakeAPI{sc: ServeConfig{HTTPS: map[int]string{443: "x", 8443: "y"}}}
	if _, _, err := Ensure(ctx, f, target); !errors.Is(err, ErrPortsTaken) {
		t.Fatal(err)
	}
	f = &fakeAPI{sc: ServeConfig{HTTPS: map[int]string{}}, serveErr: errors.New("access denied")}
	if _, _, err := Ensure(ctx, f, target); err == nil {
		t.Fatal("serve error")
	}
	f = &fakeAPI{statErr: errors.New("x")}
	if _, _, err := Ensure(ctx, f, target); err == nil {
		t.Fatal("status error")
	}
	if !(ServeConfig{Funnel: []int{443}}).FunnelOn(443) || (ServeConfig{}).FunnelOn(443) {
		t.Fatal("FunnelOn")
	}
	ready := Status{BackendState: "Running", MagicDNSEnabled: true, CertDomains: []string{"x"}}
	if !ready.HTTPSReady() || (Status{BackendState: "Running", MagicDNSEnabled: true}).HTTPSReady() {
		t.Fatal("HTTPSReady")
	}
}
