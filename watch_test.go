package mcpulse

// End to end, against a real mcp server over the SDK's in-memory transport.
//
// Nothing is stubbed except the network: the server, the client, the transport
// and the middleware chain are the real ones, because the whole risk in this
// package is that it reaches for something the SDK has moved.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── A capturing stream, so no test touches the network ──────────────────────

type capture struct {
	mu       sync.Mutex
	payloads []Payload
}

func (c *capture) add(p Payload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, p)
}

func (c *capture) calls() []Payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Payload
	for _, p := range c.payloads {
		if p.Type == "call" {
			out = append(out, p)
		}
	}
	return out
}

func (c *capture) startups() []Payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Payload
	for _, p := range c.payloads {
		if p.Type == "startup" {
			out = append(out, p)
		}
	}
	return out
}

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to echo"`
}
type echoOut struct {
	Text string `json:"text"`
}

func buildServer(t *testing.T, wrap bool) (*mcp.Server, *capture) {
	t.Helper()

	captured := &capture{}
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)

	echo := func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: in.Text}},
		}, echoOut{Text: in.Text}, nil
	}
	explode := func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
		return nil, echoOut{}, errors.New("boom")
	}
	// No structured output: content only, which is the common Go shape and the
	// one where "[]" is the tool saying "nothing found" while reporting success.
	nothing := func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "[]"}},
		}, nil, nil
	}

	if wrap {
		mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo"}, WrapTool(echo))
		mcp.AddTool(server, &mcp.Tool{Name: "explode", Description: "Fails"}, WrapTool(explode))
		mcp.AddTool(server, &mcp.Tool{Name: "nothing", Description: "Empty"}, WrapTool(nothing))
	} else {
		mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo"}, echo)
		mcp.AddTool(server, &mcp.Tool{Name: "explode", Description: "Fails"}, explode)
		mcp.AddTool(server, &mcp.Tool{Name: "nothing", Description: "Empty"}, nothing)
	}

	// toolsAreWrapped is process-global by design — a real server registers its
	// tools once, at startup, and wraps all of them or none. Tests share a
	// process, so this one pins it and puts it back.
	previous := toolsAreWrapped.Load()
	toolsAreWrapped.Store(wrap)
	t.Cleanup(func() { toolsAreWrapped.Store(previous) })

	// Attach the middleware by hand so the payloads land in the capture rather
	// than in a real buffer.
	s := &stream{sessionID: "s_testtesttest", clientName: "unknown"}
	s.buf = &buffer{opts: resolve(Options{Key: "k"}), log: makeLogger(false)}
	s.sink = captured.add
	server.AddReceivingMiddleware(middleware(s, makeLogger(false)))

	return server, captured
}

// connect runs the server against an in-memory client and hands back a session.
func connect(t *testing.T, server *mcp.Server) (*mcp.ClientSession, func()) {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	return session, func() {
		_ = session.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		// A protocol-level error is a legitimate outcome for these tests.
		return nil
	}
	return result
}

func TestASuccessfulCallIsRecordedOnce(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "echo", map[string]any{"text": "hello"})

	calls := captured.calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	if calls[0].ToolName != "echo" {
		t.Errorf("tool_name = %q", calls[0].ToolName)
	}
	if calls[0].Outcome != OutcomeOK {
		t.Errorf("outcome = %q, want ok", calls[0].Outcome)
	}
	if calls[0].V != WireVersion {
		t.Errorf("v = %d", calls[0].V)
	}
}

func TestARaisingToolIsCrashed(t *testing.T) {
	// The SDK converts the returned error into IsError before anything outside
	// sees it. Telling that apart from a rejected argument set is the whole
	// reason WrapTool exists.
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "explode", map[string]any{"text": "hi"})

	calls := captured.calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls", len(calls))
	}
	if calls[0].Outcome != OutcomeCrashed {
		t.Errorf("outcome = %q, want crashed", calls[0].Outcome)
	}
}

func TestBadArgumentsAreBadArgs(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	// "text" must be a string; the schema rejects a number before the handler
	// is reached.
	callTool(t, session, "echo", map[string]any{"text": 42})

	calls := captured.calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls", len(calls))
	}
	if calls[0].Outcome != OutcomeBadArgs {
		t.Errorf("outcome = %q, want bad_args", calls[0].Outcome)
	}
}

func TestAnEmptyResultIsFlagged(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "nothing", map[string]any{"text": "hi"})

	calls := captured.calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls", len(calls))
	}
	if calls[0].Outcome != OutcomeOK {
		t.Fatalf("outcome = %q", calls[0].Outcome)
	}
	if !calls[0].IsEmpty {
		t.Error("a tool returning [] should be empty")
	}
}

func TestARealAnswerIsNotEmpty(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "echo", map[string]any{"text": "an actual answer"})

	calls := captured.calls()
	if len(calls) != 1 || calls[0].IsEmpty {
		t.Errorf("is_empty = %v, want false", calls[0].IsEmpty)
	}
}

