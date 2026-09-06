package mcpulse

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// httpClient is shared: a fresh client per send would open a new connection
// pool each time, inside somebody else's server.
var httpClient = &http.Client{Timeout: sendTimeout}

// postBatch sends one batch. It reports whether the batch landed, and never
// returns an error — a caller must never have to handle one.
//
// A failed batch is dropped, deliberately. Retrying means either a queue that
// grows while the network is down, or duplicate rows when a 202 is lost on the
// way back. Neither is worth it for analytics: the next flush is five seconds
// away, and a gap in a chart is a far smaller problem than memory growth inside
// someone else's server.
func postBatch(ctx context.Context, payloads []Payload, opts resolved, timeout time.Duration) bool {
	if len(payloads) == 0 {
		return true
	}

	body, err := json.Marshal(struct {
		Batch []Payload `json:"batch"`
	}{Batch: payloads})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.endpoint+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+opts.key)
	req.Header.Set("user-agent", "mcpulse-go")

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
