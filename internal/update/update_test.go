package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/config"
)

func TestSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "v1.2.3", 0}, {"1.2.4", "1.2.3", 1}, {"1.3.0", "1.2.9", 1}, {"2.0.0", "1.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1}, {"1.0.0", "1.0.0-rc.1", 1}, {"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-rc.2", "1.0.0-rc.1", 1}, {"1.0.0+build", "1.0.0", 0}, {"0.9.0", "1.0.0", -1},
	}
	for _, c := range cases {
		a, err1 := ParseSemver(c.a)
		b, err2 := ParseSemver(c.b)
		if err1 != nil || err2 != nil || a.Compare(b) != c.want {
			t.Errorf("%s vs %s: %d", c.a, c.b, a.Compare(b))
		}
	}
	for _, bad := range []string{"", "1.2", "a.b.c", "1.2.-3", "dev"} {
		if _, err := ParseSemver(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
	if !Newer("v1.0.1", "1.0.0") || Newer("1.0.0", "1.0.0") || Newer("junk", "1.0.0") || !Newer("0.0.1", "dev") {
		t.Fatal("Newer")
	}
}

func TestGoreleaserContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `name_template: "agent_{{ .Os }}_{{ .Arch }}"`) || !strings.Contains(s, "name_template: "+ChecksumsFile) {
		t.Fatal(".goreleaser.yaml no longer matches ArchiveName and ChecksumsFile")
	}
	if ArchiveName("linux", "amd64") != "agent_linux_amd64.tar.gz" {
		t.Fatal("ArchiveName")
	}
	sh, _ := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if !strings.Contains(string(sh), `file="agent_${os}_${arch}.tar.gz"`) || !strings.Contains(string(sh), "checksums.txt") {
		t.Fatal("install.sh no longer matches the release asset names")
	}
}

// fakeBinary is a shell script standing in for a released agent binary.
func fakeBinary(version string, setupFails bool) []byte {
	setup := "exit 0"
	if setupFails {
		setup = "echo setup broke; exit 3"
	}
	return []byte(fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  version) echo %s ;;\n  setup) %s ;;\nesac\n", version, setup))
}

func archive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type release struct {
	archive   []byte
	checksums string
}

type fakeGitHub struct {
	srv      *httptest.Server
	latest   string
	releases map[string]release
	auth     string
}

func newGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{releases: map[string]release{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			g.auth = r.Header.Get("Authorization")
			if g.latest == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name":%q}`, g.latest)
		case strings.HasPrefix(r.URL.Path, "/dl/download/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/download/"), "/")
			rel, ok := g.releases[parts[0]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if parts[1] == ChecksumsFile {
				io.WriteString(w, rel.checksums)
			} else {
				w.Write(rel.archive)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) publish(t *testing.T, tag string, bin []byte) {
	a := archive(t, "agent", bin)
	sum := sha256.Sum256(a)
	g.releases[tag] = release{archive: a, checksums: hex.EncodeToString(sum[:]) + "  " + ArchiveName("linux", "amd64") + "\n"}
}

type fakeService struct {
	restarts int
	err      error
}

func (f *fakeService) Restart(context.Context) error { f.restarts++; return f.err }

type rig struct {
	u   *Updater
	gh  *fakeGitHub
	svc *fakeService
	out *bytes.Buffer
	up  bool // whether the restarted server answers
}

func newRig(t *testing.T) *rig {
	t.Helper()
	paths := config.Paths{Home: t.TempDir()}
	os.MkdirAll(paths.Bin(), 0o755)
	os.WriteFile(paths.Binary(), fakeBinary("1.0.0", false), 0o755)
	r := &rig{gh: newGitHub(t), svc: &fakeService{}, out: &bytes.Buffer{}, up: true}
	r.u = &Updater{
		Paths: paths, Repo: "o/r", APIBase: r.gh.srv.URL, DownloadBase: r.gh.srv.URL + "/dl",
		GOOS: "linux", GOARCH: "amd64", Current: "1.0.0", Out: r.out, Service: r.svc,
		RunBinary: ExecBinary, WaitTimeout: 300 * time.Millisecond,
		Health: func(ctx context.Context, port int) (string, error) {
			if !r.up {
				return "", errors.New("refused")
			}
			return ExecBinary(ctx, paths.Binary(), "version")
		},
	}
	return r
}

func (r *rig) installed(t *testing.T) string {
	out, _ := ExecBinary(context.Background(), r.u.Paths.Binary(), "version")
	return strings.TrimSpace(out)
}

func TestNoNewVersion(t *testing.T) {
	r := newRig(t)
	r.gh.latest = "v1.0.0"
	if err := r.u.Run(context.Background(), Options{}); err != nil || !strings.Contains(r.out.String(), "already up to date") {
		t.Fatal(err, r.out)
	}
	r.u.Token = "secret"
	r.u.Run(context.Background(), Options{Check: true})
	if r.gh.auth != "Bearer secret" {
		t.Fatal("token not sent to the API")
	}
}

func TestSuccessfulUpdateAndRollback(t *testing.T) {
	r := newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	if err := r.u.Run(context.Background(), Options{Check: true}); err != nil || !strings.Contains(r.out.String(), "1.1.0 is available") {
		t.Fatal(err, r.out)
	}
	if r.installed(t) != "1.0.0" {
		t.Fatal("--check must not install")
	}
	if err := r.u.Run(context.Background(), Options{}); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.installed(t) != "1.1.0" || r.svc.restarts != 1 {
		t.Fatalf("installed %s restarts %d", r.installed(t), r.svc.restarts)
	}
	if prev, _ := ExecBinary(context.Background(), r.u.Paths.PrevBinary(), "version"); strings.TrimSpace(prev) != "1.0.0" {
		t.Fatal("previous binary not kept")
	}
	if _, err := os.Stat(r.u.Paths.NewBinary()); !os.IsNotExist(err) {
		t.Fatal("agent.new left behind")
	}
	// Manual rollback.
	err := r.u.Run(context.Background(), Options{Rollback: true})
	if err == nil || !strings.Contains(err.Error(), "rolled back to 1.0.0") || r.installed(t) != "1.0.0" {
		t.Fatalf("rollback: %v, installed %s", err, r.installed(t))
	}
}

func TestPinnedVersion(t *testing.T) {
	r := newRig(t)
	r.gh.publish(t, "v0.9.0", fakeBinary("0.9.0", false))
	if err := r.u.Run(context.Background(), Options{Version: "0.9.0"}); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.installed(t) != "0.9.0" {
		t.Fatal("downgrade to a pinned version")
	}
	if err := r.u.Run(context.Background(), Options{Version: "garbage"}); err == nil {
		t.Fatal("bad version")
	}
	if err := r.u.Run(context.Background(), Options{Version: "v5.0.0"}); err == nil {
		t.Fatal("missing release")
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]func(r *rig){
		"checksum mismatch": func(r *rig) {
			rel := r.gh.releases["v1.1.0"]
			rel.archive = append(rel.archive, 'x')
			r.gh.releases["v1.1.0"] = rel
		},
		"reports version": func(r *rig) { r.gh.publish(t, "v1.1.0", fakeBinary("6.6.6", false)) },
		"not listed": func(r *rig) {
			rel := r.gh.releases["v1.1.0"]
			rel.checksums = "abc other.tar.gz\n"
			r.gh.releases["v1.1.0"] = rel
		},
		"does not contain": func(r *rig) {
			a := archive(t, "something-else", []byte("x"))
			sum := sha256.Sum256(a)
			r.gh.releases["v1.1.0"] = release{archive: a, checksums: hex.EncodeToString(sum[:]) + " *" + ArchiveName("linux", "amd64") + "\n"}
		},
		"gzip": func(r *rig) {
			a := []byte("not a gzip")
			sum := sha256.Sum256(a)
			r.gh.releases["v1.1.0"] = release{archive: a, checksums: hex.EncodeToString(sum[:]) + " " + ArchiveName("linux", "amd64") + "\n"}
		},
	}
	for want, breakIt := range cases {
		r := newRig(t)
		r.gh.latest = "v1.1.0"
		r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
		breakIt(r)
		err := r.u.Run(context.Background(), Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
		if r.installed(t) != "1.0.0" || r.svc.restarts != 0 {
			t.Errorf("%s: binary changed or server restarted", want)
		}
	}
}

func TestAutomaticRollback(t *testing.T) {
	// The new server never comes up: roll back.
	r := newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	calls := 0
	r.u.Health = func(ctx context.Context, port int) (string, error) {
		calls++
		v, _ := ExecBinary(ctx, r.u.Paths.Binary(), "version")
		if strings.TrimSpace(v) == "1.1.0" {
			return "", errors.New("crash loop")
		}
		return v, nil
	}
	err := r.u.Run(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "rolled back to 1.0.0") || r.installed(t) != "1.0.0" || r.svc.restarts != 2 {
		t.Fatalf("%v installed %s restarts %d", err, r.installed(t), r.svc.restarts)
	}

	// setup --upgrade of the new binary fails: roll back.
	r = newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", true))
	err = r.u.Run(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "setup --upgrade failed") || r.installed(t) != "1.0.0" {
		t.Fatalf("%v installed %s", err, r.installed(t))
	}

	// Neither version comes up.
	r = newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	r.up = false
	if err := r.u.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "did not come up either") {
		t.Fatal(err)
	}

	// Restart itself fails.
	r = newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	r.svc.err = errors.New("no systemd")
	if err := r.u.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "restart the service") {
		t.Fatal(err)
	}

	// Wrong version keeps answering until the deadline.
	r = newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	r.u.Health = func(context.Context, int) (string, error) { return "0.0.1", nil }
	if err := r.u.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "expected 1.1.0") {
		t.Fatal(err)
	}
}

