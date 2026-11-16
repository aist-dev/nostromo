package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestAliasDisableRestore(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".bashrc")
	orig := "export A=1\nalias agent='claude --dangerously-skip-permissions'\n  alias agent=\"x\"\nalias agents='ls'\nalias ll='ls -l'\n# alias agent=old\n"
	write(t, p, orig)
	if !HasAgentAlias(p) {
		t.Fatal("alias not detected")
	}
	disabled, err := DisableAgentAlias(p)
	if err != nil || len(disabled) != 2 {
		t.Fatalf("%v %v", disabled, err)
	}
	got := read(t, p)
	if !strings.Contains(got, DisabledPrefix+"alias agent='claude") || !strings.Contains(got, "alias agents='ls'\n") || HasAgentAlias(p) {
		t.Fatalf("after disable:\n%s", got)
	}
	if read(t, p+".nostromo.bak") != orig {
		t.Fatal("backup missing")
	}
	if d, _ := DisableAgentAlias(p); d != nil {
		t.Fatal("second disable must do nothing")
	}
	if ok, err := RestoreAgentAlias(p); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if read(t, p) != orig {
		t.Fatalf("restore:\n%s", read(t, p))
	}
	if ok, _ := RestoreAgentAlias(p); ok {
		t.Fatal("nothing left to restore")
	}

	fish := filepath.Join(t.TempDir(), "config.fish")
	write(t, fish, "alias agent 'claude --x'\n")
	if d, _ := DisableAgentAlias(fish); len(d) != 1 {
		t.Fatal("fish alias")
	}
	missing := filepath.Join(t.TempDir(), "none")
	if d, err := DisableAgentAlias(missing); d != nil || err != nil {
		t.Fatal("missing file")
	}
	if ok, err := RestoreAgentAlias(missing); ok || err != nil {
		t.Fatal("missing file restore")
	}
	if _, err := DisableAgentAlias(t.TempDir()); err == nil {
		t.Fatal("directory must fail")
	}
}

func TestBlocks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".zshrc")
	write(t, p, "export A=1")
	line := PathLine(p, "/h/.local/bin")
	if changed, err := EnsureBlock(p, line); !changed || err != nil {
		t.Fatal(changed, err)
	}
	want := "export A=1\n\n" + BlockStart + "\nexport PATH=\"/h/.local/bin:$PATH\"\n" + BlockEnd + "\n"
	if read(t, p) != want {
		t.Fatalf("block:\n%q", read(t, p))
	}
	if changed, _ := EnsureBlock(p, line); changed {
		t.Fatal("idempotent")
	}
	if changed, _ := EnsureBlock(p, "export X=2"); !changed || !strings.Contains(read(t, p), "export X=2\n"+BlockEnd) || strings.Count(read(t, p), BlockStart) != 1 {
		t.Fatalf("replace:\n%s", read(t, p))
	}
	write(t, p, read(t, p)+"after\n")
	if changed, err := RemoveBlock(p); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if read(t, p) != "export A=1\nafter\n" {
		t.Fatalf("remove:\n%q", read(t, p))
	}
	if changed, _ := RemoveBlock(p); changed {
		t.Fatal("nothing to remove")
	}
	write(t, p, BlockStart+"\nbroken")
	if changed, _ := RemoveBlock(p); changed {
		t.Fatal("unterminated block is left alone")
	}
	if changed, _ := EnsureBlock(p, "x"); !changed {
		t.Fatal("unterminated block: append a new one")
	}
	if changed, err := RemoveBlock(filepath.Join(dir, "none")); changed || err != nil {
		t.Fatal("missing")
	}
	fish := filepath.Join(dir, "new", "config.fish")
	if changed, err := EnsureBlock(fish, PathLine(fish, "/h/.local/bin")); !changed || err != nil || !strings.Contains(read(t, fish), "fish_add_path /h/.local/bin") {
		t.Fatalf("fish: %v %s", err, read(t, fish))
	}
	if _, err := EnsureBlock(dir, "x"); err == nil {
		t.Fatal("directory must fail")
	}
	blocker := filepath.Join(dir, "file")
	write(t, blocker, "")
	if _, err := EnsureBlock(filepath.Join(blocker, "sub", "rc"), "x"); err == nil {
		t.Fatal("parent is a file")
	}
}

func TestSymlinkedRCIsKept(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "bashrc")
	write(t, target, "alias agent='x'\n")
	link := filepath.Join(dir, ".bashrc")
	os.Symlink(target, link)
	if _, err := DisableAgentAlias(link); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if !strings.Contains(read(t, target), DisabledPrefix) {
		t.Fatal("the target was not updated")
	}
}

func TestRCFileForShell(t *testing.T) {
	for shell, want := range map[string]string{"/bin/zsh": "/h/.zshrc", "/usr/bin/fish": "/h/.config/fish/config.fish", "/bin/bash": "/h/.bashrc", "": "/h/.bashrc"} {
		if got := RCFileForShell("/h", shell); got != want {
			t.Errorf("%s: %s", shell, got)
		}
	}
	if len(RCFiles("/h")) != 3 {
		t.Fatal("RCFiles")
	}
}

func TestWriteRCBackupErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".bashrc")
	write(t, p, "alias agent=x\n")
	os.Chmod(p, 0o200)
	defer os.Chmod(p, 0o644)
	if os.Getuid() != 0 {
		if _, err := DisableAgentAlias(p); err == nil {
			t.Fatal("unreadable rc must fail")
		}
	}
	os.Chmod(p, 0o644)
	if err := writeRC(filepath.Join(dir, "missing"), "x", true); err == nil {
		t.Fatal("backup of a missing file must fail")
	}
	ro := t.TempDir()
	q := filepath.Join(ro, ".bashrc")
	write(t, q, "x")
	os.Chmod(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	if os.Getuid() != 0 {
		if err := writeRC(q, "y", true); err == nil {
			t.Fatal("read-only dir must fail")
		}
	}
}
