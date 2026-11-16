// Package tailscale wraps the parts of the tailscale CLI that nostromo uses.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aist-dev/nostromo/internal/tmux"
)

// MacAppBinary is where the Mac App Store / standalone app keeps its CLI.
const MacAppBinary = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// Status is the subset of `tailscale status --json` that nostromo needs.
type Status struct {
	BackendState    string
	DNSName         string // full MagicDNS name without the trailing dot
	HostName        string
	Login           string // login of the user owning this node
	MagicDNSEnabled bool
	CertDomains     []string
}

// Running reports whether tailscaled is up and logged in.
func (s Status) Running() bool { return s.BackendState == "Running" }

// Client runs the tailscale CLI.
type Client struct {
	Bin string
	Run tmux.Runner
}

// Find returns a client for the tailscale CLI on PATH or in the macOS app bundle.
func Find() (*Client, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return &Client{Bin: p, Run: tmux.ExecRunner{}}, nil
	}
	if _, err := os.Stat(MacAppBinary); err == nil {
		return &Client{Bin: MacAppBinary, Run: tmux.ExecRunner{}}, nil
	}
	return nil, errors.New("tailscale CLI not found")
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	out, errb, err := c.Run.Run(ctx, c.Bin, args...)
	if err != nil {
		msg := strings.TrimSpace(string(errb))
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("tailscale %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

type rawStatus struct {
	BackendState string
	Self         struct {
		DNSName  string
		HostName string
		UserID   json.Number
	}
	User           map[string]struct{ LoginName string }
	CurrentTailnet *struct {
		MagicDNSEnabled bool
	}
	CertDomains []string
}

// ParseStatus parses `tailscale status --json`.
func ParseStatus(data []byte) (Status, error) {
	var raw rawStatus
	if err := json.Unmarshal(data, &raw); err != nil {
		return Status{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	s := Status{
		BackendState: raw.BackendState,
		DNSName:      strings.TrimSuffix(raw.Self.DNSName, "."),
		HostName:     raw.Self.HostName,
		CertDomains:  raw.CertDomains,
	}
	if u, ok := raw.User[raw.Self.UserID.String()]; ok {
		s.Login = u.LoginName
	}
	if raw.CurrentTailnet != nil {
		s.MagicDNSEnabled = raw.CurrentTailnet.MagicDNSEnabled
	}
	return s, nil
}

// Status runs `tailscale status --json`.
func (c *Client) Status(ctx context.Context) (Status, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil && len(out) == 0 {
		return Status{}, err
	}
	return ParseStatus(out)
}

// ServeConfig is the subset of `tailscale serve status --json`.
type ServeConfig struct {
	// HTTPS maps a listening port to the proxy target of its "/" handler.
	HTTPS map[int]string
	// Funnel lists ports exposed to the public internet.
	Funnel []int
}

type rawServe struct {
	TCP map[string]struct {
		HTTPS bool
	}
	Web map[string]struct {
		Handlers map[string]struct {
			Proxy string
		}
	}
	AllowFunnel map[string]bool
}

// ParseServe parses `tailscale serve status --json`. Empty output means no config.
func ParseServe(data []byte) (ServeConfig, error) {
	sc := ServeConfig{HTTPS: map[int]string{}}
	if len(strings.TrimSpace(string(data))) == 0 {
		return sc, nil
	}
	var raw rawServe
	if err := json.Unmarshal(data, &raw); err != nil {
		return sc, fmt.Errorf("parse tailscale serve status: %w", err)
	}
	for hostPort, web := range raw.Web {
		port := portOf(hostPort)
		if h, ok := web.Handlers["/"]; ok && port > 0 {
			sc.HTTPS[port] = h.Proxy
		}
	}
	for hostPort, on := range raw.AllowFunnel {
		if p := portOf(hostPort); on && p > 0 {
			sc.Funnel = append(sc.Funnel, p)
		}
	}
	return sc, nil
}

func portOf(hostPort string) int {
	i := strings.LastIndex(hostPort, ":")
	if i < 0 {
		return 0
	}
	p, _ := strconv.Atoi(hostPort[i+1:])
	return p
}

// ServeStatus returns the current serve configuration.
func (c *Client) ServeStatus(ctx context.Context) (ServeConfig, error) {
	out, err := c.run(ctx, "serve", "status", "--json")
	if err != nil {
		return ServeConfig{HTTPS: map[int]string{}}, err
	}
	return ParseServe(out)
}

// Serve proxies HTTPS on httpsPort to target in the background (persists across restarts).
func (c *Client) Serve(ctx context.Context, httpsPort int, target string) error {
	_, err := c.run(ctx, "serve", "--bg", "--https="+strconv.Itoa(httpsPort), target)
	return err
}

// ServeOff removes the HTTPS proxy on httpsPort.
func (c *Client) ServeOff(ctx context.Context, httpsPort int) error {
	_, err := c.run(ctx, "serve", "--https="+strconv.Itoa(httpsPort), "off")
	return err
}

// API is what setup, doctor and the server need from Tailscale.
type API interface {
	Status(ctx context.Context) (Status, error)
	ServeStatus(ctx context.Context) (ServeConfig, error)
	Serve(ctx context.Context, httpsPort int, target string) error
	ServeOff(ctx context.Context, httpsPort int) error
}
