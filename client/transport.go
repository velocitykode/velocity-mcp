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

// ProtocolAware is the optional hook a Transport implements to take part in
// protocol negotiation. The client calls UseProtocol before every frame with
// the version that frame belongs to, so a transport can adapt what it puts on
// the wire around it (an HTTP transport, for one, sends different headers per
// version and drops a session that belonged to another handshake).
//
// A transport that does not implement it keeps working: the version is simply
// not announced to it.
type ProtocolAware interface {
	// UseProtocol records the protocol version the following frames belong to.
	UseProtocol(version ProtocolVersion)
}

// HeaderSender is the optional hook a Transport implements to carry the
// per-request protocol headers that mirror a request frame (its method and the
// name of the primitive it addresses). Only a transport with a header channel
// can carry them; stdio, which has none, implements Send alone and the headers
// are dropped.
type HeaderSender interface {
	// SendWithHeaders transmits a frame together with the protocol headers.
	SendWithHeaders(ctx context.Context, message string, headers map[string]string) error
}

// AuthorizationAware is the optional hook a Transport implements when the frames
// it carries present a credential, as the HTTP transport presents a bearer
// token. What a server answers may depend on who is asking, so whatever the
// client keeps of its answers (the result a connection was settled with, the
// tool definitions a listing stated, the session a handshake opened) belongs to
// the credential it was fetched with. The specification forbids a result scoped
// to one authorization context from being reused in another, and a different
// access token is a different context.
//
// An authorization context is identified by a number the transport hands out:
// it changes whenever the credential differs from the one settled before, and
// stays as it is while the credential does. The credential itself never leaves
// the transport.
//
// A transport that does not implement it keeps working: everything it carries
// is taken to belong to the one context it was built with, which is what a
// transport with no credential of its own, such as stdio, has.
type AuthorizationAware interface {
	// SettleAuthorization resolves the credential the frames that follow
	// present and returns the authorization context it belongs to. The client
	// calls it at the start of every exchange, while it holds the exchange, and
	// every frame sent until it is called again presents the credential it
	// resolved: a credential that is looked up afresh for each frame could
	// change between the question of whose connection this is and the frame
	// that relies on the answer.
	SettleAuthorization() int64
	// AuthorizationContext returns the authorization context the credential
	// resolved now belongs to, which is the number SettleAuthorization would
	// return, and settles nothing: the frames in flight go on presenting what
	// SettleAuthorization resolved.
	AuthorizationContext() int64
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
