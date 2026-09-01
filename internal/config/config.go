// Package config loads tordownloader configuration from a YAML file with
// environment-variable overrides, applies defaults, and validates the result.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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
	Arr      []ArrInstance  `yaml:"arr"`
	Log      LogConfig      `yaml:"log"`

	// Warnings collects non-fatal configuration problems found during Load
	// (e.g. a half-set env-var pair) for the caller to log once logging is up.
	Warnings []string `yaml:"-"`
}

// ArrInstance is one Sonarr/Radarr endpoint to notify when a download fails
// for good. Sonarr never triggers failed-download handling from qBittorrent
// states (state=error is only a warning), so without this the *arr never
// blocklists a failed release or grabs an alternative — the item just sits in
// its queue. With an instance configured, tordownloader removes the failed
// item via the *arr's own API with blocklist + redownload, closing the loop.
type ArrInstance struct {
	Name   string `yaml:"name"`    // label for logs; defaults to the URL
	URL    string `yaml:"url"`     // e.g. http://sonarr:8989 (base path ok)
	APIKey string `yaml:"api_key"` // Settings → General → API Key
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
	// ParallelTorrents is how many torrents are pulled to local disk at once, so
	// one huge download doesn't hold already-cached content hostage behind it.
	// Total concurrent connections are bounded by parallel_torrents ×
	// parallel_files.
	ParallelTorrents int `yaml:"parallel_torrents"`
	// IdleTimeout aborts a CDN transfer that goes silent — connection still open,
	// no bytes arriving — for this long. Without it a hung transfer blocks
	// forever: the HTTP client deliberately has no overall timeout (a legitimate
	// download can run for hours), so silence is the only thing we can safely
	// measure. An aborted transfer is retried with a fresh link and resumed from
	// the bytes already on disk, so this is cheap to trip. A non-positive value
	// disables the watchdog.
	IdleTimeout Duration `yaml:"idle_timeout"`
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
	// MinSpeed is the average download speed, in bytes per second, a TorBox fetch
	// must sustain over SlowWindow once it has started moving bytes. A fetch
	// averaging less than this is abandoned (ERROR → the *arr blocklists the
	// release and grabs another): at a trickle it would not finish inside any
	// useful window, and meanwhile it holds one of the account's scarce slots.
	// The average is measured from real progress (progress × size), not from
	// TorBox's reported speed, so a torrent that reports movement while
	// delivering nothing is caught too. 0 disables the check, restoring the
	// older "never fail for slowness, only for a full stall" behaviour.
	MinSpeed ByteSize `yaml:"min_speed"`
	// SlowWindow is the averaging window for MinSpeed. It is deliberately long:
	// swarms are lumpy, and a torrent is only judged on a sustained average, never
	// on an instantaneous dip. The check needs a full window of data before it can
	// fail anything, and the window only starts once the first bytes arrive — a
	// fetch still at 0% is owned by the stall tiers below. A non-positive value
	// disables the check; 0 means the default (15m).
	SlowWindow Duration `yaml:"slow_window"`
	// ErrorRetention prunes terminal failures: an ERROR torrent whose *arr
	// notification is settled (reported, or given up on — or *arr push-back is
	// not configured at all) is deleted outright (row + partial files + TorBox
	// leftovers) once it has sat unchanged this long, so failures don't
	// accumulate forever on a long-running box. A negative value disables
	// pruning; 0 means the default (168h).
	ErrorRetention Duration `yaml:"error_retention"`
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
			ParallelTorrents: 3,
			IdleTimeout:      Duration(2 * time.Minute),
		},
		Database: DatabaseConfig{Path: "data/tordownloader.db"},
		Failure: FailureConfig{
			Timeout:              0,
			StallTimeout:         Duration(20 * time.Minute),
			ProgressStallTimeout: Duration(2 * time.Hour),
			CachedStallTimeout:   Duration(30 * time.Minute),
			ReannounceInterval:   Duration(5 * time.Minute),
			MinSpeed:             50 * KiB,
			SlowWindow:           Duration(15 * time.Minute),
			ErrorRetention:       Duration(168 * time.Hour),
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
	// Docker-friendly shorthand for the common one-Sonarr / one-Radarr setup;
	// appended to (not replacing) any instances from the YAML file.
	c.applyArrEnv("sonarr", "TD_SONARR_URL", "TD_SONARR_API_KEY")
	c.applyArrEnv("radarr", "TD_RADARR_URL", "TD_RADARR_API_KEY")
}

