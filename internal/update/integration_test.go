//go:build integration

package update_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/testutil"
	"github.com/aist-dev/nostromo/internal/update"
)

func build(t *testing.T, version, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/aist-dev/nostromo/internal/version.Version="+version, "-o", out, "./cmd/agent")
	cmd.Dir = testutil.RepoRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, b)
	}
}

func publish(t *testing.T, root, tag, binary string) {
	t.Helper()
	dir := filepath.Join(root, "download", tag)
	os.MkdirAll(dir, 0o755)
	work := t.TempDir()
	data, _ := os.ReadFile(binary)
	os.WriteFile(filepath.Join(work, "agent"), data, 0o755)
	name := update.ArchiveName(goos(), goarch())
	if b, err := exec.Command("tar", "-czf", filepath.Join(dir, name), "-C", work, "agent").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	arch, _ := os.ReadFile(filepath.Join(dir, name))
	sum := sha256.Sum256(arch)
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644)
}

func goos() string {
	out, _ := exec.Command("go", "env", "GOOS").Output()
	return strings.TrimSpace(string(out))
}
func goarch() string {
	out, _ := exec.Command("go", "env", "GOARCH").Output()
	return strings.TrimSpace(string(out))
}

// Stage 9 acceptance, locally: 0.1.0 updates to 0.1.1 while an agent keeps running,
// and a broken 0.1.2 rolls itself back.
func TestUpdateKeepsAgentsRunning(t *testing.T) {
	home := t.TempDir()
	tm := testutil.Tmux(t, home)
	bins := t.TempDir()
	releases := t.TempDir()
	build(t, "0.1.0", filepath.Join(bins, "v010"))
	build(t, "0.1.1", filepath.Join(bins, "v011"))
	publish(t, releases, "v0.1.1", filepath.Join(bins, "v011"))
	// 0.1.2 claims to be 0.1.2 but its "setup --upgrade" fails.
	broken := filepath.Join(bins, "v012")
	os.WriteFile(broken, []byte("#!/bin/sh\ncase \"$1\" in version) echo 0.1.2;; setup) exit 4;; esac\n"), 0o755)
	publish(t, releases, "v0.1.2", broken)

	var latest atomic.Value
	latest.Store("v0.1.1")
	files := http.FileServer(http.Dir(releases))
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/aist-dev/nostromo/releases/latest" {
			fmt.Fprintf(w, `{"tag_name":%q}`, latest.Load().(string))
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer gh.Close()

	os.MkdirAll(filepath.Join(home, "bin"), 0o755)
	data, _ := os.ReadFile(filepath.Join(bins, "v010"))
	agent := filepath.Join(home, "bin", "agent")
	os.WriteFile(agent, data, 0o755)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(testutil.FakeConfig(testutil.FakeAgent(t), tm.Socket)), 0o644)
	userHome := t.TempDir()
	env := append(os.Environ(), "NOSTROMO_HOME="+home, "HOME="+userHome, "NOSTROMO_SERVICE=none", "TMUX=",
		"NOSTROMO_API_URL="+gh.URL, "NOSTROMO_DOWNLOAD_URL="+gh.URL)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(agent, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	id, err := run("--nostr-detach")
	if err != nil {
		t.Fatalf("start chat: %v %s", err, id)
	}
	testutil.Eventually(t, 5*time.Second, "agent start", func() bool { return len(testutil.FakeRuns(home)) == 1 })
	pid := testutil.FakeRuns(home)[0].PID

	if out, err := run("update", "--check"); err != nil || !strings.Contains(out, "0.1.1 is available") {
		t.Fatalf("check: %v %s", err, out)
	}
	if out, err := run("update"); err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if v, _ := run("version"); strings.TrimSpace(v) != "0.1.1" {
		t.Fatalf("installed version %q", v)
	}
	if out, _ := run("update"); !strings.Contains(out, "already up to date (0.1.1)") {
		t.Fatalf("second update: %s", out)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("the agent was interrupted by the update")
	}
	if out, _ := run("ls"); !strings.Contains(out, "running") {
		t.Fatalf("chat after update: %s", out)
	}

	latest.Store("v0.1.2")
	out, err := run("update")
	if err == nil || !strings.Contains(out, "rolled back to 0.1.1") {
		t.Fatalf("broken release must roll back: %v\n%s", err, out)
	}
	if v, _ := run("version"); strings.TrimSpace(v) != "0.1.1" {
		t.Fatalf("after rollback: %q", v)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("the agent was interrupted by the rollback")
	}
}
