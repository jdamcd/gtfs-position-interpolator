// Records the trip update feeds as the server's poller hands them to the
// tracker, for replay: one <fetch unix time>.pb.gz per message in a directory
// per feed, with the static feed and the config alongside.
package main

import (
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/config"
	"github.com/jdamcd/gtfs-position-interpolator/internal/feed"
	"github.com/jdamcd/gtfs-position-interpolator/internal/fetch"
)

func main() {
	configPath := flag.String("config", "", "config file listing the static GTFS feed and the trip update feeds")
	out := flag.String("out", "", "directory to write the capture to, which must not exist yet")
	length := flag.Duration("for", 30*time.Minute, "how long to record")
	flag.Parse()
	if *configPath == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*configPath, *out, *length); err != nil {
		slog.Error("capture failed", "err", err)
		os.Exit(1)
	}
}

func run(configPath, out string, length time.Duration) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		return err
	}
	// The file as written rather than cfg, which has API keys filled in.
	if err := os.WriteFile(filepath.Join(out, "config.json"), raw, 0o644); err != nil {
		return err
	}
	for _, f := range cfg.Feeds {
		if err := os.Mkdir(filepath.Join(out, f.Name), 0o755); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 30 * time.Second}
	if err := download(ctx, client, cfg.StaticURL, filepath.Join(out, "static.zip")); err != nil {
		return fmt.Errorf("static GTFS: %w", err)
	}

	save := func(name string, msg *gtfs.FeedMessage, fetched time.Time) {
		path := filepath.Join(out, name, fmt.Sprintf("%d.pb.gz", fetched.Unix()))
		if err := write(path, msg); err != nil {
			slog.Error("saving feed message", "feed", name, "err", err)
		}
	}
	// Idle as long as the recording, so the poller doesn't stop itself first.
	poller := feed.NewPoller(client, cfg.Feeds, time.Duration(cfg.Poll), length, save, func() {})
	ctx, cancel := context.WithTimeout(ctx, length)
	defer cancel()
	poller.Demand()
	slog.Info("recording", "for", length, "out", out)
	poller.Run(ctx)

	for _, f := range cfg.Feeds {
		saved, _ := filepath.Glob(filepath.Join(out, f.Name, "*.pb.gz"))
		slog.Info("recorded", "feed", f.Name, "messages", len(saved))
	}
	return nil
}

func download(ctx context.Context, client *http.Client, address, path string) error {
	resp, err := fetch.Get(ctx, client, address, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	return errors.Join(err, f.Close())
}

func write(path string, msg *gtfs.FeedMessage) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	_, err = zw.Write(body)
	return errors.Join(err, zw.Close(), f.Close())
}
