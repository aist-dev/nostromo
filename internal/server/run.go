package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aist-dev/nostromo/internal/cli"
	"github.com/aist-dev/nostromo/internal/tailscale"
	"github.com/aist-dev/nostromo/internal/version"
)

// Extras lets other packages plug features into the server without import cycles.
type Extras struct {
	Static          http.Handler
	UpdateAvailable func() string
	StartUpdate     func() error
	// Background runs for the lifetime of the server (update checks).
	Background func(ctx context.Context, logger *log.Logger)
}

// Command implements `agent serve`.
func Command(extras func(app *cli.App) Extras) cli.Command {
	return func(ctx context.Context, args []string, env cli.Env) int {
		fs := flag.NewFlagSet("agent serve", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		listen := fs.String("listen", "", "address to listen on (default 127.0.0.1:<port>)")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		app, err := cli.NewApp(env)
		if err != nil {
			fmt.Fprintf(env.Stderr, "agent serve: %v\n", err)
			return 1
		}
		var ex Extras
		if extras != nil {
			ex = extras(app)
		}
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := Run(ctx, app, *listen, ex, env.Stderr); err != nil {
			fmt.Fprintf(env.Stderr, "agent serve: %v\n", err)
			return 1
		}
		return 0
	}
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, app *cli.App, listen string, ex Extras, stderr io.Writer) error {
	logFile, err := OpenRotating(app.Paths.ServerLog(), 5<<20, 2)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(io.MultiWriter(logFile, stderr), "", log.LstdFlags)

	if listen == "" {
		listen = "127.0.0.1:" + strconv.Itoa(app.Config.Port)
	} else if !isLoopback(listen) {
		logger.Printf("WARNING: listening on %s, not only on loopback; anyone who can reach it gets a terminal", listen)
	}
	if len(app.Config.AllowedUsers) == 0 {
		logger.Printf("WARNING: allowed_users is empty: every device in the tailnet can use this server; run \"agent setup\" or set allowed_users in %s", app.Paths.Config())
	}
	if err := app.Chats.EnsureTmuxConf(); err != nil {
		return err
	}

	opts := Options{
		Chats: app.Chats, Config: app.Config, Version: version.Version,
		Hostname: shortHostname(), Home: app.Env.Getenv("HOME"), Logger: logger,
		Static: ex.Static, UpdateAvailable: ex.UpdateAvailable, StartUpdate: ex.StartUpdate,
	}
	if ts, err := tailscale.Find(); err == nil {
		opts.TailscaleStatus = ts.Status
		if st, err := ts.Status(ctx); err == nil && st.HostName != "" {
			opts.Hostname = st.HostName
		}
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          logger,
	}
	if ex.Background != nil {
		go ex.Background(ctx, logger)
	}
	logger.Printf("nostromo %s listening on http://%s", version.Version, ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Printf("shutting down; chats keep running in tmux")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown()
	if err := httpSrv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
