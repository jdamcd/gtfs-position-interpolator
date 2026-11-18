package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	// The Docker image has no system timezone database.
	_ "time/tzdata"

	"github.com/jdamcd/gtfs-position-interpolator/internal/config"
	"github.com/jdamcd/gtfs-position-interpolator/internal/feed"
	"github.com/jdamcd/gtfs-position-interpolator/internal/server"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
	"github.com/jdamcd/gtfs-position-interpolator/internal/track"
)

const (
	cache         = time.Second
	staticRefresh = 24 * time.Hour
	maxAge        = 10 * time.Minute
	ramp          = 15 * time.Second
)

func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "gtfs-position-interpolator")
}

func main() {
	configPath := flag.String("config", "", "config file listing the static GTFS feed and the trip update feeds")
	addr := flag.String("addr", ":8080", "listen address")
	cacheDir := flag.String("cache-dir", defaultCacheDir(), "directory for the cached static GTFS feed")
	flag.Parse()
	if *configPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 30 * time.Second}
	store := &static.Store{URL: cfg.StaticURL, Dir: *cacheDir, Client: client}
	data, err := store.Fetch(ctx)
	if err != nil {
		slog.Error("loading static GTFS", "err", err)
		os.Exit(1)
	}
	slog.Info("static GTFS loaded", "stops", len(data.Stops))

	tracker := track.New(data, track.Config{MaxAge: maxAge, Ramp: ramp})
	go func() {
		ticker := time.NewTicker(staticRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			switch data, err := store.Fetch(ctx); {
			case err != nil:
				slog.Warn("static GTFS refresh failed", "err", err)
			case data != nil:
				tracker.SetStatic(data)
				slog.Info("static GTFS updated", "stops", len(data.Stops))
			}
		}
	}()

	var names []string
	for _, f := range cfg.Feeds {
		names = append(names, f.Name)
		go feed.Poll(ctx, client, f, time.Duration(cfg.Poll), tracker.Apply)
	}

	srv := &http.Server{Addr: *addr, Handler: server.New(tracker, names, cache)}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
