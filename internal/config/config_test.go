package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaultsAndOverrides(t *testing.T) {
	c, err := Load(write(t, `{
		"static_url": "https://example.com/gtfs.zip",
		"feeds": [{"name": "bus", "url": "https://example.com/trip-updates"}],
		"poll": "30s"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll != Duration(30*time.Second) {
		t.Errorf("poll %v, want the value in the file", time.Duration(c.Poll))
	}
}

func TestLoadFillsSecretsFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_FEED_KEY", "s3cret")
	c, err := Load(write(t, `{
		"static_url": "https://example.com/gtfs.zip?key=${TEST_FEED_KEY}",
		"feeds": [{"name": "bus", "url": "https://example.com/trip-updates?$format=pb", "headers": {"Authorization": "Bearer ${TEST_FEED_KEY}"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Feeds[0].URL; got != "https://example.com/trip-updates?$format=pb" {
		t.Errorf("feed URL %q, want its own $ left alone", got)
	}
	if c.StaticURL != "https://example.com/gtfs.zip?key=s3cret" {
		t.Errorf("static URL %q", c.StaticURL)
	}
	if got := c.Feeds[0].Headers["Authorization"]; got != "Bearer s3cret" {
		t.Errorf("header %q", got)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	feed := `"feeds": [{"name": "bus", "url": "https://example.com/tu"}]`
	static := `"static_url": "https://example.com/gtfs.zip"`
	for name, c := range map[string]struct{ content, want string }{
		"no static URL":    {`{` + feed + `}`, "static_url is required"},
		"no feeds":         {`{` + static + `}`, "at least one feed"},
		"unnamed feed":     {`{` + static + `, "feeds": [{"url": "https://example.com/tu"}]}`, "name is required"},
		"duplicate feed":   {`{` + static + `, "feeds": [{"name": "a", "url": "x"}, {"name": "a", "url": "y"}]}`, "listed twice"},
		"unknown setting":  {`{` + static + `, ` + feed + `, "pol": "15s"}`, `unknown field "pol"`},
		"numeric duration": {`{` + static + `, ` + feed + `, "poll": 15}`, "written as strings"},
		"unset variable":   {`{` + static + `, "feeds": [{"name": "a", "url": "https://example.com/${TEST_UNSET_KEY}"}]}`, "TEST_UNSET_KEY"},
	} {
		_, err := Load(write(t, c.content))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one mentioning %q", name, err, c.want)
		}
	}
}

func TestShippedConfigsLoad(t *testing.T) {
	paths, err := filepath.Glob("../../configs/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shipped configs found: %v", err)
	}
	for _, path := range paths {
		if _, err := Load(path); err != nil {
			t.Error(err)
		}
	}
}