// applyArrEnv appends an *arr instance from a URL+key env pair, unless an
// instance with that URL is already configured (env then overrides its key).
// A half-set pair is almost always a typo in the container config, so it is
// surfaced as a warning instead of silently ignored.
func (c *Config) applyArrEnv(name, urlEnv, keyEnv string) {
	url, key := os.Getenv(urlEnv), os.Getenv(keyEnv)
	switch {
	case url == "" && key == "":
		return
	case url == "":
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"%s is set but %s is empty/unset; %s failure push-back disabled", keyEnv, urlEnv, name))
		return
	case key == "":
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"%s is set but %s is empty/unset; %s failure push-back disabled", urlEnv, keyEnv, name))
		return
	}
	for i := range c.Arr {
		if strings.TrimRight(c.Arr[i].URL, "/") == strings.TrimRight(url, "/") {
			c.Arr[i].APIKey = key
			return
		}
	}
	c.Arr = append(c.Arr, ArrInstance{Name: name, URL: url, APIKey: key})
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
	if c.Download.ParallelTorrents < 1 {
		return errors.New("download.parallel_torrents must be >= 1")
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
	for i := range c.Arr {
		in := &c.Arr[i]
		if in.URL == "" {
			return fmt.Errorf("arr[%d]: url is required", i)
		}
		if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
			return fmt.Errorf("arr[%d]: url %q must start with http:// or https://", i, in.URL)
		}
		if in.APIKey == "" {
			return fmt.Errorf("arr[%d] (%s): api_key is required", i, in.URL)
		}
		if in.Name == "" {
			in.Name = in.URL
		}
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

// Byte-size units for ByteSize values. Binary (1024-based), matching how disk
// and transfer sizes are usually quoted in this domain.
const (
	KiB ByteSize = 1 << 10
	MiB ByteSize = 1 << 20
	GiB ByteSize = 1 << 30
)

// byteSizeUnits maps the accepted suffixes to their multiplier. "KB" and "KiB"
// are both 1024: a config file is not the place to litigate SI vs binary, and
// treating them alike avoids a silently-3%-off threshold.
var byteSizeUnits = []struct {
	suffix string
	mult   ByteSize
}{
	{"KIB", KiB}, {"MIB", MiB}, {"GIB", GiB},
	{"KB", KiB}, {"MB", MiB}, {"GB", GiB},
	{"K", KiB}, {"M", MiB}, {"G", GiB},
	{"B", 1},
}

// ByteSize is a byte count that unmarshals from either a plain YAML integer
// (bytes) or a human string with a unit suffix, e.g. "50KB", "1.5MiB", "2G".
type ByteSize int64

// Bytes returns the size as a plain byte count.
func (b ByteSize) Bytes() int64 { return int64(b) }

// String renders the size in the largest unit that divides it evenly, so a
// value round-trips readably in logs.
func (b ByteSize) String() string {
	switch {
	case b == 0:
		return "0B"
	case b%GiB == 0:
		return fmt.Sprintf("%dGiB", b/GiB)
	case b%MiB == 0:
		return fmt.Sprintf("%dMiB", b/MiB)
	case b%KiB == 0:
		return fmt.Sprintf("%dKiB", b/KiB)
	default:
		return fmt.Sprintf("%dB", int64(b))
	}
}

// UnmarshalYAML parses a byte size from an integer or a suffixed string.
func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	var n int64
	if err := value.Decode(&n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("size must be a number of bytes or a string like \"50KB\": %w", err)
	}
	parsed, err := ParseByteSize(s)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// ParseByteSize parses a byte size such as "50KB", "1.5MiB", "2G" or "1024".
// The number may be fractional; the result is truncated to whole bytes.
func ParseByteSize(s string) (ByteSize, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, errors.New("empty size")
	}
	upper := strings.ToUpper(trimmed)
	mult := ByteSize(1)
	for _, u := range byteSizeUnits {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mult
			upper = strings.TrimSpace(strings.TrimSuffix(upper, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(upper, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: want a number of bytes or a value like \"50KB\"", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", s)
	}
	return ByteSize(n * float64(mult)), nil
}
