package mcpulse

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Unhashable is what an argument set hashes to when it cannot be serialised.
const Unhashable = "000000000000"

// ArgsHash is a short, one-way fingerprint of a call's arguments.
//
// This is the only thing MCPulse ever learns about what was passed to a tool,
// and it is deliberately not enough to learn anything: 12 hex characters of a
// SHA-256 over the RFC 8785 canonical form, with no way back. All the product
// asks of it is "were these two calls made with the same arguments or different
// ones" — which is what separates a model retrying a reworded request from a
// client paging through results.
func ArgsHash(args any) string {
	// A tool that takes no arguments is called with arguments absent. That is
	// an ordinary call, not a failure, and it hashes as the empty object it is
	// — otherwise every no-argument tool shares one hash with every call whose
	// arguments blew up.
	if args == nil {
		args = map[string]any{}
	}

	canonical, err := Canonicalize(args)
	if err != nil {
		return Unhashable
	}

	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:12]
}

// ArgsHashRaw hashes arguments still in their wire form.
//
// The Go MCP SDK hands tool arguments over as json.RawMessage, so this is the
// path a real call takes: decoding here rather than accepting the SDK's typed
// struct means the hash is computed from what the client actually sent, before
// defaults are applied or unknown fields are dropped.
func ArgsHashRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ArgsHash(nil)
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Unhashable
	}
	if decoded == nil {
		return ArgsHash(nil)
	}
	return ArgsHash(decoded)
}

// NewSessionID identifies one run of the customer's server, so calls can be
// grouped and a cost-per-session worked out.
//
// Random rather than derived — there is nothing about the process worth
// encoding here, and anything derived from the machine would be an identifier
// we did not intend to collect.
func NewSessionID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice, and a session with a constant
		// id is still better than a server that refuses to start over it.
		return "s_000000000000"
	}
	return "s_" + hex.EncodeToString(buf)
}
