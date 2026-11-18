package static

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdamcd/gtfs-position-interpolator/internal/fetch"
)

// Store downloads the static feed and keeps a copy on disk, so a restart
// without network access can still start from the last feed it saw.
type Store struct {
	URL    string
	Dir    string
	Client *http.Client

	etag   string
	loaded bool
}

// The copy on disk is named for its URL, so that a server started with a
// different feed never falls back to this one.
func (s *Store) zipPath() string {
	sum := sha256.Sum256([]byte(s.URL))
	return filepath.Join(s.Dir, fmt.Sprintf("static-%x.zip", sum[:6]))
}

func (s *Store) etagPath() string { return s.zipPath() + ".etag" }

// Fetch returns the feed if it is new since the last call. The first call
// always returns a feed, falling back to the copy on disk if the download
// fails.
func (s *Store) Fetch(ctx context.Context) (*Data, error) {
	if !s.loaded {
		if etag, err := os.ReadFile(s.etagPath()); err == nil {
			if _, err := os.Stat(s.zipPath()); err == nil {
				s.etag = strings.TrimSpace(string(etag))
			}
		}
	}
	body, etag, err := s.download(ctx)
	switch {
	case err != nil && s.loaded:
		return nil, err
	case err != nil:
		data, cacheErr := s.fromDisk()
		if cacheErr != nil {
			return nil, err
		}
		slog.Warn("static GTFS download failed, using the copy on disk", "err", err)
		s.loaded = true
		return data, nil
	case body == nil && s.loaded:
		return nil, nil
	case body == nil:
		data, err := s.fromDisk()
		if err != nil {
			return nil, err
		}
		s.loaded = true
		return data, nil
	}

	data, err := Parse(body)
	if err != nil {
		return nil, err
	}
	if err := s.toDisk(body, etag); err != nil {
		slog.Warn("could not cache static GTFS", "err", err)
	}
	s.etag, s.loaded = etag, true
	return data, nil
}

// download returns a nil body when the server reports the feed unchanged.
func (s *Store) download(ctx context.Context) (body []byte, etag string, err error) {
	headers := map[string]string{}
	if s.etag != "" {
		headers["If-None-Match"] = s.etag
	}
	resp, err := fetch.Get(ctx, s.Client, s.URL, headers)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, s.etag, nil
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		return body, resp.Header.Get("ETag"), err
	}
	return nil, "", fmt.Errorf("fetch static GTFS: %s", resp.Status)
}

func (s *Store) fromDisk() (*Data, error) {
	body, err := os.ReadFile(s.zipPath())
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

func (s *Store) toDisk(body []byte, etag string) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	// Remove the tag first so a crash part-way never pairs it with the
	// wrong zip.
	os.Remove(s.etagPath())
	tmp := s.zipPath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.zipPath()); err != nil {
		return err
	}
	if etag == "" {
		return nil
	}
	return os.WriteFile(s.etagPath(), []byte(etag), 0o644)
}
