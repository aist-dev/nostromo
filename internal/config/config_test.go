package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	if cfg.Port != def.Port || cfg.TmuxSocket != "nostromo" || cfg.DefaultAgent != "claude" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !strings.Contains(cfg.Agents["claude"].Start, "--dangerously-skip-permissions --session-id {id}") {
		t.Fatalf("claude start template lost the permission flag: %q", cfg.Agents["claude"].Start)
	}
	if !cfg.Agents["claude"].WrapInShell() {
		t.Fatal("shell wrap must default to true")
	}
}

func TestDefaultFileParsesToDefaults(t *testing.T) {
	cfg, err := Load(writeFile(t, DefaultFile))
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	if cfg.Port != def.Port || cfg.Repo != def.Repo || cfg.CheckUpdates != def.CheckUpdates ||
		cfg.Agents["claude"].Start != def.Agents["claude"].Start ||
		cfg.Agents["claude"].Resume != def.Agents["claude"].Resume {
		t.Fatalf("DefaultFile and Default() disagree: %+v", cfg)
	}
}

func TestLoadOverridesAndAddsAgents(t *testing.T) {
	p := writeFile(t, `
port = 9000
tmux_socket = "other"
check_updates = false
auto_update = true
allowed_users = ["me@example.com"]
peers = ["box"]
default_agent = "codex"
repo = "x/y"

[agents.codex]
start = "codex"
resume = "codex resume --last"
shell_wrap = false
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9000 || cfg.TmuxSocket != "other" || cfg.CheckUpdates || !cfg.AutoUpdate || cfg.Repo != "x/y" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if len(cfg.AllowedUsers) != 1 || cfg.AllowedUsers[0] != "me@example.com" || len(cfg.Peers) != 1 {
		t.Fatalf("lists not applied: %+v", cfg)
	}
	if _, ok := cfg.Agents["claude"]; !ok {
		t.Fatal("built-in claude adapter must remain")
	}
	if cfg.Agents["codex"].WrapInShell() {
		t.Fatal("shell_wrap = false not honored")
	}
	if got := cfg.AgentNames(); strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("AgentNames = %v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"syntax":              `port = `,
		"unknown placeholder": "[agents.x]\nstart = \"x {nope}\"\nresume = \"x\"\n",
		"unknown key":         "colour = 1\n",
		"bad default":         "default_agent = \"ghost\"\n",
		"bad port":            "port = 0\n",
		"bad socket":          "tmux_socket = \"a/b\"\n",
		"empty resume":        "[agents.x]\nstart = \"x\"\nresume = \"\"\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, content)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadUnreadable(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("reading a directory must fail")
	}
}

func TestValidateTemplate(t *testing.T) {
	for _, ok := range []string{"claude --session-id {id}", "x --resume {resume_id} --dir {cwd}", "plain"} {
		if err := ValidateTemplate(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "x {ID}", "x {}"} {
		if err := ValidateTemplate(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestSetAllowedUsers(t *testing.T) {
	p := writeFile(t, DefaultFile)
	if err := SetAllowedUsers(p, []string{"a@example.com", "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.AllowedUsers, ",") != "a@example.com,b@example.com" {
		t.Fatalf("got %v", cfg.AllowedUsers)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "# nostromo configuration") {
		t.Fatal("comments must be preserved")
	}
}

func TestSetAllowedUsersInsertsBeforeTables(t *testing.T) {
	p := writeFile(t, "port = 9000\n[agents.claude]\nstart = \"c {id}\"\nresume = \"c {resume_id}\"\n")
	if err := SetAllowedUsers(p, []string{"me"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedUsers) != 1 || cfg.Port != 9000 {
		t.Fatalf("got %+v", cfg)
	}
}

func TestSetAllowedUsersAppendsAndCreates(t *testing.T) {
	p := writeFile(t, "port = 9000")
	if err := SetAllowedUsers(p, []string{"me"}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(p); err != nil || len(cfg.AllowedUsers) != 1 {
		t.Fatalf("append: %v %+v", err, cfg)
	}
	missing := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := SetAllowedUsers(missing, []string{"me"}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(missing); err != nil || len(cfg.AllowedUsers) != 1 {
		t.Fatalf("create: %v %+v", err, cfg)
	}
	if err := SetAllowedUsers(t.TempDir(), nil); err == nil {
		t.Fatal("directory path must fail")
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o644)
	if err := WriteFileAtomic(filepath.Join(blocker, "x"), nil, 0o644); err == nil {
		t.Fatal("parent is a file: expected error")
	}
	target := filepath.Join(dir, "d")
	os.Mkdir(target, 0o755)
	os.WriteFile(filepath.Join(target, "keep"), nil, 0o644)
	if err := WriteFileAtomic(target, []byte("x"), 0o644); err == nil {
		t.Fatal("renaming over a non-empty directory must fail")
	}
}

func TestPaths(t *testing.T) {
	t.Setenv(HomeEnv, "/tmp/nh")
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		p.Bin(): "/tmp/nh/bin", p.Binary(): "/tmp/nh/bin/agent", p.PrevBinary(): "/tmp/nh/bin/agent.prev",
		p.NewBinary(): "/tmp/nh/bin/agent.new", p.Config(): "/tmp/nh/config.toml", p.TmuxConf(): "/tmp/nh/tmux.conf",
		p.Chats(): "/tmp/nh/chats", p.Logs(): "/tmp/nh/logs", p.ServerLog(): "/tmp/nh/logs/server.log",
		p.HookLog(): "/tmp/nh/logs/hook.log", p.UpdateCache(): "/tmp/nh/update.json",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
	t.Setenv(HomeEnv, "")
	t.Setenv("HOME", "/tmp/uh")
	p, err = DefaultPaths()
	if err != nil || p.Home != "/tmp/uh/.nostromo" {
		t.Fatalf("got %v %v", p, err)
	}
	t.Setenv("HOME", "")
	if _, err := DefaultPaths(); err == nil {
		t.Fatal("expected error without HOME")
	}
}
