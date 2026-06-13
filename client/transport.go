package client

import (
	"context"
	"time"
)

// Transport is the byte-level channel between the client and an MCP server. A
// transport carries newline-free JSON-RPC frames in both directions; framing
// (newline-delimited for stdio, HTTP request/response for HTTP) is the
// transport's concern.
//
// Send and Receive form a request/response pair from the protocol's point of
// view: the protocol sends a request frame, then calls Receive until it reads
// the matching response. Implementations are not required to be safe for
// concurrent use; the protocol serializes access.
type Transport interface {
	// Connect establishes the underlying channel (spawns the subprocess, or
	// resets HTTP session state). It is idempotent.
	Connect(ctx context.Context) error
	// Disconnect tears the channel down and releases resources. It is safe to
	// call when not connected.
	Disconnect() error
	// Send transmits a single JSON-RPC frame.
	Send(ctx context.Context, message string) error
	// Receive returns the next available JSON-RPC frame, blocking until one is
	// available or the context/timeout elapses.
	Receive(ctx context.Context) (string, error)
	// SetTimeout sets the per-operation timeout used when the caller's context
	// carries no deadline.
	SetTimeout(d time.Duration)
	// Recipe returns a serializable description sufficient to rebuild the
	// transport (used by the client manager).
	Recipe() Recipe
}

// defaultTimeout is the per-operation timeout applied when neither the caller's
// context nor SetTimeout provides one.
const defaultTimeout = 30 * time.Second

// Recipe is a serializable description of a transport, used to rebuild a named
// client. Driver is "stdio" or "http"; the remaining fields are populated per
// driver.
type Recipe struct {
	Driver  string        `json:"driver"`
	URL     string        `json:"url,omitempty"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Token   string        `json:"token,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// TransportFromRecipe rebuilds a transport from its Recipe.
func TransportFromRecipe(r Recipe) (Transport, error) {
	switch r.Driver {
	case "stdio":
		if r.Command == "" {
			return nil, newError("invalid stdio transport recipe: missing command")
		}
		t := NewStdioTransport(r.Command, r.Args...)
		applyRecipeTimeout(t, r)
		return t, nil
	case "http":
		if r.URL == "" {
			return nil, newError("invalid http transport recipe: missing url")
		}
		t := NewHTTPTransport(r.URL)
		if r.Token != "" {
			t.WithToken(r.Token)
		}
		applyRecipeTimeout(t, r)
		return t, nil
	default:
		return nil, newError("unable to rebuild transport from an unknown recipe")
	}
}

// applyRecipeTimeout applies a recipe's timeout to a transport when set.
func applyRecipeTimeout(t Transport, r Recipe) {
	if r.Timeout > 0 {
		t.SetTimeout(r.Timeout)
	}
}
