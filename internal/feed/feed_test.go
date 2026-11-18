package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	Poll(ctx, srv.Client(), Feed{Name: "a", URL: srv.URL}, 10*time.Millisecond, func(string, *gtfs.FeedMessage, time.Time) {
		if applied++; applied == 2 {
			cancel()
		}
	})
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
	Poll(ctx, srv.Client(), Feed{Name: "a", URL: srv.URL}, 10*time.Millisecond, func(string, *gtfs.FeedMessage, time.Time) { applied++ })
	if applied != 1 {
		t.Errorf("feed applied %d times over %d fetches, want once", applied, fetches)
	}
}
