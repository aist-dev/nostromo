package server

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tmux"
	"github.com/aist-dev/nostromo/internal/tmux/faketmux"
)

func testEnv(t *testing.T) (cli.Env, string, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	vars := map[string]string{"HOME": home, "NOSTROMO_HOME": filepath.Join(home, ".nostromo")}
	errb := &bytes.Buffer{}
	env := cli.DefaultEnv()
	env.Stdout, env.Stderr = &bytes.Buffer{}, errb
	env.Getenv = func(k string) string { return vars[k] }
	env.NewTmux = func(config.Config, config.Paths) tmux.API { return faketmux.New() }
	// Never let a test server touch the real tailscale serve config of this machine.
	os.MkdirAll(filepath.Join(home, ".nostromo"), 0o755)
	os.WriteFile(filepath.Join(home, ".nostromo", "config.toml"), []byte(safeConfig), 0o644)
	return env, home, errb
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRunServesAndShutsDown(t *testing.T) {
	env, home, errb := testEnv(t)
	port := freePort(t)
	os.MkdirAll(filepath.Join(home, ".nostromo"), 0o755)
	os.WriteFile(filepath.Join(home, ".nostromo", "config.toml"), []byte(safeConfig+"port = "+itoa(port)+"\n"), 0o644)
	app, err := cli.NewApp(env)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bg := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, app, "", Extras{Background: func(context.Context, *log.Logger) { close(bg) }}, errb)
	}()
	url := "http://127.0.0.1:" + itoa(port) + "/healthz"
	var res *http.Response
	for i := 0; i < 100; i++ {
		if res, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("server not reachable: %v", err)
	}
	res.Body.Close()
	<-bg
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	logData, _ := os.ReadFile(filepath.Join(home, ".nostromo", "logs", "server.log"))
	for _, want := range []string{"only this machine can use the UI", "listening on", "shutting down"} {
		if !strings.Contains(string(logData), want) {
			t.Errorf("log lacks %q:\n%s", want, logData)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".nostromo", "tmux.conf")); err != nil {
		t.Fatal("tmux.conf must be written at start")
	}
}

func TestRunErrors(t *testing.T) {
	env, home, errb := testEnv(t)
	app, _ := cli.NewApp(env)
	// Listening on a non-loopback address warns; a busy port fails.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if err := Run(context.Background(), app, l.Addr().String(), Extras{}, errb); err == nil {
		t.Fatal("busy port must fail")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(cancelled, app, "0.0.0.0:0", Extras{}, errb); err != nil {
		t.Fatalf("cancelled run: %v", err)
	}
	if !strings.Contains(errb.String(), "not only on loopback") {
		t.Fatalf("no warning: %s", errb)
	}
	// Unwritable log directory.
	os.RemoveAll(filepath.Join(home, ".nostromo", "logs"))
	os.WriteFile(filepath.Join(home, ".nostromo", "logs"), nil, 0o644)
	if err := Run(context.Background(), app, "127.0.0.1:0", Extras{}, errb); err == nil {
		t.Fatal("log open failure must fail")
	}
	os.Remove(filepath.Join(home, ".nostromo", "logs"))
	os.MkdirAll(filepath.Join(home, ".nostromo", "tmux.conf", "x"), 0o755)
	app.Paths.Home = filepath.Join(home, ".nostromo")
	os.WriteFile(filepath.Join(home, "file"), nil, 0o644)
	app.Chats.Paths.Home = filepath.Join(home, "file")
	if err := Run(context.Background(), app, "127.0.0.1:0", Extras{}, errb); err == nil {
		t.Fatal("tmux.conf failure must fail")
	}
}

func TestCommand(t *testing.T) {
	env, home, errb := testEnv(t)
	cmd := Command(func(*cli.App) Extras { return Extras{} })
	if code := cmd(context.Background(), []string{"--bogus"}, env); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	os.MkdirAll(filepath.Join(home, ".nostromo"), 0o755)
	os.WriteFile(filepath.Join(home, ".nostromo", "config.toml"), []byte("port = "), 0o644)
	if code := cmd(context.Background(), nil, env); code != 1 {
		t.Fatalf("bad config: %d", code)
	}
	os.WriteFile(filepath.Join(home, ".nostromo", "config.toml"), []byte(safeConfig), 0o644)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if code := cmd(context.Background(), []string{"--listen", l.Addr().String()}, env); code != 1 || !strings.Contains(errb.String(), "address already in use") {
		t.Fatalf("listen failure: %d %s", code, errb)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if code := Command(nil)(ctx, []string{"--listen", "127.0.0.1:0"}, env); code != 0 {
		t.Fatalf("clean shutdown: %d %s", code, errb)
	}
}

const safeConfig = "tailscale_serve = false\n"

func itoa(i int) string { return strconv.Itoa(i) }
