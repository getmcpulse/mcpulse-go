package mcpulse

import (
	"fmt"
	"os"
)

// logf writes debug output to stderr.
//
// stdout is the transport for a stdio MCP server — a single stray line there
// corrupts the JSON-RPC stream and takes the customer's server down with it.
// This is the one thing in the package that would be trivially easy to get
// wrong and catastrophic to ship, so it goes through one function.
//
// Deliberately not log.Printf: the standard logger writes to stderr today, but
// it is process-global and a customer is free to point it at stdout.
type logger func(format string, args ...any)

func makeLogger(debug bool) logger {
	if !debug {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[mcpulse] "+format+"\n", args...)
	}
}
