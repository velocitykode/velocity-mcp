package client

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// scriptRequestID is the id placeholder a scripted frame carries: the transport
// rewrites it to the id of the request in flight, so a script can be written
// without tracking ids. Any other id (including an explicit null) is left as
// written.
const scriptRequestID = `"@request"`

// scriptedTransport replays a script of raw response frames: every request is
// answered with the next entry. It records what the client sent, the protocol
// headers it attached, and the protocol version it announced for each frame, so
// a test can assert on the wire rather than on internals.
type scriptedTransport struct {
	mu          sync.Mutex
	responses   []string
	sent        []string
	headers     []map[string]string
	versions    []ProtocolVersion
	connects    int
	disconnects int
	connected   bool
	// failOn maps a method to the error the transport raises instead of
	// carrying the frame, as a broken channel would.
	failOn map[string]error
	// failOnce is failOn for a channel that recovers: the entry is spent the
	// first time it fires, so the next frame of the same method is carried.
	failOnce map[string]error
	// sendErr is raised (once) by the next send, whatever the method.
	sendErr error
	// beforeSend runs just before a frame is carried, so a test can change the
	// world at a precise point of an exchange (withdraw the caller's context
	// while a handshake is in flight, for instance).
	beforeSend func(method string)
	pendingID  jsonrpc.ID
}

var (
	_ Transport     = (*scriptedTransport)(nil)
	_ ProtocolAware = (*scriptedTransport)(nil)
	_ HeaderSender  = (*scriptedTransport)(nil)
)

func newScriptedTransport(responses ...string) *scriptedTransport {
	return &scriptedTransport{
		responses: responses,
		failOn:    map[string]error{},
		failOnce:  map[string]error{},
	}
}

// headlessTransport is a scripted transport without a header channel, which is
// what a transport such as stdio is: the protocol has nowhere to put a mirrored
// header on it. It delegates rather than embeds, so it cannot pick up the
// header hook by accident.
type headlessTransport struct{ scripted *scriptedTransport }

var _ Transport = (*headlessTransport)(nil)

func newHeadlessTransport(responses ...string) *headlessTransport {
	return &headlessTransport{scripted: newScriptedTransport(responses...)}
}

func (t *headlessTransport) Connect(ctx context.Context) error { return t.scripted.Connect(ctx) }
func (t *headlessTransport) Disconnect() error                 { return t.scripted.Disconnect() }
func (t *headlessTransport) SetTimeout(d time.Duration)        { t.scripted.SetTimeout(d) }
func (t *headlessTransport) Recipe() Recipe                    { return t.scripted.Recipe() }

func (t *headlessTransport) Send(ctx context.Context, message string) error {
	return t.scripted.Send(ctx, message)
}

func (t *headlessTransport) Receive(ctx context.Context) (string, error) {
	return t.scripted.Receive(ctx)
}

func (s *scriptedTransport) Connect(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connects++
	s.connected = true
	return nil
}

func (s *scriptedTransport) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnects++
	s.connected = false
	return nil
}

// lifecycle returns how many times the transport was connected and
// disconnected, and whether it is up right now.
func (s *scriptedTransport) lifecycle() (connects, disconnects int, connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connects, s.disconnects, s.connected
}

// script appends frames to the script the transport replays, and forgets what
// the client has sent so far.
func (s *scriptedTransport) script(responses ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = nil
	s.headers = nil
	s.responses = append(s.responses, responses...)
}

func (s *scriptedTransport) SetTimeout(time.Duration) {}

func (s *scriptedTransport) Recipe() Recipe { return Recipe{Driver: "scripted"} }

func (s *scriptedTransport) UseProtocol(version ProtocolVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions = append(s.versions, version)
}

// SendWithHeaders carries a frame with its headers. The headers are recorded
// only once the frame itself is, so headersAt(i) is the header set of the frame
// at i however many sends the channel refused in between.
func (s *scriptedTransport) SendWithHeaders(ctx context.Context, message string, headers map[string]string) error {
	if err := s.Send(ctx, message); err != nil {
		return err
	}
	s.mu.Lock()
	s.headers = append(s.headers, headers)
	s.mu.Unlock()
	return nil
}

func (s *scriptedTransport) Send(_ context.Context, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.sendErr; err != nil {
		s.sendErr = nil
		s.connected = false
		return err
	}

	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(message), &probe)

	if s.beforeSend != nil {
		s.beforeSend(probe.Method)
	}

	if err, ok := s.failOnce[probe.Method]; ok {
		delete(s.failOnce, probe.Method)
		s.connected = false
		return err
	}
	if err, ok := s.failOn[probe.Method]; ok {
		s.connected = false
		return err
	}
	if len(probe.ID) > 0 {
		_ = s.pendingID.UnmarshalJSON(probe.ID)
	}

	s.sent = append(s.sent, message)
	return nil
}

