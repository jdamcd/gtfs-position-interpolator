package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jdamcd/gtfs-position-interpolator/internal/static"
	"github.com/jdamcd/gtfs-position-interpolator/internal/static/statictest"
	"github.com/jdamcd/gtfs-position-interpolator/internal/track"
)

func standing(route, tripID, stop string, at time.Time) *gtfs.FeedEntity {
	return &gtfs.FeedEntity{
		Id: proto.String(tripID),
		Vehicle: &gtfs.VehiclePosition{
			Trip:          &gtfs.TripDescriptor{TripId: proto.String(tripID), RouteId: proto.String(route)},
			StopId:        proto.String(stop),
			CurrentStatus: gtfs.VehiclePosition_STOPPED_AT.Enum(),
			Timestamp:     proto.Uint64(uint64(at.Unix())),
		},
	}
}

func snapshot(header time.Time, entities ...*gtfs.FeedEntity) *gtfs.FeedMessage {
	return &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("1.0"), Timestamp: proto.Uint64(uint64(header.Unix()))},
		Entity: entities,
	}
}

func newTracker(t *testing.T) *track.Tracker {
	t.Helper()
	data, err := static.Parse(statictest.Zip(t, statictest.Network()))
	if err != nil {
		t.Fatal(err)
	}
	return track.New(data, track.Config{MaxAge: 10 * time.Minute})
}

type poller struct {
	active bool
	ready  chan struct{}
}

func (p *poller) Demand() <-chan struct{} {
	p.active = true
	return p.ready
}

func (p *poller) Active() bool { return p.active }

func polling() *poller {
	ready := make(chan struct{})
	close(ready)
	return &poller{active: true, ready: ready}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestPositionsFiltersByRoute(t *testing.T) {
	now := time.Now()
	tracker := newTracker(t)
	tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now), standing("2", "X1", "104N", now)), now)
	h := New(tracker, polling(), []string{"a"}, 0)

	for path, want := range map[string]int{
		"/positions":                 2,
		"/positions?route=2":         1,
		"/positions?route=1,%202":    2,
		"/positions?route=Z":         0,
		"/positions?route=1&route=2": 2,
	} {
		rec := get(t, h, path)
		if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("%s: content type %q", path, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: allowed origin %q, want any", path, got)
		}
		feed := &gtfs.FeedMessage{}
		if err := proto.Unmarshal(rec.Body.Bytes(), feed); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(feed.Entity) != want {
			t.Errorf("%s: %d entities, want %d", path, len(feed.Entity), want)
		}
	}
}

func TestHealthz(t *testing.T) {
	now := time.Now()
	tracker := newTracker(t)
	h := New(tracker, polling(), []string{"a", "b"}, 0)

	tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now)), now)
	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d with feed b never seen, want 503", rec.Code)
	}
	var report struct {
		Healthy bool
		Feeds   map[string]struct {
			AgeSeconds *int64 `json:"age_seconds"`
			Vehicles   int
			Healthy    bool
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if a := report.Feeds["a"]; !a.Healthy || a.Vehicles != 1 || a.AgeSeconds == nil {
		t.Errorf("feed a: %+v, want healthy with 1 vehicle and an age", a)
	}
	if b := report.Feeds["b"]; b.Healthy || b.AgeSeconds != nil {
		t.Errorf("feed b: %+v, want unhealthy with no age", b)
	}

	tracker.Apply("b", snapshot(now.Add(-5*time.Minute)), now)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d with feed b five minutes old, want 503", rec.Code)
	}
	tracker.Apply("b", snapshot(now), now)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status %d with both feeds fresh, want 200", rec.Code)
	}
}

func TestHealthzAgesFeedsByTheHandlerClock(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	now := start
	tracker := newTracker(t)
	h := newHandler(tracker, polling(), []string{"a"}, 0, func() time.Time { return now })
	tracker.Apply("a", snapshot(start), start)

	now = start.Add(staleAfter - time.Second)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("status %d a second short of stale, want 200", rec.Code)
	}
	now = start.Add(staleAfter)
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d once stale, want 503", rec.Code)
	}
}

