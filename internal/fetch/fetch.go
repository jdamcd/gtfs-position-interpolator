package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Get's errors leave out the URL, which may hold an API key.
func Get(ctx context.Context, client *http.Client, address string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, withoutURL(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	return resp, nil
}

func withoutURL(err error) error {
	var failed *url.Error
	if errors.As(err, &failed) {
		return fmt.Errorf("%s: %w", strings.ToLower(failed.Op), failed.Err)
	}
	return err
}
