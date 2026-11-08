package publish

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Ping calls a healthchecks.io ping URL; an empty URL is a no-op. The check
// alerts when pings stop arriving, which catches runs that never happen.
func Ping(ctx context.Context, pingURL string) error {
	if pingURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingURL, nil)
	if err != nil {
		return fmt.Errorf("invalid HC_PING_ASN")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck ping failed")
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthcheck ping returned HTTP %d", resp.StatusCode)
	}
	return nil
}
