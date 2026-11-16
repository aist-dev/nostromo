package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aist-dev/nostromo/internal/config"
	"github.com/aist-dev/nostromo/internal/state"
	"github.com/aist-dev/nostromo/internal/store"
	"github.com/aist-dev/nostromo/internal/tmux"
)

// Check results.
const (
	Pass = "ok"
	Warn = "warn"
	Fail = "fail"
)

// Check is one doctor finding.
type Check struct {
	Name   string
	Result string
	Detail string
	Fix    string
}

// Doctor runs every check, prints them and reports whether none failed.
func (in *Installer) Doctor(ctx context.Context) bool {
	checks := in.Checks(ctx)
	ok := true
	for _, c := range checks {
		mark := "✓"
		switch c.Result {
		case Warn:
			mark = "!"
		case Fail:
			mark = "✗"
			ok = false
		}
		line := fmt.Sprintf("%s %s", mark, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(in.Out, line)
		if c.Fix != "" && c.Result != Pass {
			fmt.Fprintf(in.Out, "    fix: %s\n", strings.ReplaceAll(c.Fix, "\n", "\n         "))
		}
	}
	return ok
}

// Checks runs the doctor checks without printing.
func (in *Installer) Checks(ctx context.Context) []Check {
	var out []Check
	add := func(name, result, detail, fix string) {
		out = append(out, Check{Name: name, Result: result, Detail: detail, Fix: fix})
	}
	cfg, err := config.Load(in.Paths.Config())
	if err != nil {
		add("config", Fail, err.Error(), "fix or remove "+in.Paths.Config())
		cfg = config.Default()
	} else {
		add("config", Pass, in.Paths.Config(), "")
	}

	if v, err := tmux.CheckVersion(ctx, in.Tmux); err != nil {
		add("tmux", Fail, err.Error(), "brew install tmux (macOS) or sudo apt install tmux (Debian/Ubuntu)")
	} else {
		add("tmux", Pass, v.Raw, "")
	}

	if fi, err := os.Stat(in.Paths.Binary()); err != nil || fi.Mode()&0o111 == 0 {
		add("binary", Fail, in.Paths.Binary()+" missing", "reinstall: curl -fsSL https://raw.githubusercontent.com/aist-dev/nostromo/main/install.sh | bash")
	} else {
		add("binary", Pass, in.Paths.Binary(), "")
	}

	link := in.linkPath()
	if dest, err := os.Readlink(link); err != nil || dest != in.Paths.Binary() {
		add("command", Fail, link+" does not point to "+in.Paths.Binary(), in.setupCmd())
	} else {
		add("command", Pass, link, "")
	}
	if !inPath(in.PathEnv, filepath.Dir(link)) {
		add("PATH", Warn, filepath.Dir(link)+" is not on PATH in this shell", "open a new terminal, or add it to PATH in your shell rc file")
	} else {
		add("PATH", Pass, filepath.Dir(link), "")
	}
	in.checkResolution(ctx, link, add)

	cfgPort := strconv.Itoa(cfg.Port)
	if in.Service == nil {
		add("service", Warn, "no supported service manager", "run \"agent serve\" yourself")
	} else if in.Service.Running(ctx) {
		add("service", Pass, in.Service.Name()+" is running", "")
	} else {
		add("service", Fail, in.Service.Name()+" is not running", in.Service.RestartCommand())
	}
	if v, err := in.Health(ctx, cfg.Port); err != nil {
		add("server", Fail, "no answer on 127.0.0.1:"+cfgPort, "see "+in.Paths.ServerLog())
	} else if v != in.Version {
		add("server", Warn, "running version "+v+", installed "+in.Version, restartFix(in))
	} else {
		add("server", Pass, "version "+v+" on 127.0.0.1:"+cfgPort, "")
	}

	in.checkTailscale(ctx, cfg, add)

	if len(cfg.AllowedUsers) == 0 {
		add("allowed_users", Warn, "empty: the UI only works on this machine", in.setupCmd()+" (fills in your Tailscale login), or edit "+in.Paths.Config())
	} else {
		add("allowed_users", Pass, strings.Join(cfg.AllowedUsers, ", "), "")
	}

	for _, name := range cfg.AgentNames() {
		if cfg.Agents[name].Hooks == "claude" {
			if ok, err := ClaudeHooksInstalled(in.claudeSettings()); err != nil {
				add("hooks", Warn, err.Error(), "fix the JSON, then: "+in.setupCmd())
			} else if !ok {
				add("hooks", Warn, "Claude Code hooks missing: Resume may reopen a cleared conversation", in.setupCmd())
			} else {
				add("hooks", Pass, "Claude Code hooks installed", "")
			}
			break
		}
	}
	for _, name := range cfg.AgentNames() {
		prog := agentProgram(cfg.Agents[name])
		if p, err := in.loginShell(ctx, "command -v "+prog); err != nil || p == "" {
			add("agent "+name, Warn, prog+" not found in a login shell", "install it, or fix the [agents."+name+"] start command")
		} else {
			add("agent "+name, Pass, lastLine(p), "")
		}
	}

	st := store.New(in.Paths.Chats())
	recs, bad, err := st.List()
	if err != nil {
		add("chats", Fail, err.Error(), "")
	} else if len(bad) > 0 {
		var names []string
		for _, b := range bad {
			names = append(names, fmt.Sprintf("%s (%v)", b.Path, b.Err))
		}
		add("chats", Warn, fmt.Sprintf("%d unreadable records: %s", len(bad), strings.Join(names, "; ")), "move or delete the listed files")
	} else {
		add("chats", Pass, fmt.Sprintf("%d records", len(recs)), "")
	}
	if panes, err := in.Tmux.ListPanes(ctx); err == nil {
		if orphans := state.Orphans(recs, panes); len(orphans) > 0 {
			var names []string
			for _, p := range orphans {
				names = append(names, p.Session)
			}
			add("sessions", Warn, "tmux sessions without a chat record: "+strings.Join(names, ", "), "tmux -L "+cfg.TmuxSocket+" kill-session -t <name>")
		}
	}
	return out
}

func restartFix(in *Installer) string {
	if in.Service != nil {
		return in.Service.RestartCommand()
	}
	return "restart agent serve"
}

func (in *Installer) checkResolution(ctx context.Context, link string, add func(string, string, string, string)) {
	name := filepath.Base(link)
	out, err := in.loginShell(ctx, "type "+name)
	switch {
	case err != nil && out == "":
		add("resolve", Warn, "\""+name+"\" not found in a login shell", "open a new terminal")
	case strings.Contains(out, "alias"):
		add("resolve", Fail, "\""+name+"\" is a shell alias: "+lastLine(out), "remove the alias from your shell rc file, or run: "+in.setupCmd())
	case strings.Contains(out, "function"):
		add("resolve", Fail, "\""+name+"\" is a shell function", "remove the function from your shell rc file")
	case strings.Contains(out, link) || strings.Contains(out, in.Paths.Binary()):
		add("resolve", Pass, "\""+name+"\" runs "+in.Paths.Binary(), "")
	default:
		add("resolve", Fail, "\""+name+"\" runs another program: "+lastLine(out), "remove it from PATH, or reinstall with NOSTROMO_CMD=<other name>")
	}
}

func (in *Installer) checkTailscale(ctx context.Context, cfg config.Config, add func(string, string, string, string)) {
	if in.Tailscale == nil {
		add("tailscale", Warn, "tailscale CLI not found", "install Tailscale: https://tailscale.com/download")
		return
	}
	st, err := in.Tailscale.Status(ctx)
	if err != nil || !st.Running() {
		add("tailscale", Warn, "not running", "tailscale up")
		return
	}
	add("tailscale", Pass, st.DNSName, "")
	if !st.MagicDNSEnabled || len(st.CertDomains) == 0 {
		add("https certs", Warn, "MagicDNS or HTTPS certificates are off", "enable them at https://login.tailscale.com/admin/dns")
	}
	sc, err := in.Tailscale.ServeStatus(ctx)
	if err != nil {
		add("serve", Warn, err.Error(), in.operatorHint())
		return
	}
	target := in.target(cfg)
	port := 0
	for p, t := range sc.HTTPS {
		if t == target {
			port = p
		}
	}
	if port == 0 {
		add("serve", Warn, "tailscale serve does not proxy to "+target, in.setupCmd())
		return
	}
	add("serve", Pass, fmt.Sprintf("https port %d -> %s", port, target), "")
	funnel := false
	for _, f := range sc.Funnel {
		if f == port {
			funnel = true
		}
	}
	if funnel {
		add("funnel", Fail, "Funnel is ON: the UI is on the public internet", fmt.Sprintf("tailscale funnel --https=%d off", port))
	} else {
		add("funnel", Pass, "off", "")
	}
	url := "https://" + st.DNSName
	if port != 443 {
		url += ":" + strconv.Itoa(port)
	}
	if in.HTTPSCheck != nil {
		if err := in.HTTPSCheck(ctx, url+"/healthz"); err != nil {
			add("https", Warn, url+": "+err.Error(), "check the tailnet HTTPS settings")
		} else {
			add("https", Pass, url, "")
		}
	}
}
