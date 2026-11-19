package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/track"
)

// A feed that has not changed for this long is reported unhealthy. Upstream
// feeds normally change every 15 seconds or so.
const staleAfter = 2 * time.Minute

// The longest a request that restarts the polling waits for the feeds.
const coldWait = 5 * time.Second

type Poller interface {
	// The channel is closed once every feed has been tried.
	Demand() <-chan struct{}
	Active() bool
}

type feedHealth struct {
	AgeSeconds *int64 `json:"age_seconds"`
	Vehicles   int    `json:"vehicles"`
	Healthy    bool   `json:"healthy"`
}

// positions computes the feed at most once per cache period, however many
// requests arrive, so that every client sees the same vehicles in the same
// places.
type positions struct {
	tracker *track.Tracker
	poller  Poller
	cache   time.Duration
	now     func() time.Time

	mu      sync.Mutex
	feed    *gtfs.FeedMessage
	body    []byte
	expires time.Time
}

func (p *positions) current() (*gtfs.FeedMessage, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.feed == nil || !now.Before(p.expires) {
		feed := p.tracker.Feed(now)
		body, err := proto.Marshal(feed)
		if err != nil {
			return nil, nil, err
		}
		p.feed, p.body = feed, body
		p.expires = now.Add(p.cache)
	}
	return p.feed, p.body, nil
}

func (p *positions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-p.poller.Demand():
	case <-time.After(coldWait):
	case <-r.Context().Done():
		return
	}
	feed, body, err := p.current()
	if routes := routeFilter(r); err == nil && len(routes) > 0 {
		filtered := &gtfs.FeedMessage{Header: feed.Header}
		for _, e := range feed.Entity {
			if routes[e.GetVehicle().GetTrip().GetRouteId()] {
				filtered.Entity = append(filtered.Entity, e)
			}
		}
		body, err = proto.Marshal(filtered)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// New serves the feed and a health report. The feed is recomputed at most
// once per cache period; zero computes it for every request.
func New(tracker *track.Tracker, poller Poller, feeds []string, cache time.Duration) http.Handler {
	return newHandler(tracker, poller, feeds, cache, time.Now)
}

func newHandler(tracker *track.Tracker, poller Poller, feeds []string, cache time.Duration, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /positions", &positions{tracker: tracker, poller: poller, cache: cache, now: now})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		status := tracker.Status()
		report := map[string]feedHealth{}
		healthy := true
		// An idle poller has no feeds to be stale.
		polling := poller.Active()
		polled := feeds
		if !polling {
			polled = nil
		}
		for _, name := range polled {
			h := feedHealth{Vehicles: status[name].Vehicles}
			if header := status[name].Header; !header.IsZero() {
				age := int64(now.Sub(header).Seconds())
				h.AgeSeconds = &age
				h.Healthy = now.Sub(header) < staleAfter
			}
			healthy = healthy && h.Healthy
			report[name] = h
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(map[string]any{"healthy": healthy, "polling": polling, "feeds": report})
	})
	return mux
}

func routeFilter(r *http.Request) map[string]bool {
	routes := map[string]bool{}
	for _, param := range r.URL.Query()["route"] {
		for _, id := range strings.Split(param, ",") {
			if id = strings.TrimSpace(id); id != "" {
				routes[id] = true
			}
		}
	}
	return routes
}
