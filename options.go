package mcpulse

import (
	"strings"
	"time"
)

// DefaultEndpoint is where payloads go when Endpoint is not set.
const DefaultEndpoint = "https://api.getmcpulse.com"

// Flush when either is reached, whichever comes first.
const (
	flushAtItems = 30
	flushEvery   = 5 * time.Second
)

// maxBuffered is a hard ceiling on the buffer. Reached only when the network is
// gone; past it the oldest payloads are dropped, because a customer's server
// running out of memory over our analytics is the one failure we must never
// cause.
const maxBuffered = 1000

// Best-effort window for the final flush on the way out, and the ceiling on any
// single batch.
const (
	exitFlushTimeout = 1 * time.Second
	sendTimeout      = 10 * time.Second
)

// Options is everything Watch accepts.
type Options struct {
	// Key is the ingest key, mp_live_…, minted per MCP in the dashboard.
	Key string
	// Endpoint overrides where payloads are sent. Point this at a local API
	// while developing.
	Endpoint string
	// Disabled makes Watch a no-op — useful in tests and CI.
	//
	// Negative rather than an Enabled bool so the zero value of Options is the
	// working one: mcpulse.Options{Key: k} must not be silently switched off.
	Disabled bool
	// Debug logs what is being sent, and why a send failed, to stderr.
	Debug bool
}

type resolved struct {
	key      string
	endpoint string
	enabled  bool
	debug    bool
}

// resolve fills in the defaults.
//
// An empty key turns the SDK off: a server started without its key configured
// should be silent, not a source of 401s on every flush.
func resolve(o Options) resolved {
	key := strings.TrimSpace(o.Key)

	endpoint := o.Endpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultEndpoint
	}

	return resolved{
		key:      key,
		endpoint: strings.TrimRight(endpoint, "/"),
		enabled:  !o.Disabled && key != "",
		debug:    o.Debug,
	}
}
