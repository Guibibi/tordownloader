// Package config loads tordownloader configuration from a YAML file with
// environment-variable overrides, applies defaults, and validates the result.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full application configuration.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	TorBox   TorBoxConfig   `yaml:"torbox"`
	Download DownloadConfig `yaml:"download"`
	Database DatabaseConfig `yaml:"database"`
	Failure  FailureConfig  `yaml:"failure"`
	Log      LogConfig      `yaml:"log"`
}

// ServerConfig configures the qBittorrent-emulation HTTP API.
type ServerConfig struct {
	ListenAddr string `yaml:"listen_addr"`
}

// TorBoxConfig configures the TorBox client and queue.
type TorBoxConfig struct {
	APIKey         string   `yaml:"api_key"`
	BaseURL        string   `yaml:"base_url"`
	MaxActiveSlots int      `yaml:"max_active_slots"`
	CacheCheck     bool     `yaml:"cache_check"`
	PollInterval   Duration `yaml:"poll_interval"`
	// MaxRequestsPerMin caps the rate of TorBox API calls (all endpoints share
	// TorBox's general 300/min limit). A burst of requestdl links for a large pack
	// is paced to stay under the cap instead of tripping a 429. 0 disables the cap.
	MaxRequestsPerMin int `yaml:"max_requests_per_min"`
}

// DownloadConfig configures how files are written to local disk.
type DownloadConfig struct {
	Root             string `yaml:"root"`
	IncompleteSubdir string `yaml:"incomplete_subdir"`
	ParallelFiles    int    `yaml:"parallel_files"`
}

// DatabaseConfig configures the SQLite state store.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// FailureConfig configures when a fetching torrent is abandoned with an error
// (which makes Sonarr/Radarr blacklist the release and try another).
type FailureConfig struct {
	// Timeout is an optional absolute cap measured from when a torrent becomes
	// TorBox-active. 0 (the default) disables it, leaving StallTimeout as the only
	// failure path; set it to bound how long a perpetually-slow torrent may hold a
	// scarce TorBox slot.
	Timeout Duration `yaml:"timeout"`
	// StallTimeout fails a torrent that makes no forward progress (no bytes moving
	// and progress not climbing) for this long while still at 0% — a dead or
	// unseeded release, where abandoning costs nothing and lets Sonarr/Radarr try
	// another release sooner. A slow but advancing download keeps resetting the
	// clock and is never failed for being slow. A non-positive value disables
	// stall detection for zero-progress torrents.
	StallTimeout Duration `yaml:"stall_timeout"`
	// ProgressStallTimeout is the stall grace for a torrent that has already made
	// real progress (>0%). Thin swarms lose their seeds for a while and recover on
	// a scale of hours, and failing a partial fetch blacklists a release that
	// would likely have finished — so it gets far more patience than a 0% one. A
	// non-positive value disables stall-failing for torrents with progress.
	ProgressStallTimeout Duration `yaml:"progress_stall_timeout"`
	// CachedStallTimeout is the stall grace for a release TorBox reported as cached.
	// Such a release sits at 0% only while TorBox materialises bytes it already has,
	// not because peers are dead, so it gets a longer grace than StallTimeout — but
	// still bounded, as a safety net if the hand-off is genuinely broken. A
	// non-positive value disables stall-failing for cached releases.
	CachedStallTimeout Duration `yaml:"cached_stall_timeout"`
	// ReannounceInterval is how often a stalled (non-cached) fetch is nudged with a
	// TorBox reannounce so its client re-contacts trackers — stalls are often
	// transient (seeds drop off and return) and a nudge can pick recovered peers up
	// sooner than passively waiting. A non-positive value disables nudging.
	ReannounceInterval Duration `yaml:"reannounce_interval"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// Default returns a Config populated with the built-in defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{ListenAddr: "0.0.0.0:6500"},
		TorBox: TorBoxConfig{
			BaseURL:           "https://api.torbox.app",
			MaxActiveSlots:    3,
			CacheCheck:        true,
			PollInterval:      Duration(10 * time.Second),
			MaxRequestsPerMin: 280,
		},
		Download: DownloadConfig{
			Root:             "/downloads",
			IncompleteSubdir: ".incomplete",
			ParallelFiles:    4,
		},
		Database: DatabaseConfig{Path: "data/tordownloader.db"},
		Failure: FailureConfig{
			Timeout:              0,
			StallTimeout:         Duration(20 * time.Minute),
			ProgressStallTimeout: Duration(2 * time.Hour),
			CachedStallTimeout:   Duration(30 * time.Minute),
			ReannounceInterval:   Duration(5 * time.Minute),
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}
}

// Load reads config from path (if it exists), layered over defaults, then
// applies environment overrides and validates. A missing file is not an error:
// defaults + env are used.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// No file: defaults + env only.
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv overlays a handful of environment variables. The API key is
// intentionally env-first so it never has to live in a committed file.
func (c *Config) applyEnv() {
	if v := os.Getenv("TORBOX_API_KEY"); v != "" {
		c.TorBox.APIKey = v
	}
	if v := os.Getenv("TD_LISTEN_ADDR"); v != "" {
		c.Server.ListenAddr = v
	}
	if v := os.Getenv("TD_DOWNLOAD_ROOT"); v != "" {
		c.Download.Root = v
	}
	if v := os.Getenv("TD_DB_PATH"); v != "" {
		c.Database.Path = v
	}
	if v := os.Getenv("TD_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("TD_LOG_FORMAT"); v != "" {
		c.Log.Format = v
	}
}

// Validate checks invariants the rest of the app relies on. Note: the TorBox
// API key is *not* required here — it's only needed once the TorBox client runs
// (M1+), so M0 can start without it (main warns instead).
func (c *Config) Validate() error {
	if c.Server.ListenAddr == "" {
		return errors.New("server.listen_addr is required")
	}
	if c.Download.Root == "" {
		return errors.New("download.root is required")
	}
	if c.Download.ParallelFiles < 1 {
		return errors.New("download.parallel_files must be >= 1")
	}
	if c.Database.Path == "" {
		return errors.New("database.path is required")
	}
	if c.TorBox.MaxActiveSlots < 1 {
		return errors.New("torbox.max_active_slots must be >= 1")
	}
	if c.TorBox.PollInterval.Std() <= 0 {
		return errors.New("torbox.poll_interval must be > 0")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log.level %q (want debug|info|warn|error)", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log.format %q (want text|json)", c.Log.Format)
	}
	return nil
}

// Duration is a time.Duration that unmarshals from a Go duration string
// (e.g. "20m", "10s") in YAML.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string such as "20m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"20m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}
