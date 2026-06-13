package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// HandleResult is the outcome of handling a single inbound message. Exactly one
// of Response or nothing is produced: a notification yields a nil Response and
// HasResponse=false (no reply is sent), while a request yields a non-nil
// Response. SessionID is set for an initialize response so the transport can
// surface the session id (e.g. in the Mcp-Session-Id header).
type HandleResult struct {
	// Response is the JSON-RPC response to send back, or nil for a
	// notification (which produces no reply).
	Response *jsonrpc.Response
	// HasResponse reports whether a reply should be sent.
	HasResponse bool
	// SessionID is the newly assigned session id for an initialize response,
	// or "" for any other message.
	SessionID string
}

// Handle processes a single raw inbound JSON-RPC message and returns the result
// to send back. It routes inbound messages by method: parse error and
// invalid-request handling, notification short-circuit (no reply), the special
// initialize path (which dispatches SessionInitialized and assigns a session
// id), method lookup with MethodNotFound for unknown methods, and a generic
// internal-error fallback that never leaks internal detail to the client.
//
// ctx is threaded to the event dispatcher so listeners observe request-scoped
// values. sessionID is the transport's existing session id (for non-initialize
// requests); it may be empty.
func (s *Server) Handle(ctx context.Context, raw []byte, sessionID string) HandleResult {
	if ctx == nil {
		ctx = context.Background()
	}

	// A message without an id is a notification: validate it and send no reply.
	isNotification, perr := jsonrpc.IsNotificationBytes(raw)
	if perr != nil {
		var rpcErr *jsonrpc.Error
		if !errors.As(perr, &rpcErr) {
			rpcErr = jsonrpc.NewError(jsonrpc.CodeParseError, "Parse error: Invalid JSON was received by the server.")
		}
		return HandleResult{Response: jsonrpc.NewErrorResponse(jsonrpc.NullID(), rpcErr), HasResponse: true}
	}
	if isNotification {
		// Parse to surface a malformed notification as a parse/invalid error
		// per the spec, but a well-formed notification produces no response.
		if _, nerr := jsonrpc.ParseNotification(raw); nerr != nil {
			return HandleResult{Response: jsonrpc.NewErrorResponse(jsonrpc.NullID(), nerr), HasResponse: true}
		}
		return HandleResult{HasResponse: false}
	}

	req, id, rerr := jsonrpc.ParseRequest(raw)
	if rerr != nil {
		return HandleResult{Response: jsonrpc.NewErrorResponse(id, rerr), HasResponse: true}
	}

	sc := s.createContext(ctx, sessionID)

	if req.Method == "initialize" {
		return s.handleInitialize(ctx, sc, req)
	}

	method, ok := s.methods[req.Method]
	if !ok {
		return HandleResult{
			Response:    jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeMethodNotFound, "The method ["+req.Method+"] was not found."),
			HasResponse: true,
		}
	}

	if req.Method == "tools/call" {
		return s.handleToolCall(ctx, sc, req, method, sessionID)
	}

	resp, err := s.runMethod(sc, req, method)
	return HandleResult{Response: resp, HasResponse: true, SessionID: ""}.withError(req.ID, err)
}

// runMethod invokes a method handler and normalizes any error into a JSON-RPC
// response. A handler that returns a *jsonrpc.Error (wrapped or direct) yields a
// matching error response; any other error yields a generic internal error so
// no internal detail reaches the client.
func (s *Server) runMethod(sc *Context, req *jsonrpc.Request, method Method) (*jsonrpc.Response, error) {
	return method.Handle(sc, req)
}

// withError finalizes a HandleResult given a handler error. A nil error leaves
// the result unchanged; a *jsonrpc.Error becomes an error response; any other
// error becomes a generic internal error response.
func (r HandleResult) withError(id jsonrpc.ID, err error) HandleResult {
	if err == nil {
		if r.Response == nil {
			// A handler that returned (nil, nil) still owes a result; emit an
			// empty success rather than nothing.
			resp, _ := jsonrpc.NewResult(id, nil)
			r.Response = resp
		}
		return r
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		r.Response = jsonrpc.NewErrorResponse(id, rpcErr)
		return r
	}
	r.Response = jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeInternalError, "Something went wrong while processing the request.")
	return r
}

