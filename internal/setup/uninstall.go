package setup

import (
	"context"
	"fmt"
	"os"

	"github.com/aist-dev/nostromo/internal/config"
)

// Uninstall removes the service, Tailscale serve config, hooks, the command and rc
// edits. Chat records stay unless purge is set; running tmux sessions are left alone.
func (in *Installer) Uninstall(ctx context.Context, purge bool) error {
	r := &report{out: in.Out}
	cfg, err := config.Load(in.Paths.Config())
	if err != nil {
		cfg = config.Default()
	}
	if in.Service != nil {
		if err := in.Service.Uninstall(ctx); err != nil {
			r.warn("service: %v", err)
		} else {
			r.ok("removed %s", in.Service.Name())
		}
	}
	if in.Tailscale != nil {
		if sc, err := in.Tailscale.ServeStatus(ctx); err == nil {
			for p, t := range sc.HTTPS {
				if t != in.target(cfg) {
					continue
				}
				if err := in.Tailscale.ServeOff(ctx, p); err != nil {
					r.warn("tailscale serve on port %d: %v", p, err)
				} else {
					r.ok("removed tailscale serve on https port %d", p)
				}
			}
		}
	}
	if changed, err := RemoveClaudeHooks(in.claudeSettings()); err != nil {
		r.warn("Claude Code hooks: %v", err)
	} else if changed {
		r.ok("removed Claude Code hooks")
	}
	link := in.linkPath()
	if dest, err := os.Readlink(link); err == nil && dest == in.Paths.Binary() {
		os.Remove(link)
		r.ok("removed %s", link)
	}
	for _, rc := range RCFiles(in.Home) {
		if changed, err := RemoveBlock(rc); err != nil {
			r.warn("%s: %v", rc, err)
		} else if changed {
			r.ok("removed the nostromo block from %s", rc)
		}
		if changed, err := RestoreAgentAlias(rc); err != nil {
			r.warn("%s: %v", rc, err)
		} else if changed {
			r.ok("restored the agent alias in %s", rc)
		}
	}
	if purge {
		if err := os.RemoveAll(in.Paths.Home); err != nil {
			return err
		}
		r.ok("removed %s", in.Paths.Home)
	} else {
		fmt.Fprintf(in.Out, "Chat records are kept in %s; remove it with: agent uninstall --purge (or rm -rf)\n", in.Paths.Home)
	}
	fmt.Fprintln(in.Out, "Running chats keep running in tmux.")
	return nil
}
