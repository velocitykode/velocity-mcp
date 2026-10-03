package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers what the client does with a request the server initiates,
// which depends on the revision of the connection. The initialize-era revisions
// let either peer send a request, so a server's ping is answered and anything
// else declined with method-not-found. The 2026-07-28 revision forbids a server
// to send requests and a client to send responses, on the stdio binding and the
// streamable HTTP binding alike, so a request arriving over such a connection
// fails the exchange and nothing is written back.

// responsesWritten returns the frames among sent that are JSON-RPC responses:
// frames carrying a result or an error and no method.
func responsesWritten(sent []string) []string {
	var responses []string
	for _, frame := range sent {
		var probe struct {
			Method *string         `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(frame), &probe); err != nil {
			continue
		}
		if probe.Method == nil && (len(probe.Result) > 0 || len(probe.Error) > 0) {
			responses = append(responses, frame)
		}
	}
	return responses
}

// serverRequestFrame builds a request the server initiates.
func serverRequestFrame(id, method string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `"}`
}

// TestAServerRequestOnTheDiscoveryRevisionIsRefusedUnanswered asserts that over
// a 2026-07-28 connection a request from the server fails the exchange, names
// the request and the revision in the error, and has the client write no
// response, ping included. The connection goes down: a server that sends what
// the revision forbids has a stream the client can no longer keep in step with.
func TestAServerRequestOnTheDiscoveryRevisionIsRefusedUnanswered(t *testing.T) {
	for _, method := range []string{"sampling/createMessage", "ping", "roots/list"} {
		t.Run(method, func(t *testing.T) {
			f := newFakeTransport()
			f.on("server/discover", discoverHandler(LatestProtocolVersion))
			f.framesBefore["prompts/list"] = []string{serverRequestFrame(`"srv-1"`, method)}
			f.on("prompts/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
				resp, _ := jsonrpc.NewResult(id, map[string]any{"resultType": "complete", "prompts": []any{}})
				return resp
			})
			c := newTestClient(f)

			_, err := c.Prompts(context.Background())
			want := "the server sent a [" + method + "] request over a connection of protocol version [" +
				LatestProtocolVersion + "], which forbids it; this client sends no response"
			if err == nil || err.Error() != want {
				t.Fatalf("prompts = %v, want %q", err, want)
			}
			f.mu.Lock()
			responses := responsesWritten(f.sent)
			f.mu.Unlock()
			if len(responses) != 0 {
				t.Fatalf("the client wrote %d response(s) on a connection that forbids it: %v", len(responses), responses)
			}
			if c.Connected() {
				t.Fatal("the connection stood after the server broke the revision's rules")
			}
		})
	}
}

// TestAServerRequestOverHTTPOnTheDiscoveryRevisionIsNeverPosted asserts the
// same over streamable HTTP, however the server delivers the request: in the
// JSON body of the reply, as one event of the stream, or as an event written
// across several data lines. No response is POSTed back.
func TestAServerRequestOverHTTPOnTheDiscoveryRevisionIsNeverPosted(t *testing.T) {
	request := serverRequestFrame(`"srv-1"`, "roots/list")
	deliveries := []struct {
		name  string
		reply func(w http.ResponseWriter, id json.RawMessage)
	}{
		{
			name: "in the JSON body",
			reply: func(w http.ResponseWriter, _ json.RawMessage) {
				writeJSON(w, http.StatusOK, request)
			},
		},
		{
			name: "as an event of the stream",
			reply: func(w http.ResponseWriter, id json.RawMessage) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+request+"\n\n")
				_, _ = io.WriteString(w, "data: "+jsonFrame(id, map[string]any{"result": map[string]any{
					"resultType": "complete", "prompts": []any{},
				}})+"\n\n")
			},
		},
		{
			name: "as an event across several data lines",
			reply: func(w http.ResponseWriter, _ json.RawMessage) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"jsonrpc\": \"2.0\",\ndata: \"id\": \"srv-1\",\ndata: \"method\": \"roots/list\"}\n\n")
			},
		},
	}

	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				switch {
				case request.method == "server/discover":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"resultType":        "complete",
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
					}}))
				case request.method == "prompts/list":
					delivery.reply(w, request.id)
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			})
			c := Web(endpoint.URL)

			_, err := c.Prompts(context.Background())
			want := "the server sent a [roots/list] request over a connection of protocol version [" +
				LatestProtocolVersion + "], which forbids it; this client sends no response"
			if err == nil || err.Error() != want {
				t.Fatalf("prompts = %v, want %q", err, want)
			}
			endpoint.mu.Lock()
			var posted []string
			for _, request := range endpoint.requests {
				if request.method == "" && request.httpMethod == http.MethodPost {
					posted = append(posted, request.body)
				}
			}
			endpoint.mu.Unlock()
			if len(posted) != 0 {
				t.Fatalf("the client POSTed %d response(s) to a 2026-07-28 endpoint: %v", len(posted), posted)
			}
		})
	}
}

