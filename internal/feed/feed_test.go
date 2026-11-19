package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

func TestGetSendsTheFeedsHeaders(t *testing.T) {
	body, err := proto.Marshal(&gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: proto.Uint64(42)},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "s3cret" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	if _, err := get(context.Background(), srv.Client(), Feed{URL: srv.URL}); err == nil {
		t.Error("fetch without the key succeeded, want an error")
	}
	msg, err := get(context.Background(), srv.Client(), Feed{URL: srv.URL, Headers: map[string]string{"x-api-key": "s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.GetHeader().GetTimestamp(); got != 42 {
		t.Errorf("header timestamp %d, want 42", got)
	}
}

func TestPollAppliesEveryFetchOfAFeedWithNoHeaderTimestamp(t *testing.T) {
	body, err := proto.Marshal(&gtfs.FeedMessage{Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0")}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied := 0
	p := NewPoller(srv.Client(), nil, 10*time.Millisecond, time.Minute, func(string, *gtfs.FeedMessage, time.Time) {
		if applied++; applied == 2 {
			cancel()
		}
	}, func() {})
	p.poll(ctx, Feed{Name: "a", URL: srv.URL}, func() {})
	if applied < 2 {
		t.Errorf("feed applied %d times, want every fetch", applied)
	}
}

func TestPollSkipsAFeedWhoseHeaderTimestampIsUnchanged(t *testing.T) {
	body, err := proto.Marshal(&gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: proto.Uint64(42)},
	})
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetches++; fetches == 4 {
			cancel()
		}
		w.Write(body)
	}))
	defer srv.Close()

	applied := 0
	p := NewPoller(srv.Client(), nil, 10*time.Millisecond, time.Minute, func(string, *gtfs.FeedMessage, time.Time) { applied++ }, func() {})
	p.poll(ctx, Feed{Name: "a", URL: srv.URL}, func() {})
	if applied != 1 {
		t.Errorf("feed applied %d times over %d fetches, want once", applied, fetches)
	}
}

func await(t *testing.T, what string, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Counts requests as sent: the server may still handle one the poller abandoned.
type counted struct {
	http.RoundTripper
	sent *atomic.Int32
}

func (c counted) RoundTrip(r *http.Request) (*http.Response, error) {
	c.sent.Add(1)
	return c.RoundTripper.RoundTrip(r)
}

func TestPollerFetchesOnlyWhileInDemand(t *testing.T) {
	body, err := proto.Marshal(&gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0"), Timestamp: proto.Uint64(42)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fetches, applied atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	client := &http.Client{Transport: counted{srv.Client().Transport, &fetches}}

	resets := make(chan struct{}, 1)
	p := NewPoller(client, []Feed{{Name: "a", URL: srv.URL}, {Name: "b", URL: srv.URL}}, 10*time.Millisecond, 100*time.Millisecond,
		func(string, *gtfs.FeedMessage, time.Time) { applied.Add(1) },
		func() { resets <- struct{}{} })
	go p.Run(t.Context())

	time.Sleep(50 * time.Millisecond)
	if got := fetches.Load(); got != 0 || p.Active() {
		t.Fatalf("%d fetches before any demand, want none", got)
	}

	await(t, "the first fetch of each feed", p.Demand())
	if got := applied.Load(); got != 2 || !p.Active() {
		t.Errorf("%d feeds applied once in demand, want both", got)
	}

	await(t, "the demand to lapse", resets)
	stopped := fetches.Load()
	if stopped < 4 {
		t.Errorf("%d fetches while in demand, want each feed polled more than once", stopped)
	}
	time.Sleep(50 * time.Millisecond)
	if got := fetches.Load(); got != stopped || p.Active() {
		t.Errorf("%d fetches after the demand lapsed, want none", got-stopped)
	}

	await(t, "the first fetch of each feed again", p.Demand())
	if got := applied.Load(); got != 4 {
		t.Errorf("%d feeds applied over two spells of demand, want both in each", got)
	}
}
