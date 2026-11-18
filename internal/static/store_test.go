package static

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jdamcd/gtfs-position-interpolator/internal/static/statictest"
)

type upstream struct {
	body     []byte
	etag     string
	down     bool
	requests int
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.requests++
	switch {
	case u.down:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case r.Header.Get("If-None-Match") == u.etag:
		w.WriteHeader(http.StatusNotModified)
	default:
		w.Header().Set("ETag", u.etag)
		w.Write(u.body)
	}
}

func TestStoreFetchesOnlyWhenTheFeedChanges(t *testing.T) {
	up := &upstream{body: statictest.Zip(t, statictest.Network()), etag: `"v1"`}
	srv := httptest.NewServer(up)
	defer srv.Close()
	store := &Store{URL: srv.URL, Dir: t.TempDir(), Client: srv.Client()}
	ctx := context.Background()

	first, err := store.Fetch(ctx)
	if err != nil || first == nil {
		t.Fatalf("first fetch: data %v, err %v", first, err)
	}
	if again, err := store.Fetch(ctx); err != nil || again != nil {
		t.Errorf("unchanged feed: data %v, err %v, want neither", again, err)
	}

	files := statictest.Network()
	files["stops.txt"] += "301N,New,40.8,-73.9\n"
	up.body, up.etag = statictest.Zip(t, files), `"v2"`
	changed, err := store.Fetch(ctx)
	if err != nil || changed == nil {
		t.Fatalf("changed feed: data %v, err %v", changed, err)
	}
	if _, ok := changed.Stops["301N"]; !ok {
		t.Error("changed feed is missing its new stop")
	}
}

func TestStoreStartsFromDiskWhenUnchangedOrOffline(t *testing.T) {
	up := &upstream{body: statictest.Zip(t, statictest.Network()), etag: `"v1"`}
	srv := httptest.NewServer(up)
	defer srv.Close()
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := (&Store{URL: srv.URL, Dir: dir, Client: srv.Client()}).Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	restarted := &Store{URL: srv.URL, Dir: dir, Client: srv.Client()}
	data, err := restarted.Fetch(ctx)
	if err != nil || data == nil {
		t.Fatalf("restart with unchanged feed: data %v, err %v", data, err)
	}

	up.down = true
	offline := &Store{URL: srv.URL, Dir: dir, Client: srv.Client()}
	data, err = offline.Fetch(ctx)
	if err != nil || data == nil {
		t.Fatalf("restart offline: data %v, err %v", data, err)
	}
	if _, err := offline.Fetch(ctx); err == nil {
		t.Error("a failed refresh should report its error")
	}
}

func TestStoreFailsWithNothingToFallBackOn(t *testing.T) {
	srv := httptest.NewServer(&upstream{down: true})
	defer srv.Close()
	store := &Store{URL: srv.URL, Dir: t.TempDir(), Client: srv.Client()}
	if data, err := store.Fetch(context.Background()); err == nil {
		t.Errorf("got data %v, want an error", data)
	}
}

func TestStoreDoesNotFallBackToAnotherFeed(t *testing.T) {
	up := &upstream{body: statictest.Zip(t, statictest.Network()), etag: `"v1"`}
	srv := httptest.NewServer(up)
	defer srv.Close()
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := (&Store{URL: srv.URL + "/one.zip", Dir: dir, Client: srv.Client()}).Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	up.down = true
	other := &Store{URL: srv.URL + "/two.zip", Dir: dir, Client: srv.Client()}
	if data, err := other.Fetch(ctx); err == nil {
		t.Errorf("got data %v from another feed's copy, want an error", data)
	}
}
