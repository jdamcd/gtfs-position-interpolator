package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetSendsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("x-api-key")))
	}))
	defer srv.Close()
	resp, err := Get(context.Background(), srv.Client(), srv.URL, map[string]string{"x-api-key": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 16)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "s3cret" {
		t.Errorf("server saw key %q, want s3cret", got)
	}
}

func TestErrorsLeaveOutTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	address := srv.URL + "/feed?key=s3cret"
	srv.Close()
	for name, address := range map[string]string{
		"unreachable server": address,
		"malformed URL":      "https://example.com/\x7f?key=s3cret",
	} {
		_, err := Get(context.Background(), http.DefaultClient, address, nil)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error %q includes the key", name, err)
		}
	}
}
