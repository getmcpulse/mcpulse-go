package mcpulse

// Attaching MCPulse to a Go MCP server.
//
// The Go SDK has a public middleware API — AddReceivingMiddleware takes a
// func(MethodHandler) MethodHandler — so nothing here reaches for an
// unexported field. That covers timing, the argument hash, response size and
// emptiness for every tool, with one line of setup.
//
// Telling crashed from bad_args needs one thing middleware cannot give. See
// WrapTool below.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// watched guards against a second Watch on the same server, which would open a
// second session and report every call under both — doubling the customer's
// numbers and their bill.
var (
	watchedMu sync.Mutex
	watched   = map[*mcp.Server]bool{}
)

// Watch starts recording what this MCP server does.
//
//	server := mcp.NewServer(&mcp.Implementation{Name: "my-server"}, nil)
//	mcpulse.Watch(server, mcpulse.Options{Key: "mp_live_…"})
//
// The server is instrumented in place and handed straight back, so the call can
// be dropped around an existing one without moving anything else.
//
// Nothing here is allowed to break the server it is measuring. If anything goes
// wrong attaching, the server is returned untouched and the process carries on
// without analytics, because a customer's tool failing over our telemetry is
// worse than no telemetry.
func Watch(server *mcp.Server, opts Options) *mcp.Server {
	if server == nil {
		return server
	}

	resolvedOpts := resolve(opts)
	log := makeLogger(resolvedOpts.debug)

	watchedMu.Lock()
	if watched[server] {
		watchedMu.Unlock()
		log("already watching this server")
		return server
	}
	watched[server] = true
	watchedMu.Unlock()

	if !resolvedOpts.enabled {
		log("disabled — no key, or Disabled: true")
		return server
	}

	// Shared across every Watch in this process that reports to the same place.
	// See session.go.
	s := streamFor(resolvedOpts, log)
	server.AddReceivingMiddleware(middleware(s, log))
	log("watching")

	return server
}

// ─── The middleware ──────────────────────────────────────────────────────────

func middleware(s *stream, log logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// Every inbound request is a chance to learn who the client is.
			// See rememberClient for why it cannot simply be read once.
			rememberClient(s, req)

			if method != "tools/call" {
				result, err := next(ctx, method, req)
				switch method {
				case "tools/list":
					// Startup rides on the tools/list response rather than on
					// initialize. The server has no public way to enumerate its
					// own tools, and this is the reply that carries them —
					// already converted to exactly the JSON the client sees,
					// which is what schema_bytes has to be measured on. Every
					// client calls it immediately after initialising.
					sendStartup(s, result, log)
				}
				return result, err
			}

			call, ok := req.(*mcp.CallToolRequest)
			if !ok {
				// A shape this version does not recognise. Pass it straight
				// through rather than guessing.
				return next(ctx, method, req)
			}

			// The slot rides on the context so a wrapped handler can report
			// what it did. Per request by construction, so two tools running
			// concurrently cannot claim each other's result.
			slot := &callSlot{}
			ctx = withSlot(ctx, slot)

			startedAt := time.Now().UTC()
			result, err := next(ctx, method, req)

			record(s, slot, call, result, err, startedAt)
			return result, err
		}
	}
}

func record(s *stream, slot *callSlot, call *mcp.CallToolRequest, result mcp.Result, err error, startedAt time.Time) {
	// Recording must never be the reason a tool call fails.
	defer func() { _ = recover() }()

	toolResult, _ := result.(*mcp.CallToolResult)
	outcome := decideOutcome(slot, toolResult, err)

	name := "unknown"
	var rawArgs json.RawMessage
	if call.Params != nil {
		if call.Params.Name != "" {
			name = call.Params.Name
		}
		rawArgs = call.Params.Arguments
	}

	s.emit(Payload{
		V:          WireVersion,
		Type:       "call",
		SessionID:  s.sessionID,
		ClientName: s.client(),
		ToolName:   truncate(name, maxToolName),
		StartedAt:  startedAt.Format(time.RFC3339Nano),
		DurationMS: time.Since(startedAt).Milliseconds(),
		Outcome:    outcome,
		// Hashed from the raw wire bytes, which is what the client actually
		// sent — before defaults are applied or unknown fields are dropped.
		ArgsHash:      ArgsHashRaw(rawArgs),
		ResponseBytes: responseBytes(toolResult),
		// An error is not also an absence — it has its own outcome already.
		IsEmpty: outcome == OutcomeOK && isEmptyResult(toolResult),
	})
}

// rememberClient reads the client's name off the session, on every request
// until it has one.
//
// The TypeScript SDK takes the name straight off the initialize request. That
// is not available here: Go's receiving middleware runs only on post-handshake
// methods, so initialize never reaches it — the first thing this package sees
// is a tools/list or a tools/call. By then the handshake has settled and
// ServerSession.InitializeParams is populated, which makes the session the
// right place to ask rather than the second-best one.
//
// Checked on every request rather than once, because an HTTP server serves
// more than one client from the same stream and the last identification wins,
// exactly as the shared session already assumes.
func rememberClient(s *stream, req mcp.Request) {
	defer func() { _ = recover() }()

	session, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || session == nil {
		return
	}
	params := session.InitializeParams()
	if params == nil || params.ClientInfo == nil || params.ClientInfo.Name == "" {
		return
	}
	s.setClient(truncate(params.ClientInfo.Name, maxClientName))
}

