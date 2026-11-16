package server

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/tailscale"
)

// Healer finishes the HTTPS setup when setup could not: as soon as the tailnet
// has HTTPS certificates and this user may configure tailscale serve, it proxies
// https to the server and puts the user's login into allowed_users. Nobody has to
// run setup again after fixing Tailscale by hand.
type Healer struct {
	// Find returns the tailscale CLI; it is looked up on every check, so
	// Tailscale installed after nostromo is picked up too.
	Find       func() (tailscale.API, error)
	ConfigPath string
	Port       int
	Logger     *log.Logger
	// SetUsers applies a new allowed_users list to the running server.
	SetUsers func([]string)
	lastErr  string
}

func (h *Healer) report(msg string) {
	if msg != h.lastErr && msg != "" {
		h.Logger.Printf("https setup: %s", msg)
	}
	h.lastErr = msg
}

// Once runs one check. It returns true when the HTTPS address is fully set up.
func (h *Healer) Once(ctx context.Context) bool {
	ts, err := h.Find()
	if err != nil {
		h.report("waiting for Tailscale to be installed")
		return false
	}
	st, err := ts.Status(ctx)
	if err != nil || !st.Running() {
		h.report("waiting for Tailscale to run and log in")
		return false
	}
	if !st.HTTPSReady() {
		h.report("waiting for MagicDNS and HTTPS certificates (https://login.tailscale.com/admin/dns)")
		return false
	}
	target := "http://127.0.0.1:" + strconv.Itoa(h.Port)
	port, changed, err := tailscale.Ensure(ctx, ts, target)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "access denied") {
			h.report("waiting for permission: sudo tailscale set --operator=$USER")
		} else {
			h.report(err.Error())
		}
		return false
	}
	if changed {
		h.Logger.Printf("https setup: tailscale serve now proxies https port %d to %s", port, target)
	}
	// Serve first, then allowed_users: with an empty list the server only answers
	// local requests, so there is no window in which the tailnet gets in.
	cfg, err := config.Load(h.ConfigPath)
	if err != nil {
		h.report(err.Error())
		return false
	}
	if len(cfg.AllowedUsers) == 0 && st.Login != "" {
		if err := config.SetAllowedUsers(h.ConfigPath, []string{st.Login}); err != nil {
			h.report(err.Error())
			return false
		}
		h.SetUsers([]string{st.Login})
		h.Logger.Printf("https setup: allowed_users = [%q]", st.Login)
	}
	url := "https://" + st.DNSName
	if port != 443 {
		url += ":" + strconv.Itoa(port)
	}
	if changed {
		h.Logger.Printf("https setup: done, open %s", url)
	}
	h.report("")
	return true
}

// Loop checks every interval until the HTTPS address is set up or ctx ends.
func (h *Healer) Loop(ctx context.Context, interval time.Duration) {
	for !h.Once(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
