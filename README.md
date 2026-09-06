# mcpulse-go

Analytics for MCP servers, for the [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk).

One import, one wrap — see which of your tools actually work for the models
calling them.

```go
import (
    "github.com/modelcontextprotocol/go-sdk/mcp"
    mcpulse "github.com/getmcpulse/mcpulse-go"
)

server := mcp.NewServer(&mcp.Implementation{Name: "my-server"}, nil)
mcpulse.Watch(server, mcpulse.Options{Key: "mp_live_…"})
```

The server is instrumented in place and handed straight back, so the call drops
in around an existing server without moving anything else. It attaches through
the SDK's public `AddReceivingMiddleware` — nothing here reaches for an
unexported field.

## Install

```bash
go get github.com/getmcpulse/mcpulse-go
```

## Options

| Field | Default | Meaning |
|---|---|---|
| `Key` | — | Ingest key, `mp_live_…`, minted per MCP in the dashboard |
| `Endpoint` | `https://api.getmcpulse.com` | Point at a local API while developing |
| `Disabled` | `false` | `true` makes `Watch` a no-op — useful in tests and CI |
| `Debug` | `false` | Log what is sent, and why a send failed, to **stderr** |

`Disabled` is negative rather than an `Enabled` bool so the zero value of
`Options` is the working one. An empty `Key` also turns it off, so a server
started without its key configured is silent rather than a source of 401s.

## Two things Go needs that other SDKs do not

### `WrapTool`, for the full outcome split

```go
mcp.AddTool(server, tool, mcpulse.WrapTool(myHandler))
```

Optional, and what it buys is precision. The Go SDK validates arguments against
the input schema and, on failure, returns a `CallToolResult` with `IsError` set
— byte for byte what a handler returning an error produces. From outside the
handler the two are identical, and reading the difference back out of an error
string is not an interface anyone promised to keep.

Wrap your handlers and MCPulse reports `bad_args`, `crashed` and `tool_error`
separately. Don't, and validation failures arrive as `tool_error`. Everything
else — timing, argument hashes, response sizes, emptiness — works from `Watch`
alone either way.

### `FlushAll`, if you shut down without a signal

```go
defer mcpulse.FlushAll()
```

Go has no `atexit`. SIGINT and SIGTERM are handled for you (and forwarded on,
so your own handler still runs), but a server that returns from `main` on its
own needs this line, or the last few seconds of calls go unsent.

## What leaves your process

Sizes and hashes. Arguments and results do not, and no option turns that on.

Arguments are hashed straight from the raw wire bytes the client sent, before
defaults are applied or unknown fields are dropped.

## The three rules

1. **Never throw.** Every entry point recovers. If MCPulse fails inside your
   tool call, your tool fails and you blame us.
2. **Never block.** The wake channel is buffered and every send to it is a
   select-with-default, so a request path can never wait on the sender.
3. **Never store customer data.** See above.

## Cross-language consistency

`ArgsHash` is the first 12 hex characters of the SHA-256 of the
[RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) canonical form of the
arguments. `testdata/canonical.json` is the shared conformance suite every
MCPulse SDK runs, so a call hashed here and a call hashed by the TypeScript or
Python SDK land in the same bucket.

Getting there took undoing three Go defaults: `encoding/json` HTML-escapes `<`,
`>` and `&`; map keys sort by UTF-8 bytes where JCS sorts by UTF-16 code unit;
and `strconv` writes `1e-07` where ECMAScript writes `1e-7`.

## Licence

MIT