func TestRollbackWithoutPrevious(t *testing.T) {
	r := newRig(t)
	if err := r.u.Run(context.Background(), Options{Rollback: true}); err == nil || !strings.Contains(err.Error(), "no previous version") {
		t.Fatal(err)
	}
	// Without a service there is nothing to restart.
	r.u.Service = nil
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	if err := r.u.Run(context.Background(), Options{}); err != nil || r.installed(t) != "1.1.0" {
		t.Fatal(err)
	}
}

func TestLatestErrors(t *testing.T) {
	r := newRig(t)
	if _, err := r.u.Latest(context.Background()); err == nil {
		t.Fatal("404")
	}
	if err := r.u.Run(context.Background(), Options{}); err == nil {
		t.Fatal("run with no release")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "big") {
			w.Write(bytes.Repeat([]byte("x"), 2<<20))
			return
		}
		io.WriteString(w, `{"tag_name": "nightly"}`)
	}))
	defer bad.Close()
	r.u.APIBase = bad.URL
	if _, err := r.u.Latest(context.Background()); err == nil {
		t.Fatal("non-semver tag")
	}
	if _, err := r.u.download(context.Background(), bad.URL+"/big", 1<<20); err == nil {
		t.Fatal("size limit")
	}
	bad2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{") }))
	defer bad2.Close()
	r.u.APIBase = bad2.URL
	if _, err := r.u.Latest(context.Background()); err == nil {
		t.Fatal("bad json")
	}
	r.u.APIBase = "http://127.0.0.1:1"
	if _, err := r.u.Latest(context.Background()); err == nil {
		t.Fatal("refused")
	}
	if _, err := r.u.get(context.Background(), "::bad", ""); err == nil {
		t.Fatal("bad url")
	}
	if (&Updater{}).client() == nil {
		t.Fatal("default client")
	}
}

func TestInstallFileErrors(t *testing.T) {
	r := newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	os.Remove(r.u.Paths.Binary())
	if err := r.u.Run(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "back up the current binary") {
		t.Fatal(err)
	}
	r = newRig(t)
	r.gh.latest = "v1.1.0"
	r.gh.publish(t, "v1.1.0", fakeBinary("1.1.0", false))
	os.MkdirAll(filepath.Join(r.u.Paths.NewBinary(), "x"), 0o755)
	if err := r.u.Run(context.Background(), Options{}); err == nil {
		t.Fatal("agent.new blocked")
	}
	r = newRig(t)
	os.WriteFile(r.u.Paths.PrevBinary(), fakeBinary("0.5.0", false), 0o755)
	os.Remove(r.u.Paths.Binary())
	if err := r.u.Run(context.Background(), Options{Rollback: true}); err == nil || !strings.Contains(err.Error(), "rolled back to 0.5.0") {
		t.Fatal(err)
	}
	r = newRig(t)
	os.MkdirAll(filepath.Join(r.u.Paths.PrevBinary(), "x"), 0o755)
	if err := r.u.Run(context.Background(), Options{Rollback: true}); err == nil {
		t.Fatal("unreadable previous binary")
	}
	if err := copyFile(filepath.Join(r.u.Paths.Home, "none"), filepath.Join(r.u.Paths.Home, "x")); err == nil {
		t.Fatal("copy missing")
	}
	if _, err := ExtractBinary(func() []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte("not a tar archive at all, definitely not"))
		gz.Close()
		return buf.Bytes()
	}()); err == nil {
		t.Fatal("bad tar")
	}
}

func TestExecBinary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	os.WriteFile(p, []byte("#!/bin/sh\necho out; echo err >&2; exit 2\n"), 0o755)
	out, err := ExecBinary(context.Background(), p)
	if err == nil || !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Fatal(out, err)
	}
	if _, err := ExecBinary(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing")
	}
}