// sendStartup reports the tool list once per stream, reading it off the
// server's own tools/list reply.
//
// schema_bytes is the cost of a tool's presence in the context window, so it
// has to be measured on the JSON that actually goes over the wire, not on the
// Go struct the tool was declared from. This result is that JSON.
func sendStartup(s *stream, result mcp.Result, log logger) {
	defer func() { _ = recover() }()

	listed, ok := result.(*mcp.ListToolsResult)
	if !ok || listed == nil {
		return
	}
	if !s.claimStartup() {
		return
	}

	tools := make([]ToolInfo, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		if tool == nil || tool.Name == "" {
			continue
		}
		tools = append(tools, ToolInfo{Name: tool.Name, SchemaBytes: schemaBytes(tool)})
		if len(tools) >= maxTools {
			break
		}
	}

	s.emit(Payload{
		V:          WireVersion,
		Type:       "startup",
		SessionID:  s.sessionID,
		ClientName: s.client(),
		Tools:      tools,
	})
	log("startup: %d tools, client %s", len(tools), s.client())
}

func schemaBytes(tool *mcp.Tool) int {
	raw, err := json.Marshal(tool)
	if err != nil {
		return 0
	}
	return utf16Length(string(raw))
}

// responseBytes is what this response costs the context window. Unserialisable
// means unmeasurable.
func responseBytes(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return 0
	}
	return utf16Length(string(raw))
}

// ─── Telling the four outcomes apart ─────────────────────────────────────────

// callSlot is what a wrapped handler reports into.
type callSlot struct {
	mu         sync.Mutex
	handlerRan bool
	outcome    Outcome
}

// toolsAreWrapped records whether the author used WrapTool at all.
//
// It has to be set at registration rather than at call time: a call rejected by
// schema validation never reaches the wrapped handler, and that is precisely
// the case the flag is needed to interpret. Without it, "the handler did not
// run" is ambiguous between "validation rejected the arguments" and "this SDK
// was never given a way to know".
var toolsAreWrapped atomic.Bool

type slotKey struct{}

func withSlot(ctx context.Context, slot *callSlot) context.Context {
	return context.WithValue(ctx, slotKey{}, slot)
}

func slotFrom(ctx context.Context) *callSlot {
	slot, _ := ctx.Value(slotKey{}).(*callSlot)
	return slot
}

// WrapTool wraps a tool handler so MCPulse can tell a crash from a rejected
// argument set.
//
//	mcp.AddTool(server, tool, mcpulse.WrapTool(myHandler))
//
// This is optional, and what it buys is precision. The Go SDK validates
// arguments against the input schema and, on failure, returns a CallToolResult
// with IsError set — the same shape a handler that returned an error produces.
// From outside the handler the two are identical, and reading the difference
// back out of an error string is not an interface anyone promised to keep.
//
// Without it a call that failed validation is reported as tool_error rather
// than bad_args, and nothing else changes: timing, argument hashes, response
// sizes and emptiness all work from Watch alone.
func WrapTool[In, Out any](handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	toolsAreWrapped.Store(true)

	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (result *mcp.CallToolResult, out Out, err error) {
		slot := slotFrom(ctx)
		if slot == nil {
			// Not watched, or a context that did not come through the
			// middleware. Run the handler and stay out of the way.
			return handler(ctx, req, in)
		}

		slot.mu.Lock()
		slot.handlerRan = true
		slot.mu.Unlock()

		defer func() {
			if recovered := recover(); recovered != nil {
				slot.mu.Lock()
				slot.outcome = OutcomeCrashed
				slot.mu.Unlock()
				// The panic is re-raised untouched: swallowing it would change
				// what the customer's server does.
				panic(recovered)
			}
		}()

		result, out, err = handler(ctx, req, in)

		slot.mu.Lock()
		switch {
		case err != nil:
			slot.outcome = OutcomeCrashed
		case result != nil && result.IsError:
			slot.outcome = OutcomeToolError
		default:
			slot.outcome = OutcomeOK
		}
		slot.mu.Unlock()

		return result, out, err
	}
}

// decideOutcome is what the request layer concludes, given what the handler
// layer reported.
func decideOutcome(slot *callSlot, res *mcp.CallToolResult, err error) Outcome {
	if slot != nil {
		slot.mu.Lock()
		reported := slot.outcome
		ran := slot.handlerRan
		slot.mu.Unlock()

		if reported != "" {
			return reported
		}

		// The handler never ran, so whatever went wrong went wrong on the way
		// in. The Go SDK reports a schema rejection as a CallToolResult with
		// IsError set and no error returned, which is byte-for-byte what a
		// failing handler produces — handlerRan is the only thing that tells
		// them apart, and it is why WrapTool exists.
		//
		// Only trustworthy when the author actually wrapped their handlers.
		// Otherwise handlerRan is false for every call and reading it would
		// report every tool error in the server as bad_args.
		if !ran && toolsAreWrapped.Load() {
			if err != nil || (res != nil && res.IsError) {
				return OutcomeBadArgs
			}
		} else if ran && err != nil {
			return OutcomeCrashed
		}
	}

	if err != nil {
		return OutcomeCrashed
	}
	if res != nil && res.IsError {
		// Without a wrapped handler this is as far as the distinction goes: a
		// validation failure and a handler error reach here identically. See
		// WrapTool.
		return OutcomeToolError
	}
	return OutcomeOK
}