// handleInitialize runs the initialize method, assigns a session id, and
// dispatches the SessionInitialized event.
func (s *Server) handleInitialize(ctx context.Context, sc *Context, req *jsonrpc.Request) HandleResult {
	method, ok := s.methods["initialize"]
	if !ok {
		method = initializeMethod{}
	}

	resp, err := method.Handle(sc, req)
	res := HandleResult{Response: resp, HasResponse: true}.withError(req.ID, err)

	// Only assign a session and dispatch the event on a successful initialize.
	if res.Response != nil && res.Response.Error == nil {
		sessionID := s.sessionID()
		res.SessionID = sessionID

		params := decodeParams(req.Params)
		sc.SetNegotiatedVersion(stringParam(params, "protocolVersion"))
		s.dispatch(ctx, event.SessionInitialized{
			SessionID:          sessionID,
			ClientInfo:         clientInfoFromParams(params),
			ProtocolVersion:    stringParam(params, "protocolVersion"),
			ClientCapabilities: mapParam(params, "capabilities"),
		})
	}
	return res
}

// handleToolCall runs a tools/call request and dispatches ToolCalled on a
// completed call or ToolFailed when the handler errors. Event dispatch lives
// here (rather than in the methods package)
// because the Server owns the event dispatcher.
func (s *Server) handleToolCall(ctx context.Context, sc *Context, req *jsonrpc.Request, method Method, sessionID string) HandleResult {
	params := decodeParams(req.Params)
	toolName := stringParam(params, "name")
	args := mapParam(params, "arguments")

	start := time.Now()
	resp, err := method.Handle(sc, req)
	elapsed := time.Since(start)

	res := HandleResult{Response: resp, HasResponse: true}.withError(req.ID, err)

	if err != nil || (res.Response != nil && res.Response.Error != nil) {
		s.dispatch(ctx, event.ToolFailed{
			SessionID: sessionID,
			Tool:      toolName,
			Arguments: args,
			Err:       err,
			Duration:  elapsed,
		})
		return res
	}

	s.dispatch(ctx, event.ToolCalled{
		SessionID: sessionID,
		Tool:      toolName,
		Arguments: args,
		IsError:   resultIsError(res.Response),
		Duration:  elapsed,
	})
	return res
}

// resultIsError reports whether a successful tools/call response carries an
// "isError": true result (a tool-level error result, distinct from a
// protocol-level error response).
func resultIsError(resp *jsonrpc.Response) bool {
	if resp == nil || resp.Error != nil || len(resp.Result) == 0 {
		return false
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return false
	}
	return result.IsError
}

// decodeParams decodes a request's params object into a map, returning an empty
// map when params are absent or not an object.
func decodeParams(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// stringParam reads a string-valued key from a params map, or "".
func stringParam(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

// mapParam reads a map-valued key from a params map, or nil.
func mapParam(params map[string]any, key string) map[string]any {
	if v, ok := params[key].(map[string]any); ok {
		return v
	}
	return nil
}

// clientInfoFromParams extracts the clientInfo object from initialize params,
// or nil when absent.
func clientInfoFromParams(params map[string]any) *event.ClientInfo {
	info, ok := params["clientInfo"].(map[string]any)
	if !ok {
		return nil
	}
	ci := &event.ClientInfo{}
	if v, ok := info["name"].(string); ok {
		ci.Name = v
	}
	if v, ok := info["title"].(string); ok {
		ci.Title = v
	}
	if v, ok := info["version"].(string); ok {
		ci.Version = v
	}
	return ci
}

// randomSessionID returns a 128-bit hex-encoded random session id. crypto/rand
// is used so session ids are unguessable; on the vanishingly rare read error an
// empty string is returned rather than panicking (library code never panics).
func randomSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
