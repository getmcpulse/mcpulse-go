package mcpulse

// The MCPulse wire format, as this package builds it.
//
// Plain structs rather than anything generated: this package compiles into
// other people's servers, and the API is what validates payloads. The price is
// drift, which payload_test.go is there to catch — it pins every field name,
// the wire version and the outcome list, so a change in the API's contract
// fails there rather than silently at ingest.

import "unicode/utf16"

// WireVersion is bumped only for a breaking change; the API rejects anything
// else.
const WireVersion = 1

// Outcome is how a tool call ended. Exactly one of these, always.
type Outcome string

const (
	// OutcomeOK means the tool ran and returned a result.
	OutcomeOK Outcome = "ok"
	// OutcomeBadArgs means arguments failed validation and the handler never ran.
	OutcomeBadArgs Outcome = "bad_args"
	// OutcomeToolError means the tool ran and returned isError: true.
	OutcomeToolError Outcome = "tool_error"
	// OutcomeCrashed means the tool threw.
	OutcomeCrashed Outcome = "crashed"
)

// Caps, so one malformed name cannot bloat a batch.
const (
	maxToolName   = 200
	maxClientName = 128
	maxTools      = 500
)

// ToolInfo is one tool as the client will see it.
type ToolInfo struct {
	Name string `json:"name"`
	// SchemaBytes is what this tool costs the context window, every session,
	// called or not.
	SchemaBytes int `json:"schema_bytes"`
}

// Payload is a startup or a call record. One struct rather than two, because
// the batch is heterogeneous and omitempty keeps each shape to its own fields.
type Payload struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	SessionID  string `json:"session_id"`
	ClientName string `json:"client_name"`

	// startup
	Tools []ToolInfo `json:"tools,omitempty"`

	// call
	ToolName      string  `json:"tool_name,omitempty"`
	StartedAt     string  `json:"started_at,omitempty"`
	DurationMS    int64   `json:"duration_ms,omitempty"`
	Outcome       Outcome `json:"outcome,omitempty"`
	ResponseBytes int     `json:"response_bytes,omitempty"`
	IsEmpty       bool    `json:"is_empty,omitempty"`
	ArgsHash      string  `json:"args_hash,omitempty"`
}

// utf16Length is the length of s in UTF-16 code units.
//
// ResponseBytes and SchemaBytes are, today, what JavaScript's String.length
// returns — code units, not bytes. The fields are named for bytes and hold code
// units, so "café" measures 4 and an emoji measures 2.
//
// That is a known wart in the wire format and fixing it is a pending decision.
// Until it is made, every port reproduces the TypeScript behaviour rather than
// each inventing its own, because the whole value of these numbers is that they
// are comparable across a customer's servers. When the wire fixes it, this
// function is the one line that changes.
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Cut on a rune boundary so a truncated name is still valid UTF-8.
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
