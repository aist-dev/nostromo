// Package testutil holds helpers for integration tests that drive real tmux.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/tmux"
)

// RepoRoot returns the repository root directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
	buildOut  []byte
)

// FakeAgent builds testdata/fakeagent once per test binary and returns its path.
func FakeAgent(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "nostromo-fakeagent-")
		if buildErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, "fakeagent"), "./testdata/fakeagent")
		cmd.Dir = RepoRoot()
		buildOut, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("build fakeagent: %v\n%s", buildErr, buildOut)
	}
	return filepath.Join(buildDir, "fakeagent")
}

// RandomName returns a short random hex string.
func RandomName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Tmux returns a client on a private socket with the nostromo tmux config.
// The tmux server is killed when the test ends. The test is skipped without tmux.
func Tmux(t testing.TB, home string) *tmux.Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	conf := filepath.Join(home, "tmux.conf")
	if err := os.WriteFile(conf, []byte(tmux.Conf), 0o644); err != nil {
		t.Fatal(err)
	}
	c := tmux.New("nostromo-test-"+RandomName(), conf)
	t.Cleanup(func() { c.KillServer(context.Background()) })
	return c
}

// Eventually polls fn until it returns true or the timeout passes.
func Eventually(t testing.TB, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// FakeRun is one start of the fake agent.
type FakeRun struct {
	Args []string          `json:"args"`
	Cwd  string            `json:"cwd"`
	PID  int               `json:"pid"`
	Env  map[string]string `json:"env"`
}

// FakeRuns reads every fake agent start logged under home.
func FakeRuns(home string) []FakeRun {
	data, err := os.ReadFile(filepath.Join(home, "fakeagent.log"))
	if err != nil {
		return nil
	}
	var runs []FakeRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r FakeRun
		if json.Unmarshal([]byte(line), &r) == nil {
			runs = append(runs, r)
		}
	}
	return runs
}

// CapturePane returns the visible text of a session's pane.
func CapturePane(c *tmux.Client, session string) string {
	args := append(c.BaseArgs(), "capture-pane", "-p", "-t", "="+session+":")
	out, _ := exec.Command("tmux", args...).Output()
	return string(out)
}

// SendKeys types text into a session followed by Enter.
func SendKeys(c *tmux.Client, session, text string) error {
	args := append(c.BaseArgs(), "send-keys", "-t", "="+session+":", "-l", text)
	if err := exec.Command("tmux", args...).Run(); err != nil {
		return err
	}
	args = append(c.BaseArgs(), "send-keys", "-t", "="+session+":", "Enter")
	return exec.Command("tmux", args...).Run()
}
