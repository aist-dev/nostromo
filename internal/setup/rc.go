package setup

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aist-dev/nostromo/internal/config"
)

// Markers around the block nostromo adds to shell rc files.
const (
	BlockStart     = "# >>> nostromo >>>"
	BlockEnd       = "# <<< nostromo <<<"
	DisabledPrefix = "# nostromo: disabled "
)

// RCFiles returns the rc files nostromo may edit, for bash, zsh and fish.
func RCFiles(home string) []string {
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
}

// RCFileForShell returns the rc file of a login shell path.
func RCFileForShell(home, shell string) string {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish")
	default:
		return filepath.Join(home, ".bashrc")
	}
}

func readFile(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(data), err == nil, err
}

func writeRC(path, content string, existed bool) error {
	if existed {
		bak := path + ".nostromo.bak"
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			orig, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(bak, orig, 0o600); err != nil {
				return err
			}
		}
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return config.WriteFileAtomic(path, []byte(content), mode)
}

// aliasRe matches an alias definition for "agent" in bash, zsh or fish.
var aliasRe = regexp.MustCompile(`^\s*alias\s+agent(=|\s)`)

// DisableAgentAlias comments out alias definitions of "agent", which would shadow
// the binary in interactive shells. It returns the lines it disabled.
func DisableAgentAlias(path string) ([]string, error) {
	content, existed, err := readFile(path)
	if err != nil || !existed {
		return nil, err
	}
	lines := strings.Split(content, "\n")
	var disabled []string
	for i, l := range lines {
		if aliasRe.MatchString(l) {
			disabled = append(disabled, strings.TrimSpace(l))
			lines[i] = DisabledPrefix + l
		}
	}
	if len(disabled) == 0 {
		return nil, nil
	}
	return disabled, writeRC(path, strings.Join(lines, "\n"), true)
}

// RestoreAgentAlias undoes DisableAgentAlias.
func RestoreAgentAlias(path string) (bool, error) {
	content, existed, err := readFile(path)
	if err != nil || !existed || !strings.Contains(content, DisabledPrefix) {
		return false, err
	}
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, DisabledPrefix)
	}
	return true, writeRC(path, strings.Join(lines, "\n"), true)
}

// HasAgentAlias reports whether an active alias for "agent" is defined in path.
func HasAgentAlias(path string) bool {
	content, _, _ := readFile(path)
	for _, l := range strings.Split(content, "\n") {
		if aliasRe.MatchString(l) {
			return true
		}
	}
	return false
}

// PathLine returns the shell line that puts dir on PATH.
func PathLine(rcFile, dir string) string {
	if strings.HasSuffix(rcFile, ".fish") {
		return "fish_add_path " + dir
	}
	return `export PATH="` + dir + `:$PATH"`
}

// EnsureBlock writes body between the nostromo markers, replacing an existing block.
func EnsureBlock(path, body string) (bool, error) {
	content, existed, err := readFile(path)
	if err != nil {
		return false, err
	}
	block := BlockStart + "\n" + body + "\n" + BlockEnd + "\n"
	if strings.Contains(content, block) {
		return false, nil
	}
	if i := strings.Index(content, BlockStart); i >= 0 {
		if j := strings.Index(content[i:], BlockEnd); j >= 0 {
			end := i + j + len(BlockEnd)
			if end < len(content) && content[end] == '\n' {
				end++
			}
			content = content[:i] + block + content[end:]
			return true, writeRC(path, content, existed)
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content != "" {
		content += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, writeRC(path, content+block, existed)
}

// RemoveBlock deletes the nostromo block from path.
func RemoveBlock(path string) (bool, error) {
	content, existed, err := readFile(path)
	if err != nil || !existed {
		return false, err
	}
	i := strings.Index(content, BlockStart)
	if i < 0 {
		return false, nil
	}
	j := strings.Index(content[i:], BlockEnd)
	if j < 0 {
		return false, nil
	}
	end := i + j + len(BlockEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	start := i
	if start > 0 && strings.HasSuffix(content[:start], "\n\n") {
		start--
	}
	return true, writeRC(path, content[:start]+content[end:], true)
}
