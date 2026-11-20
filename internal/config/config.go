package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jdamcd/gtfs-position-interpolator/internal/feed"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`durations are written as strings such as "15s"`)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

type Config struct {
	StaticURL string      `json:"static_url"`
	Feeds     []feed.Feed `json:"feeds"`

	Poll Duration `json:"poll"`
}

func Load(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Config{Poll: Duration(5 * time.Second)}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.expand(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

var reference = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// expand fills ${NAME} in URLs and headers from the environment, keeping API
// keys out of the config file. Any other $ is left alone: some URLs have one.
func (c *Config) expand() error {
	var missing []string
	fill := func(s string) string {
		return reference.ReplaceAllStringFunc(s, func(match string) string {
			name := match[2 : len(match)-1]
			value, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return value
		})
	}
	c.StaticURL = fill(c.StaticURL)
	for i := range c.Feeds {
		c.Feeds[i].URL = fill(c.Feeds[i].URL)
		for name, value := range c.Feeds[i].Headers {
			c.Feeds[i].Headers[name] = fill(value)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) validate() error {
	if c.StaticURL == "" {
		return errors.New("static_url is required")
	}
	if len(c.Feeds) == 0 {
		return errors.New("at least one feed is required")
	}
	names := map[string]bool{}
	for i, f := range c.Feeds {
		switch {
		case f.Name == "":
			return fmt.Errorf("feeds[%d]: name is required", i)
		case f.URL == "":
			return fmt.Errorf("feed %s: url is required", f.Name)
		case names[f.Name]:
			return fmt.Errorf("feed %s is listed twice", f.Name)
		}
		names[f.Name] = true
	}
	if c.Poll <= 0 {
		return errors.New("poll must be positive")
	}
	return nil
}
