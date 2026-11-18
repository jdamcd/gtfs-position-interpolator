package feed

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/fetch"
)

type Feed struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type Handler func(feed string, msg *gtfs.FeedMessage, fetched time.Time)

func Poll(ctx context.Context, client *http.Client, f Feed, interval time.Duration, handle Handler) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last uint64
	for {
		msg, err := get(ctx, client, f)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("feed fetch failed", "feed", f.Name, "err", err)
			}
		// A feed that leaves out its header timestamp gives no way to tell
		// whether it has changed.
		case msg.GetHeader().GetTimestamp() != last || last == 0:
			last = msg.GetHeader().GetTimestamp()
			handle(f.Name, msg, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func get(ctx context.Context, client *http.Client, f Feed) (*gtfs.FeedMessage, error) {
	resp, err := fetch.Get(ctx, client, f.URL, f.Headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	msg := &gtfs.FeedMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