func TestHealthzIsHealthyWhileNothingIsPolled(t *testing.T) {
	idle := &poller{ready: make(chan struct{})}
	h := New(newTracker(t), idle, []string{"a", "b"}, 0)

	rec := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status %d with no feeds polled, want 200", rec.Code)
	}
	var report struct {
		Healthy, Polling bool
		Feeds            map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Healthy || report.Polling || len(report.Feeds) != 0 {
		t.Errorf("report %+v, want healthy, not polling and no feeds", report)
	}
	if idle.active {
		t.Error("the health check started the polling")
	}
}

func TestPositionsWaitForTheFeedsWhenPollingRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		tracker := newTracker(t)
		idle := &poller{ready: make(chan struct{})}
		h := New(tracker, idle, []string{"a"}, 0)

		var rec *httptest.ResponseRecorder
		go func() { rec = get(t, h, "/positions") }()
		synctest.Wait()
		if rec != nil {
			t.Fatal("answered before the feeds were fetched")
		}
		if !idle.active {
			t.Error("the request did not start the polling")
		}

		tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now)), now)
		close(idle.ready)
		synctest.Wait()
		if rec == nil {
			t.Fatal("not answered once the feeds were fetched")
		}
		if got := len(decode(t, rec).Entity); got != 1 {
			t.Errorf("%d vehicles, want the 1 fetched", got)
		}
	})
}

func TestPositionsDoNotWaitLongForASlowFeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := New(newTracker(t), &poller{ready: make(chan struct{})}, []string{"a"}, 0)

		var rec *httptest.ResponseRecorder
		go func() { rec = get(t, h, "/positions") }()
		time.Sleep(coldWait)
		synctest.Wait()
		if rec == nil || rec.Code != http.StatusOK {
			t.Fatal("not answered while a feed was still being fetched")
		}
	})
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) *gtfs.FeedMessage {
	t.Helper()
	feed := &gtfs.FeedMessage{}
	if err := proto.Unmarshal(rec.Body.Bytes(), feed); err != nil {
		t.Fatal(err)
	}
	return feed
}

func TestPositionsAreComputedOncePerCachePeriod(t *testing.T) {
	start := time.Now()
	now := start
	tracker := newTracker(t)
	tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now), standing("2", "X1", "104N", now)), now)
	h := newHandler(tracker, polling(), []string{"a"}, 5*time.Second, func() time.Time { return now })

	first := get(t, h, "/positions")

	now = start.Add(3 * time.Second)
	tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now)), now)
	again := get(t, h, "/positions")
	if got, want := decode(t, again).Header.GetTimestamp(), decode(t, first).Header.GetTimestamp(); got != want {
		t.Errorf("header timestamp %d three seconds on, want the cached %d", got, want)
	}
	if got := len(decode(t, again).Entity); got != 2 {
		t.Errorf("%d vehicles three seconds on, want the cached 2", got)
	}
	filtered := decode(t, get(t, h, "/positions?route=2"))
	if len(filtered.Entity) != 1 || filtered.Header.GetTimestamp() != decode(t, first).Header.GetTimestamp() {
		t.Errorf("filtered feed has %d vehicles at %d, want 1 from the cached feed", len(filtered.Entity), filtered.Header.GetTimestamp())
	}

	now = start.Add(5 * time.Second)
	fresh := decode(t, get(t, h, "/positions"))
	if got := fresh.Header.GetTimestamp(); got != uint64(now.Unix()) {
		t.Errorf("header timestamp %d after the cache period, want %d", got, now.Unix())
	}
	if got := len(fresh.Entity); got != 1 {
		t.Errorf("%d vehicles after the cache period, want 1", got)
	}
}

func TestPositionsWithoutACacheAreComputedPerRequest(t *testing.T) {
	now := time.Now()
	tracker := newTracker(t)
	h := newHandler(tracker, polling(), []string{"a"}, 0, func() time.Time { return now })

	if got := len(decode(t, get(t, h, "/positions")).Entity); got != 0 {
		t.Fatalf("%d vehicles before any feed, want 0", got)
	}
	tracker.Apply("a", snapshot(now, standing("1", "L1", "101N", now)), now)
	rec := get(t, h, "/positions")
	if got := len(decode(t, rec).Entity); got != 1 {
		t.Errorf("%d vehicles on the next request, want 1", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control %q, want no-store", got)
	}
}
