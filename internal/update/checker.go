package update

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
)

// Cache is the content of ~/.nostromo/update.json.
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// Checker runs the daily update check inside the server.
type Checker struct {
	Paths    config.Paths
	Current  string
	Auto     bool
	Enabled  bool
	Interval time.Duration
	Latest   func(ctx context.Context) (string, error)
	// Spawn starts a detached `agent update`.
	Spawn func() error
	Now   func() time.Time

	mu    sync.Mutex
	cache Cache
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) load() Cache {
	var cache Cache
	if data, err := os.ReadFile(c.Paths.UpdateCache()); err == nil {
		json.Unmarshal(data, &cache)
	}
	return cache
}

// Available returns the newer version found by the last check, or "".
func (c *Checker) Available() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache.Latest == "" {
		c.cache = c.load()
	}
	if Newer(c.cache.Latest, c.Current) {
		return Normalize(c.cache.Latest)
	}
	return ""
}

// CheckOnce asks for the latest release if the cache is older than the interval.
// It returns true when a newer version is available.
func (c *Checker) CheckOnce(ctx context.Context, logger *log.Logger) bool {
	interval := c.Interval
	if interval == 0 {
		interval = 24 * time.Hour
	}
	cache := c.load()
	if c.now().Sub(cache.CheckedAt) >= interval {
		latest, err := c.Latest(ctx)
		if err != nil {
			logger.Printf("update check failed: %v", err)
			return false
		}
		cache = Cache{CheckedAt: c.now().UTC(), Latest: latest}
		data, _ := json.Marshal(cache)
		config.WriteFileAtomic(c.Paths.UpdateCache(), data, 0o600)
	}
	c.mu.Lock()
	c.cache = cache
	c.mu.Unlock()
	return Newer(cache.Latest, c.Current)
}

// Loop checks once at start and then every hour (the cache limits real requests
// to once per interval). With Auto it starts the updater when a release appears.
func (c *Checker) Loop(ctx context.Context, logger *log.Logger, tick time.Duration) {
	if !c.Enabled {
		return
	}
	if tick == 0 {
		tick = time.Hour
	}
	for {
		if c.CheckOnce(ctx, logger) && c.Auto {
			logger.Printf("auto_update: starting update to %s", c.Available())
			if err := c.Spawn(); err != nil {
				logger.Printf("auto_update: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
	}
}

// SpawnDetached starts `<binary> update` in a new session, so restarting the
// server does not kill the updater. Output goes to logs/update.log.
func SpawnDetached(paths config.Paths) error {
	os.MkdirAll(paths.Logs(), 0o700)
	logFile, err := os.OpenFile(paths.Logs()+"/update.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(paths.Binary(), "update")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