func (s *scriptedTransport) Receive(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) == 0 {
		return "", newError("scripted transport: the script ran out of responses")
	}
	raw := s.responses[0]
	s.responses = s.responses[1:]
	return s.answering(raw), nil
}

// answering rewrites the id placeholder of a scripted frame to the id of the
// request in flight.
func (s *scriptedTransport) answering(raw string) string {
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		return raw
	}
	if string(members["id"]) != scriptRequestID {
		return raw
	}
	members["id"] = s.pendingID.Raw()
	out, err := json.Marshal(members)
	if err != nil {
		return raw
	}
	return string(out)
}

// methods returns the JSON-RPC method of every frame the client sent, in order.
func (s *scriptedTransport) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make([]string, 0, len(s.sent))
	for _, frame := range s.sent {
		methods = append(methods, frameMethod(frame))
	}
	return methods
}

// frame returns the decoded request frame at index, failing the test when the
// client did not send that many.
func (s *scriptedTransport) frame(t *testing.T, index int) sentFrame {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.sent) {
		t.Fatalf("no frame at index %d; sent %d frame(s)", index, len(s.sent))
	}
	var decoded sentFrame
	if err := json.Unmarshal([]byte(s.sent[index]), &decoded); err != nil {
		t.Fatalf("frame %d is not valid JSON: %v", index, err)
	}
	return decoded
}

// rawMember returns a member of the frame at index exactly as it went on the
// wire, or the empty string when the frame carries no such member.
func (s *scriptedTransport) rawMember(t *testing.T, index int, member string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.sent) {
		t.Fatalf("no frame at index %d; sent %d frame(s)", index, len(s.sent))
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s.sent[index]), &members); err != nil {
		t.Fatalf("frame %d is not valid JSON: %v", index, err)
	}
	return string(members[member])
}

// versionsAnnounced returns the protocol version the client announced ahead of
// each frame it sent, in order.
func (s *scriptedTransport) versionsAnnounced() []ProtocolVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ProtocolVersion(nil), s.versions...)
}

// headersAt returns the protocol headers attached to the frame at index.
func (s *scriptedTransport) headersAt(index int) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.headers) {
		return nil
	}
	return s.headers[index]
}

// sentFrame is a request frame as the server would decode it.
type sentFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

// meta returns the protocol metadata of the frame, or nil when it carries none.
func (f sentFrame) meta() map[string]any {
	meta, _ := f.Params["_meta"].(map[string]any)
	return meta
}

func frameMethod(frame string) string {
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(frame), &probe)
	return probe.Method
}

// discoverFrame builds a server/discover result advertising the given versions,
// with the caching hints the specification requires a server to state on it.
func discoverFrame(versions ...string) string {
	return discoverFrameStating("Be nice.", scriptedLifetimeMs, versions...)
}

// discoverFrameStating is discoverFrame advertising the given instructions and
// stating the given ttlMs member, which is written as given. A nil lifetime
// leaves the member out, as a server that predates the hints does.
func discoverFrameStating(instructions string, lifetime any, versions ...string) string {
	result := map[string]any{
		"supportedVersions": versions,
		"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
		"instructions":      instructions,
		"cacheScope":        "private",
		"_meta": map[string]any{
			MetaServerInfo: map[string]any{"name": "Test Server", "version": "1.0.0"},
		},
	}
	if lifetime != nil {
		result["ttlMs"] = lifetime
	}
	return resultFrame(result)
}

// initializeFrame builds an initialize result settling on version.
func initializeFrame(version string) string {
	return resultFrame(map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"serverInfo":      map[string]any{"name": "Test Server", "version": "1.0.0"},
		"instructions":    "Be nice.",
	})
}

// resultFrame builds a success response carrying result.
func resultFrame(result map[string]any) string {
	out, err := json.Marshal(map[string]any{
		"jsonrpc": jsonrpc.Version,
		"id":      json.RawMessage(scriptRequestID),
		"result":  result,
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// errorFrame builds an error response with the given code, message, and
// optional data.
func errorFrame(code int, message string, data map[string]any) string {
	object := map[string]any{"code": code, "message": message}
	if data != nil {
		object["data"] = data
	}
	out, err := json.Marshal(map[string]any{
		"jsonrpc": jsonrpc.Version,
		"id":      json.RawMessage(scriptRequestID),
		"error":   object,
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// nullIDErrorFrame builds an error response that cannot be correlated to a
// request, as a framing layer in front of the server would emit.
func nullIDErrorFrame(code int, message string) string {
	out, err := json.Marshal(map[string]any{
		"jsonrpc": jsonrpc.Version,
		"id":      nil,
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// methodNotFoundFrame builds the rejection a server without the discovery
// handshake returns for server/discover.
func methodNotFoundFrame() string {
	return errorFrame(jsonrpc.CodeMethodNotFound, "Method not found.", nil)
}