func TestChecker(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	calls := 0
	latest := "v1.1.0"
	c := &Checker{Paths: paths, Current: "1.0.0", Enabled: true, Now: func() time.Time { return now },
		Latest: func(context.Context) (string, error) { calls++; return latest, nil }}
	logger := log.New(io.Discard, "", 0)
	if c.Available() != "" {
		t.Fatal("nothing checked yet")
	}
	if !c.CheckOnce(context.Background(), logger) || c.Available() != "1.1.0" || calls != 1 {
		t.Fatal("first check")
	}
	c.CheckOnce(context.Background(), logger)
	if calls != 1 {
		t.Fatal("the cache must limit checks to once a day")
	}
	now = now.Add(25 * time.Hour)
	latest = "v1.0.0"
	if c.CheckOnce(context.Background(), logger) || c.Available() != "" || calls != 2 {
		t.Fatal("second day")
	}
	c2 := &Checker{Paths: paths, Current: "0.1.0"}
	if c2.Available() != "1.0.0" {
		t.Fatal("Available must read the cache file")
	}
	failing := &Checker{Paths: config.Paths{Home: t.TempDir()}, Latest: func(context.Context) (string, error) { return "", errors.New("offline") }}
	if failing.CheckOnce(context.Background(), logger) {
		t.Fatal("failed check")
	}
	if (&Checker{}).now().IsZero() {
		t.Fatal("default clock")
	}
}

func TestCheckerLoop(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	(&Checker{Enabled: false}).Loop(context.Background(), logger, 0)

	spawned := make(chan struct{}, 1)
	c := &Checker{Paths: config.Paths{Home: t.TempDir()}, Current: "1.0.0", Enabled: true, Auto: true,
		Latest: func(context.Context) (string, error) { return "v2.0.0", nil },
		Spawn:  func() error { spawned <- struct{}{}; return errors.New("spawn failed is only logged") }}
	c.Loop(context.Background(), logger, time.Millisecond)
	select {
	case <-spawned:
	default:
		t.Fatal("auto update did not spawn")
	}

	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	c3 := &Checker{Paths: config.Paths{Home: t.TempDir()}, Current: "1.0.0", Enabled: true, Interval: time.Nanosecond,
		Latest: func(context.Context) (string, error) {
			n++
			if n == 3 {
				cancel()
			}
			return "v1.0.0", nil
		}}
	c3.Loop(ctx, logger, time.Millisecond)
	if n < 3 {
		t.Fatal("loop must keep checking")
	}
}

func TestSpawnDetached(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	os.MkdirAll(paths.Bin(), 0o755)
	marker := filepath.Join(paths.Home, "ran")
	os.WriteFile(paths.Binary(), []byte("#!/bin/sh\necho \"$1\" > "+marker+"\n"), 0o755)
	if err := SpawnDetached(paths); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == "update" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("updater did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Remove(paths.Binary())
	if err := SpawnDetached(paths); err == nil {
		t.Fatal("missing binary")
	}
	os.RemoveAll(paths.Logs())
	os.WriteFile(paths.Logs(), nil, 0o644)
	if err := SpawnDetached(paths); err == nil {
		t.Fatal("unwritable log")
	}
}

func TestCommand(t *testing.T) {
	r := newRig(t)
	r.gh.latest = "v1.0.0"
	env := cli.Env{Stdout: r.out, Stderr: r.out}
	cmd := Command(func(cli.Env) (*Updater, error) { return r.u, nil })
	if code := cmd(context.Background(), []string{"--bogus"}, env); code != 2 {
		t.Fatal("flag")
	}
	if code := cmd(context.Background(), []string{"--check"}, env); code != 0 {
		t.Fatal("check")
	}
	if code := cmd(context.Background(), []string{"--rollback"}, env); code != 1 {
		t.Fatal("rollback without previous")
	}
	if code := Command(func(cli.Env) (*Updater, error) { return nil, errors.New("x") })(context.Background(), nil, env); code != 1 {
		t.Fatal("build error")
	}
	home := t.TempDir()
	vars := map[string]string{"HOME": home, "NOSTROMO_HOME": filepath.Join(home, ".nostromo"), TokenEnv: ""}
	denv := cli.DefaultEnv()
	denv.Getenv = func(k string) string { return vars[k] }
	u, err := NewUpdater(denv)
	if err != nil || u.Repo != "aist-dev/nostromo" || u.DownloadBase != "https://github.com/aist-dev/nostromo/releases" || u.APIBase != "https://api.github.com" {
		t.Fatalf("%+v %v", u, err)
	}
	vars[APIURLEnv], vars[DownloadURLEnv] = "http://api.local", "http://dl.local"
	if u, _ := NewUpdater(denv); u.APIBase != "http://api.local" || u.DownloadBase != "http://dl.local" {
		t.Fatalf("overrides: %+v", u)
	}
	os.MkdirAll(filepath.Join(home, ".nostromo"), 0o755)
	os.WriteFile(filepath.Join(home, ".nostromo", "config.toml"), []byte("port = "), 0o644)
	if _, err := NewUpdater(denv); err == nil {
		t.Fatal("bad config")
	}
	vars["HOME"], vars["NOSTROMO_HOME"] = "", ""
	if _, err := NewUpdater(denv); err == nil {
		t.Fatal("no home")
	}
	if Command(nil) == nil {
		t.Fatal("default builder")
	}
}