// TestALegacyServerRequestIsAnsweredOverStdio asserts the initialize-era rule
// over a transport without a header channel: a ping from the server is answered
// with an empty result under its id, and any other request is declined with
// method-not-found under its id, while the call that was in flight completes.
func TestALegacyServerRequestIsAnsweredOverStdio(t *testing.T) {
	tests := []struct {
		name   string
		method string
		// wantMember is the member the answer carries, and wantInside a
		// fragment of its value.
		wantMember string
		wantInside string
	}{
		{name: "ping", method: "ping", wantMember: "result", wantInside: `{}`},
		{name: "another request", method: "sampling/createMessage", wantMember: "error", wantInside: `"code":-32601`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTransport()
			f.framesBefore["prompts/list"] = []string{serverRequestFrame(`999`, tc.method)}
			f.on("prompts/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
				resp, _ := jsonrpc.NewResult(id, map[string]any{"prompts": []any{map[string]any{"name": "p"}}})
				return resp
			})
			c := newTestClient(f)

			prompts, err := c.Prompts(context.Background())
			if err != nil {
				t.Fatalf("prompts: %v", err)
			}
			if len(prompts) != 1 {
				t.Fatalf("listed %d prompts, want the one the server has", len(prompts))
			}
			f.mu.Lock()
			responses := responsesWritten(f.sent)
			f.mu.Unlock()
			if len(responses) != 1 {
				t.Fatalf("the client wrote %d response(s), want the one answering the server: %v", len(responses), responses)
			}
			var answer map[string]json.RawMessage
			if err := json.Unmarshal([]byte(responses[0]), &answer); err != nil {
				t.Fatalf("answer: %v", err)
			}
			if string(answer["id"]) != "999" {
				t.Fatalf("the answer carries id %s, want 999", answer["id"])
			}
			if !strings.Contains(string(answer[tc.wantMember]), tc.wantInside) {
				t.Fatalf("the answer's %s = %s, want it to carry %s", tc.wantMember, answer[tc.wantMember], tc.wantInside)
			}
			if !c.Connected() {
				t.Fatal("answering the server took the connection down")
			}
		})
	}
}

// TestALegacyServerRequestOverTheStreamIsAnsweredByPOST asserts the same over
// streamable HTTP under an initialize-era revision, where a server may send a
// request on the stream of a POST and the client answers with a POST of its own
// carrying the session: the call completes, the session stands, and nothing is
// released.
func TestALegacyServerRequestOverTheStreamIsAnsweredByPOST(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantMember string
		wantInside string
	}{
		{name: "ping", method: "ping", wantMember: "result", wantInside: `{}`},
		{name: "another request", method: "elicitation/create", wantMember: "error", wantInside: `"code":-32601`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				switch {
				case request.httpMethod == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				case request.method == "server/discover":
					w.WriteHeader(http.StatusBadRequest)
				case request.method == "initialize":
					w.Header().Set(sessionHeader, "session-1")
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"protocolVersion": ProtocolV20251125,
						"capabilities":    map[string]any{"prompts": map[string]any{}},
						"serverInfo":      map[string]any{"name": "s", "version": "1"},
					}}))
				case request.method == "prompts/list":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+serverRequestFrame(`"srv-ping"`, tc.method)+"\n\n")
					_, _ = io.WriteString(w, "data: "+jsonFrame(request.id, map[string]any{"result": map[string]any{
						"prompts": []any{map[string]any{"name": "p"}},
					}})+"\n\n")
				default:
					// Notifications and responses are accepted with no body.
					w.WriteHeader(http.StatusAccepted)
				}
			})
			c := Web(endpoint.URL)

			prompts, err := c.Prompts(context.Background())
			if err != nil {
				t.Fatalf("prompts: %v", err)
			}
			if len(prompts) != 1 || prompts[0].Name != "p" {
				t.Fatalf("prompts = %+v, want the one the server has", prompts)
			}
			if !c.Connected() {
				t.Fatal("the server's request took the connection down")
			}

			var answers []recordedRequest
			endpoint.mu.Lock()
			for _, request := range endpoint.requests {
				if request.httpMethod == http.MethodDelete {
					t.Error("the session was released over a request the revision allows")
				}
				if request.method == "" && request.httpMethod == http.MethodPost && strings.Contains(request.body, `"srv-ping"`) {
					answers = append(answers, request)
				}
			}
			endpoint.mu.Unlock()
			if len(answers) != 1 {
				t.Fatalf("the client POSTed %d answer(s) to the server's request, want 1", len(answers))
			}
			if got := answers[0].headers.Get(sessionHeader); got != "session-1" {
				t.Fatalf("the answer presented session %q, want session-1", got)
			}
			var answer map[string]json.RawMessage
			if err := json.Unmarshal([]byte(answers[0].body), &answer); err != nil {
				t.Fatalf("answer: %v", err)
			}
			if !strings.Contains(string(answer[tc.wantMember]), tc.wantInside) {
				t.Fatalf("the answer's %s = %s, want it to carry %s", tc.wantMember, answer[tc.wantMember], tc.wantInside)
			}
			want := []string{"server/discover", "initialize", "notifications/initialized", "prompts/list", ""}
			if got := endpoint.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
		})
	}
}

