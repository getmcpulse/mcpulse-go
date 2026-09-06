package mcpulse

import (
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// isEmptyResult answers: did this call succeed while returning nothing useful?
//
// This is the metric that catches the failures nobody reports: a search that
// finds no rows, a lookup that misses, a query that comes back []. The protocol
// calls all of those success, the model gets nothing it can use, and the author
// never hears about it.
//
// Only ever asked of a call that already succeeded — an error has its own
// outcome and is not also "empty".
func isEmptyResult(res *mcp.CallToolResult) bool {
	if res == nil {
		return true
	}

	// Structured output is the answer when a tool provides one, so it decides.
	if res.StructuredContent != nil {
		// StructuredContent is whatever the handler returned, so it may be a
		// struct rather than one of the decoded JSON shapes. Round-tripping it
		// is what makes the reading below the same one every other SDK does.
		inner := unwrapResultEnvelope(asJSONValue(res.StructuredContent))
		if text, ok := inner.(string); ok {
			// What comes out of the envelope is whatever the tool returned.
			// When that is a string it gets the same reading a text part does.
			return isHollowText(text)
		}
		return isHollow(inner)
	}

	return isEmptyContent(res.Content)
}

// asJSONValue re-reads a Go value as the JSON shape it would go over the wire
// as, so a handler's struct and a decoded map are judged identically.
func asJSONValue(v any) any {
	switch v.(type) {
	case nil, string, bool, float64, []any, map[string]any:
		return v
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return v
	}
	return decoded
}

// unwrapResultEnvelope undoes a single-key {"result": …} wrapper.
//
// SDKs that derive an output schema from a handler's return type wrap a
// non-object return: a tool that returns "[]" arrives as {"result": "[]"}.
// Judging the envelope would quietly kill this metric — every result would be
// an object with one key, so nothing would ever be empty, and the one thing
// is_empty exists to catch would never fire.
func unwrapResultEnvelope(v any) any {
	if object, ok := v.(map[string]any); ok && len(object) == 1 {
		if inner, ok := object["result"]; ok {
			return inner
		}
	}
	return v
}

// isEmptyContent reads MCP's list of content parts.
//
// No parts is empty. One text part is the common case, and it is empty when the
// text is blank or when the text is itself a serialised empty collection — "[]"
// is the single most common way a tool says "nothing found" while reporting
// success.
func isEmptyContent(content []mcp.Content) bool {
	if len(content) == 0 {
		return true
	}
	if len(content) > 1 {
		return false
	}

	text, ok := content[0].(*mcp.TextContent)
	if !ok {
		return false
	}
	return isHollowText(text.Text)
}

func isHollowText(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		// Prose, not JSON. A tool that answers in a sentence has said something.
		return false
	}
	return isHollow(decoded)
}

// isHollow reports an empty array, empty object, blank string, or nothing.
func isHollow(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(value) == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		// A number or a boolean is an answer. 0 and false are results, not
		// absences, and counting them as empty would report working tools as
		// broken.
		return false
	}
}
