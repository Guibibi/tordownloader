// Command tordownloader emulates the qBittorrent WebUI API for Sonarr/Radarr,
// proxies releases to TorBox, and downloads finished files to local disk.
//
// M0 scaffold: load config, set up logging, open/migrate the SQLite store, and
// run until a shutdown signal. Pass --check to validate config + DB and exit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/guibibi/tordownloader/internal/config"
	"github.com/guibibi/tordownloader/internal/engine"
	"github.com/guibibi/tordownloader/internal/qbit"
	"github.com/guibibi/tordownloader/internal/store"
	"github.com/guibibi/tordownloader/internal/torbox"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	checkOnly := flag.Bool("check", false, "validate config and migrate the database, then exit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.SetDefault(newLogger(cfg.Log))

	slog.Info("starting tordownloader", "version", version, "listen", cfg.Server.ListenAddr)
	if cfg.TorBox.APIKey == "" {
		slog.Warn("TorBox API key not set; TorBox features will fail until provided (set TORBOX_API_KEY)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			slog.Error("closing store", "err", cerr)
		}
	}()
	slog.Info("database ready", "path", cfg.Database.Path)

	if *checkOnly {
		slog.Info("check mode: configuration and database OK")
		return nil
	}

	// Make the configured download root authoritative over save paths persisted
	// on a previous run, so changing download.root / TD_DOWNLOAD_ROOT actually
	// moves where new content lands instead of being shadowed by stale DB rows.
	if cats, tors, rerr := st.RebaseToRoot(ctx, cfg.Download.Root); rerr != nil {
		return fmt.Errorf("rebase save paths to root: %w", rerr)
	} else if cats > 0 || tors > 0 {
		slog.Info("rebased save paths to download root",
			"root", cfg.Download.Root, "categories", cats, "pending_torrents", tors)
	}

	// TorBox client + engine submitter (slot-gated createtorrent).
	tbClient := torbox.New(cfg.TorBox.APIKey,
		torbox.WithBaseURL(cfg.TorBox.BaseURL),
		torbox.WithRateLimit(cfg.TorBox.MaxRequestsPerMin))
	eng := engine.New(st, tbClient, engine.Config{
		MaxSlots:             cfg.TorBox.MaxActiveSlots,
		PollInterval:         cfg.TorBox.PollInterval.Std(),
		FailTimeout:          cfg.Failure.Timeout.Std(),
		StallTimeout:         cfg.Failure.StallTimeout.Std(),
		ProgressStallTimeout: cfg.Failure.ProgressStallTimeout.Std(),
		CachedStallTimeout:   cfg.Failure.CachedStallTimeout.Std(),
		ReannounceInterval:   cfg.Failure.ReannounceInterval.Std(),
		ParallelFiles:        cfg.Download.ParallelFiles,
		IncompleteSubdir:     cfg.Download.IncompleteSubdir,
		CacheCheck:           cfg.TorBox.CacheCheck,
	}, slog.Default())
	go eng.Run(ctx)

	srv := &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: qbit.New(st, cfg.Download.Root, cfg.Failure.StallTimeout.Std(), cfg.Failure.ProgressStallTimeout.Std(), cfg.Failure.CachedStallTimeout.Std(), eng.DeleteTorrent, slog.Default()).Routes(),
		// Bound the header-read phase so a slow/stalled client can't pin a
		// connection open indefinitely (Slowloris). Handlers themselves are
		// quick, so no ReadTimeout/WriteTimeout that could truncate a response.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("qBittorrent API listening", "addr", cfg.Server.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	slog.Info("startup complete; waiting for shutdown signal")
	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received; stopping")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown", "err", err)
	}
	return nil
}

func newLogger(c config.LogConfig) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(c.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(c.Format) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