// TestAServerFrameWithAnIdThatIsNoIdIsInvalid asserts what the client does with
// a frame that names a method under an id the specification does not allow. A
// request id is a string or a number and MUST NOT be null (MCP basic,
// requests), and a notification carries no id member at all, so such a frame is
// neither. It is not acted on as a notification, it is not answered, and it
// ends the exchange as a frame the client cannot read, on every revision. A
// frame with no id member stays the notification it is.
func TestAServerFrameWithAnIdThatIsNoIdIsInvalid(t *testing.T) {
	const changed = "notifications/tools/list_changed"
	eras := []struct {
		name  string
		setup func(f *fakeTransport)
	}{
		{name: "the discovery revision", setup: func(f *fakeTransport) {
			f.on("server/discover", discoverHandler(LatestProtocolVersion))
		}},
		{name: "an initialize-era revision", setup: func(*fakeTransport) {}},
	}
	tests := []struct {
		name      string
		frame     string
		wantValid bool
	}{
		{name: "a null id on a notification method", frame: `{"jsonrpc":"2.0","id":null,"method":"` + changed + `"}`},
		{name: "a null id on a request method", frame: serverRequestFrame(`null`, "ping")},
		{name: "a boolean id", frame: serverRequestFrame(`true`, "ping")},
		{name: "an object id", frame: serverRequestFrame(`{}`, changed)},
		{name: "an array id", frame: serverRequestFrame(`[1]`, "ping")},
		{name: "no id member is a notification", frame: `{"jsonrpc":"2.0","method":"` + changed + `"}`, wantValid: true},
	}

	for _, era := range eras {
		for _, tt := range tests {
			t.Run(era.name+"/"+tt.name, func(t *testing.T) {
				f := newFakeTransport()
				era.setup(f)
				f.framesBefore["prompts/list"] = []string{tt.frame}
				f.on("prompts/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
					resp, _ := jsonrpc.NewResult(id, map[string]any{"resultType": "complete", "prompts": []any{}})
					return resp
				})
				c := New(f, testClientInfo())
				if err := c.Connect(context.Background()); err != nil {
					t.Fatalf("connect: %v", err)
				}
				var heard []string
				c.proto.notified = func(method string) { heard = append(heard, method) }

				_, err := c.Prompts(context.Background())

				f.mu.Lock()
				responses := responsesWritten(f.sent)
				f.mu.Unlock()
				if len(responses) != 0 {
					t.Fatalf("the client answered the frame: %v", responses)
				}
				if tt.wantValid {
					if err != nil {
						t.Fatalf("prompts: %v", err)
					}
					if !slices.Equal(heard, []string{changed}) {
						t.Fatalf("notifications heard = %v, want %v", heard, []string{changed})
					}
					return
				}
				if err == nil {
					t.Fatal("the exchange went on past a frame that is neither a request nor a notification")
				}
				if !strings.HasPrefix(err.Error(), "invalid JSON-RPC message from server: the [") ||
					!strings.HasSuffix(err.Error(), "] frame carries an id that is neither a string nor a number") {
					t.Fatalf("error = %q, want the frame reported as invalid", err.Error())
				}
				if len(heard) != 0 {
					t.Fatalf("the frame was acted on as a notification: %v", heard)
				}
			})
		}
	}
}