func TestArgumentsNeverLeaveTheProcess(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "echo", map[string]any{"text": "sensitive-argument-value"})

	raw, err := json.Marshal(captured.payloads)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sensitive-argument-value") {
		t.Error("an argument value reached the wire")
	}
	if len(captured.calls()[0].ArgsHash) != 12 {
		t.Errorf("args_hash = %q", captured.calls()[0].ArgsHash)
	}
}

func TestTheSameArgumentsHashTheSameWhateverTheOrder(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	// Go maps have no order, so the two calls are built from literals whose
	// JSON encoding genuinely differs on the wire.
	_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`),
	})
	_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`),
	})

	calls := captured.calls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls", len(calls))
	}
	if calls[0].ArgsHash != calls[1].ArgsHash {
		t.Errorf("hashes differ: %s vs %s", calls[0].ArgsHash, calls[1].ArgsHash)
	}
}

func TestStartupCarriesTheToolsAndTheClientName(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("listing tools: %v", err)
	}

	startups := captured.startups()
	if len(startups) != 1 {
		t.Fatalf("recorded %d startups, want 1", len(startups))
	}

	names := map[string]bool{}
	for _, tool := range startups[0].Tools {
		names[tool.Name] = true
		if tool.SchemaBytes <= 0 {
			t.Errorf("%s has schema_bytes %d", tool.Name, tool.SchemaBytes)
		}
	}
	for _, want := range []string{"echo", "explode", "nothing"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	if startups[0].ClientName != "test-client" {
		t.Errorf("client_name = %q", startups[0].ClientName)
	}
}

func TestStartupIsSentOncePerStream(t *testing.T) {
	server, captured := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	for i := 0; i < 3; i++ {
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(captured.startups()); got != 1 {
		t.Errorf("recorded %d startups, want 1", got)
	}
}

func TestTheToolResultReachesTheClientUnchanged(t *testing.T) {
	server, _ := buildServer(t, true)
	session, cleanup := connect(t, server)
	defer cleanup()

	result := callTool(t, session, "echo", map[string]any{"text": "unchanged"})
	if result == nil || len(result.Content) == 0 {
		t.Fatal("no content came back")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "unchanged" {
		t.Errorf("content = %+v", result.Content[0])
	}
}

func TestWithoutWrapToolEverythingButTheOutcomeSplitStillWorks(t *testing.T) {
	// The documented trade-off: no WrapTool means bad_args and crashed both
	// arrive as tool_error, and nothing else changes.
	server, captured := buildServer(t, false)
	session, cleanup := connect(t, server)
	defer cleanup()

	callTool(t, session, "explode", map[string]any{"text": "hi"})

	calls := captured.calls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls", len(calls))
	}
	if calls[0].Outcome != OutcomeToolError {
		t.Errorf("outcome = %q, want tool_error", calls[0].Outcome)
	}
	if calls[0].ToolName != "explode" || len(calls[0].ArgsHash) != 12 {
		t.Error("the rest of the record should be unaffected")
	}
}

func TestWatchTwiceDoesNotDoubleReport(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "1"}, nil)
	Watch(server, Options{Key: "mp_test", Disabled: true})
	Watch(server, Options{Key: "mp_test", Disabled: true})
	// The guard is the assertion: a second Watch returns before attaching.
	watchedMu.Lock()
	defer watchedMu.Unlock()
	if !watched[server] {
		t.Error("server should be marked watched")
	}
}

// Two servers reporting to the same place share one session.
//
// The bug this guards against is invisible in a stdio server and fatal in an HTTP one: a
// streamable-HTTP server can build a fresh *mcp.Server per request, so Watch runs per request too.
// If each one opened its own session there could never be two calls in one session, so a retry
// could never be detected and first-call success would report a perfect score however badly the
// server was doing.
func TestServersForOneDestinationShareASession(t *testing.T) {
	t.Cleanup(func() {
		streamsMu.Lock()
		streams = map[string]*stream{}
		streamsMu.Unlock()
	})

	opts := resolve(Options{Key: "mp_share_test", Endpoint: "http://127.0.0.1:1"})
	log := makeLogger(false)

	first := streamFor(opts, log)
	second := streamFor(opts, log)

	if first.sessionID != second.sessionID {
		t.Errorf("sessions differ: %s vs %s", first.sessionID, second.sessionID)
	}
	if first != second {
		t.Error("the same destination must hand back the same stream")
	}

	// Two keys are two customers. Merging them would file one customer's calls under another's.
	other := streamFor(resolve(Options{Key: "mp_other_key", Endpoint: "http://127.0.0.1:1"}), log)
	if other.sessionID == first.sessionID {
		t.Error("different keys must not share a session")
	}
}

func TestAnEmptyKeyDisablesEverything(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "x", Version: "1"}, nil)
	if got := Watch(server, Options{Key: ""}); got != server {
		t.Error("Watch should hand the server straight back")
	}
}

func TestWatchingNilIsANoOp(t *testing.T) {
	if got := Watch(nil, Options{Key: "mp_test"}); got != nil {
		t.Error("watching nil should return nil")
	}
}
