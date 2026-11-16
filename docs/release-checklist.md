# Release checklist

Automated tests cover everything except the items below. Check them by hand on one
Mac and one Linux machine before tagging a release.

## Before tagging

- [ ] `CHANGELOG.md` has the new version and says whether it migrates data.
- [ ] CI is green on `main` (unit, integration, browser, installer jobs).

## After the release is published

- [ ] Fresh install with the one-line command on a machine without `~/.nostromo`:
      `agent doctor` is all green.
- [ ] `agent` in a new terminal opens Claude Code in tmux with
      `--dangerously-skip-permissions`.
- [ ] The HTTPS address (`https://<machine>.<tailnet>.ts.net`) opens on a phone over
      Tailscale; the chat list matches `agent ls`.
- [ ] On the phone: open a chat, send a message with the input line, scroll the history
      with the buttons, stop and resume the chat.
- [ ] Real Claude Code hooks: after `/clear` in a chat, `resume_id` in
      `~/.nostromo/chats/<id>.json` changes; the first prompt sets the title.
- [ ] Reboot the machine: the address answers without logging in (Linux, linger) or
      after logging in (macOS); chats show as stopped; Resume continues the conversation.
- [ ] `agent update` from the previous release with a chat running: the chat keeps
      running, the UI reloads with the new version.
- [ ] macOS: the downloaded binary runs without a Gatekeeper prompt (curl does not set
      the quarantine flag). If it is ever blocked, add signing to the release workflow.
- [ ] `tailscale funnel status` shows nothing for the nostromo port.
