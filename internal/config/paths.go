package config

import (
	"os"
	"path/filepath"
)

// Paths resolves files inside the nostromo home directory.
type Paths struct {
	Home string
}

// HomeEnv overrides the nostromo home directory.
const HomeEnv = "NOSTROMO_HOME"

// DefaultPaths returns paths rooted at $NOSTROMO_HOME or ~/.nostromo.
func DefaultPaths() (Paths, error) {
	if h := os.Getenv(HomeEnv); h != "" {
		return Paths{Home: h}, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{Home: filepath.Join(userHome, ".nostromo")}, nil
}

func (p Paths) Bin() string        { return filepath.Join(p.Home, "bin") }
func (p Paths) Binary() string     { return filepath.Join(p.Home, "bin", "agent") }
func (p Paths) PrevBinary() string { return filepath.Join(p.Home, "bin", "agent.prev") }
func (p Paths) NewBinary() string  { return filepath.Join(p.Home, "bin", "agent.new") }
func (p Paths) Config() string     { return filepath.Join(p.Home, "config.toml") }
func (p Paths) TmuxConf() string   { return filepath.Join(p.Home, "tmux.conf") }
func (p Paths) Chats() string      { return filepath.Join(p.Home, "chats") }
func (p Paths) Logs() string       { return filepath.Join(p.Home, "logs") }
func (p Paths) ServerLog() string  { return filepath.Join(p.Home, "logs", "server.log") }
func (p Paths) HookLog() string    { return filepath.Join(p.Home, "logs", "hook.log") }
func (p Paths) UpdateCache() string {
	return filepath.Join(p.Home, "update.json")
}
