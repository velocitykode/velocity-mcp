package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/transport"
)

// The tests in this file cover the shapes a real server cannot produce: replies
// whose fields carry the wrong JSON type, a notification emitted without params
// (no current handler path sends one: Request.ReportProgress always writes a
// progressToken), and a server that never finishes paginating. Everything a
// server can produce is asserted through the public API in asserts_test.go.

// nulString carries an embedded NUL so the helpers are exercised against a
// control character, without putting a raw NUL byte in this source file.
var nulString = string([]byte{'a', 0, 'b'})

// replyFrom decodes a raw reply frame into a Response, the same way the drivers
// build one, so these tests start from bytes rather than from a hand-built
// result map. t reports a broken fixture; assertOn is the testing.TB the
// Response reports its own failures through (nil for a Response whose
// assertions must degrade to no-ops).
func replyFrom(t testing.TB, assertOn testing.TB, method, frame string) *Response {
	t.Helper()
	resp, err := decodeResponse([]byte(frame))
	if err != nil {
		t.Fatalf("decode reply frame %q: %v", frame, err)
	}
	return newResponse(assertOn, method, resp)
}

func TestJSONString(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"map", map[string]any{"b": 1, "a": 2}, `{"a":2,"b":1}`},
		{"nil", nil, "null"},
		{"slice", []string{"x"}, `["x"]`},
		{"unencodable", make(chan int), "<unencodable value>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonString(tt.in); got != tt.want {
				t.Fatalf("jsonString(%#v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
		ok   bool
	}{
		{"sorts object keys", json.RawMessage(`{"b":1,"a":2}`), `{"a":2,"b":1}`, true},
		{"keeps a large integer verbatim", json.RawMessage(`{"id":9007199254740993}`), `{"id":9007199254740993}`, true},
		{"serializes a Go value", map[string]any{"n": 1}, `{"n":1}`, true},
		{"drops insignificant whitespace", json.RawMessage("[1,\n  2]"), `[1,2]`, true},
		// The rendering is what a failure message shows, so it keeps the number
		// as the server spelled it (comparableJSON is what equality runs on).
		{"keeps a number's spelling", json.RawMessage(`{"a":10.0,"b":1e2}`), `{"a":10.0,"b":1e2}`, true},
		// A message about a string carrying markup must be readable: no <.
		{"does not escape HTML", json.RawMessage(`{"a":"<b>&</b>"}`), `{"a":"<b>&</b>"}`, true},
		{"rejects trailing content", json.RawMessage(`{} {}`), "", false},
		{"rejects malformed bytes", json.RawMessage(`{"a":`), "", false},
		{"rejects empty bytes", json.RawMessage(``), "", false},
		{"rejects an unencodable value", make(chan int), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := canonicalJSON(tt.in)
			if ok != tt.ok {
				t.Fatalf("canonicalJSON(%v) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("canonicalJSON(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCanonicalNumber pins the spelling-independent form equality runs on. Two
// literals are the same value exactly when their canonical forms match, so the
// table doubles as the statement of which numbers this package judges equal.
func TestCanonicalNumber(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"10", "1e1"},
		{"10.0", "1e1"},
		{"1e1", "1e1"},
		{"1E1", "1e1"},
		{"1e+1", "1e1"},
		{"100", "1e2"},
		{"1e2", "1e2"},
		{"0.1", "1e-1"},
		{"1e-1", "1e-1"},
		{"1.50", "15e-1"},
		{"-0.5", "-5e-1"},
		{"0", "0"},
		{"-0", "0"},
		{"0.000", "0"},
		{"0e12", "0"},
		// Above float64's exact range the digits are kept, so neighbouring ids
		// stay apart.
		{"9007199254740993", "9007199254740993e0"},
		{"9007199254740993.0", "9007199254740993e0"},
		{"9007199254740992", "9007199254740992e0"},
		// An exponent no reply could carry is kept as written rather than
		// expanded into digits.
		{"1e2000000000", "1e2000000000"},
		{"1e99999999999999999999", "1e99999999999999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := canonicalNumber(tt.in); got != tt.want {
				t.Fatalf("canonicalNumber(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// jsonNumber is a JSON number literal paired with its exact value.
type jsonNumber struct {
	text  string
	value *big.Rat
}

// The exponent bounds below keep the exact expansions cheap. A fuzz input is
// held to the first; a canonical form is held to the second, because
// canonicalNumber moves a literal's digits into its exponent and so may raise it
// by as much as the literal's length.
const (
	maxLiteralExponent   = 2000
	maxCanonicalExponent = maxLiteralExponent + maxLiteralLength
	maxLiteralLength     = 40
)

// jsonNumberOf reads input as a single JSON number literal and pairs it with its
// exact rational value, which is the independent oracle the fuzz property below
// judges the canonical form against. maxExponent refuses a literal whose exact
// expansion would be costly; canonicalNumber keeps an exponent that large
// verbatim by design (see its doc comment), so equality there is textual and
// outside this property.
func jsonNumberOf(input string, maxExponent int) (jsonNumber, bool) {
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil || dec.More() {
		return jsonNumber{}, false
	}
	number, isNumber := decoded.(json.Number)
	if !isNumber {
		return jsonNumber{}, false
	}
	// The decoded literal, not the input: a number reaches canonicalNumber from
	// a JSON decode, so it never carries surrounding whitespace.
	text := string(number)
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent > maxExponent || exponent < -maxExponent {
			return jsonNumber{}, false
		}
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return jsonNumber{}, false
	}
	return jsonNumber{text: text, value: value}, true
}

// numberLiteralOf accepts the fuzz inputs the property can judge: one JSON
// number, no longer than a reply carries and within the exponent bound.
func numberLiteralOf(input string) (jsonNumber, bool) {
	if len(input) == 0 || len(input) > maxLiteralLength {
		return jsonNumber{}, false
	}
	return jsonNumberOf(input, maxLiteralExponent)
}

// FuzzCanonicalNumber drives the engine every value assertion's equality runs
// on. canonicalNumber decides whether two numbers on the wire are the same
// value, so the property is stated against an oracle that shares none of its
// code: exact rational arithmetic. For any two JSON number literals, each
// canonical form must be a JSON number of the same value as its literal, and two
// literals must share a canonical form exactly when they are the same number.
// The same statement is made through jsonEqual, the path a value assertion
// actually takes from the bytes a server wrote.
func FuzzCanonicalNumber(f *testing.F) {
	// The seeds are the table above, paired so each pair states an equality or
	// an inequality the package depends on.
	seeds := [][2]string{
		{"10", "10.0"},
		{"1e1", "1E+1"},
		{"100", "1e2"},
		{"0.1", "1e-1"},
		{"1.50", "15e-1"},
		{"-0.5", "-5e-1"},
		{"0", "-0"},
		{"0.000", "0e12"},
		{"9007199254740993", "9007199254740992"},
		{"9007199254740993.0", "9007199254740993"},
		{"1", "-1"},
		{"12345678901234567890", "1.234567890123456789e19"},
		{"1e-1000", "0.1"},
		// The canonical form's exponent may exceed the literal's, by as many
		// places as the literal has digits.
		{"1000e1000", "1e1003"},
		{"", "not a number"},
	}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		left, leftOK := numberLiteralOf(a)
		right, rightOK := numberLiteralOf(b)
		if !leftOK || !rightOK {
			// Not a pair of JSON numbers a reply could carry.
			return
		}

		// A canonical form is a JSON number, so a failure message built from it
		// stays readable, and it is the same number as the literal it came from.
		for _, n := range []jsonNumber{left, right} {
			canonical := canonicalNumber(n.text)
			got, ok := jsonNumberOf(canonical, maxCanonicalExponent)
			if !ok {
				t.Fatalf("canonicalNumber(%q) = %q, which is not a JSON number", n.text, canonical)
			}
			if got.value.Cmp(n.value) != 0 {
				t.Fatalf("canonicalNumber(%q) = %q, which is the different value %s",
					n.text, canonical, got.value.RatString())
			}
		}

		same := left.value.Cmp(right.value) == 0
		if got := canonicalNumber(left.text) == canonicalNumber(right.text); got != same {
			t.Fatalf("canonicalNumber(%q) == canonicalNumber(%q) is %t, want %t (%q and %q)",
				left.text, right.text, got, same, canonicalNumber(left.text), canonicalNumber(right.text))
		}
		if got := jsonEqual(json.RawMessage(left.text), json.RawMessage(right.text)); got != same {
			t.Fatalf("jsonEqual(%q, %q) = %t, want %t", left.text, right.text, got, same)
		}
	})
}

// TestComparableJSON_NumberSpelling pins that equality is numeric and not
// textual, in both directions: values that differ only in spelling are one
// document, and values that differ beyond float64's precision are not.
func TestComparableJSON_NumberSpelling(t *testing.T) {
	equal := []struct {
		name string
		a, b any
	}{
		{"a wire 10.0 and the Go int 10", json.RawMessage(`10.0`), 10},
		{"a wire 1e2 and the Go int 100", json.RawMessage(`1e2`), 100},
		{"a wire 10 and the Go float 10", json.RawMessage(`10`), 10.0},
		{"nested members", json.RawMessage(`{"a":[1.0,{"b":2e0}]}`), map[string]any{
			"a": []any{1, map[string]any{"b": 2}},
		}},
		{"every spelling of zero", json.RawMessage(`-0.000`), 0},
	}
	for _, tt := range equal {
		t.Run(tt.name, func(t *testing.T) {
			if !jsonEqual(tt.a, tt.b) {
				an, _ := comparableJSON(tt.a)
				bn, _ := comparableJSON(tt.b)
				t.Fatalf("jsonEqual(%v,%v) = false; comparable forms %q and %q", tt.a, tt.b, an, bn)
			}
		})
	}

	unequal := []struct {
		name string
		a, b any
	}{
		{"ids one apart beyond 2^53", json.RawMessage(`9007199254740993`), int64(9007199254740992)},
		{"a number and its string", json.RawMessage(`10`), "10"},
		{"values that really differ", json.RawMessage(`10.5`), 10},
		{"a trailing digit", json.RawMessage(`1e2`), 1000},
	}
	for _, tt := range unequal {
		t.Run(tt.name, func(t *testing.T) {
			if jsonEqual(tt.a, tt.b) {
				t.Fatalf("jsonEqual(%v,%v) = true, want false", tt.a, tt.b)
			}
		})
	}

	// The failure messages still report the wire spelling, which is the point of
	// keeping the rendering and the comparison apart.
	if got := describeJSON(json.RawMessage(`10.0`)); got != "10.0" {
		t.Fatalf("describeJSON(10.0) = %q, want 10.0", got)
	}
}

func TestDecodeNotificationFrame(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		wantOK     bool
		wantMethod string
		// wantParams is the params member as the frame carried it, empty for a
		// frame that carried none. It is compared byte for byte: the recording
		// reports what the server wrote, it does not rewrite it.
		wantParams string
	}{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/progress","params":{"p":1}}`, true, "notifications/progress", `{"p":1}`},
		{"notification without params", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, true, "notifications/initialized", ""},
		{"null id is a notification", `{"jsonrpc":"2.0","id":null,"method":"x"}`, true, "x", ""},
		// The inbound parser rejects params that are not an object, but a frame
		// the server emitted is evidence, not a request to validate: dropping it
		// would hide the emission from every notification assertion, so it is
		// attributed and its params are carried through untouched.
		{"null params", `{"jsonrpc":"2.0","method":"x","params":null}`, true, "x", `null`},
		{"array params", `{"jsonrpc":"2.0","method":"x","params":[1,2,3]}`, true, "x", `[1,2,3]`},
		{"scalar params", `{"jsonrpc":"2.0","method":"x","params":"raw"}`, true, "x", `"raw"`},
		{"reply with id", `{"jsonrpc":"2.0","id":1,"result":{}}`, false, "", ""},
		{"wrong version", `{"jsonrpc":"1.0","method":"x"}`, false, "", ""},
		{"null version", `{"jsonrpc":null,"method":"x"}`, false, "", ""},
		{"missing version", `{"method":"x"}`, false, "", ""},
		{"missing method", `{"jsonrpc":"2.0","params":{}}`, false, "", ""},
		{"non-string method", `{"jsonrpc":"2.0","method":42}`, false, "", ""},
		{"null method", `{"jsonrpc":"2.0","method":null}`, false, "", ""},
		{"malformed json", `{"jsonrpc":`, false, "", ""},
		{"trailing content", `{"jsonrpc":"2.0","method":"x"}{}`, false, "", ""},
		{"empty frame", ``, false, "", ""},
		{"not an object", `[1,2,3]`, false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note, ok := decodeNotificationFrame([]byte(tt.frame))
			if ok != tt.wantOK {
				t.Fatalf("decodeNotificationFrame(%q) ok = %v, want %v", tt.frame, ok, tt.wantOK)
			}
			if !ok {
				if note != nil {
					t.Fatalf("decodeNotificationFrame(%q) returned a notification with ok=false", tt.frame)
				}
				return
			}
			if note.Method != tt.wantMethod {
				t.Fatalf("decodeNotificationFrame(%q) method = %q, want %q", tt.frame, note.Method, tt.wantMethod)
			}
			if got := string(note.Params); got != tt.wantParams {
				t.Fatalf("decodeNotificationFrame(%q) params = %q, want %q", tt.frame, got, tt.wantParams)
			}
		})
	}
}

// TestDecodeNotificationFrameDoesNotAliasFrame covers the recording keeping its
// own copy of a frame's params: the transport hands out the bytes it recorded,
// and a notification that pointed into them would report whatever a later writer
// left in that buffer.
func TestDecodeNotificationFrameDoesNotAliasFrame(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","method":"x","params":{"step":1}}`)
	note, ok := decodeNotificationFrame(frame)
	if !ok {
		t.Fatal("frame must decode as a notification")
	}
	for i := range frame {
		frame[i] = ' '
	}
	if got := string(note.Params); got != `{"step":1}` {
		t.Fatalf("params = %q, want the bytes as they were recorded", got)
	}
}

// FuzzDecodeNotificationFrame drives arbitrary bytes through the outbound-frame
// classifier. It must never panic, and an accepted frame must be a well-formed
// JSON-RPC 2.0 notification whose params the assertions can render and compare
// (they read whatever shape arrived, including none).
func FuzzDecodeNotificationFrame(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":null,"method":"x"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":""}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"` + nulString + `"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"x","params":[1,2,3]}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"x","params":null}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, frame []byte) {
		note, ok := decodeNotificationFrame(frame)
		if !ok {
			if note != nil {
				t.Fatalf("ok=false must yield a nil notification, got %+v", note)
			}
			return
		}
		if note == nil {
			t.Fatal("ok=true must yield a non-nil notification")
		}
		if note.JSONRPC != "2.0" {
			t.Fatalf("an accepted frame must carry jsonrpc 2.0, got %q", note.JSONRPC)
		}
		// The params of an accepted frame are whatever arrived; the assertions
		// must be able to compare and render them without panicking. Only an
		// omitted params member reads as an empty object, the member being
		// optional; every other shape, an explicit null included, is compared as
		// it arrived so a frame the specification does not allow cannot satisfy
		// an empty expectation.
		params := notificationParams(note)
		if len(note.Params) == 0 {
			if string(params) != "{}" {
				t.Fatalf("omitted params must compare as an empty object, got %s", params)
			}
		} else if string(params) != string(note.Params) {
			t.Fatalf("params %q must be compared as they arrived, got %s", note.Params, params)
		}
		_ = describeNotificationParams(note)
	})
}

// TestMalformedReplyShapes drives the assertions against replies whose fields
// carry the wrong JSON type. A real server never emits these, but a Response is
// built from whatever bytes arrived, so the accessors must report rather than
// panic or silently pass.
func TestMalformedReplyShapes(t *testing.T) {
	t.Run("structuredContent is not an object", func(t *testing.T) {
		tb := &recordingTB{}
		r := replyFrom(t, tb, "tools/call", `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":"nope"}}`)
		// The object accessor narrows, so a string reads as no object.
		if got := r.StructuredContent(); got != nil {
			t.Fatalf("StructuredContent on a non-object = %#v, want nil", got)
		}
		// The assertions do not narrow: the tool returned a value, and the
		// comparison is against that value rather than against "none".
		r.AssertStructuredContent(map[string]any{"a": 1})
		if !strings.Contains(tb.last(), `structured content = "nope"`) {
			t.Fatalf("failure message = %q", tb.last())
		}
		// A value the reply does carry must not satisfy the negative assertion.
		r.AssertNoStructuredContent()
		if !strings.Contains(tb.last(), "expected no structured content") {
			t.Fatalf("AssertNoStructuredContent message = %q", tb.last())
		}
		if len(tb.messages) != 2 {
			t.Fatalf("reported %d failures, want 2: %v", len(tb.messages), tb.messages)
		}
		// The whole value is what a key assertion is refused against, named as
		// the wrong shape rather than as a missing key.
		r.AssertStructuredContentKey("a", 1)
		if !strings.Contains(tb.last(), "it is not an object") {
			t.Fatalf("AssertStructuredContentKey message = %q", tb.last())
		}
	})

	t.Run("completion is not an object", func(t *testing.T) {
		r := replyFrom(t, nil, "completion/complete", `{"jsonrpc":"2.0","id":1,"result":{"completion":"nope"}}`)
		if got := r.completion(); got != nil {
			t.Fatalf("completion on a non-object = %#v, want nil", got)
		}
		if got := r.CompletionValues(); len(got) != 0 {
			t.Fatalf("CompletionValues = %v, want empty", got)
		}
	})

	t.Run("completion values are not strings", func(t *testing.T) {
		tb := &recordingTB{}
		r := replyFrom(t, tb, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"result":{"completion":{"values":["go",42,null],"total":3}}}`)
		// A candidate list the specification does not permit yields nothing to
		// compare against, rather than the entries that happen to be strings.
		if got := r.CompletionValues(); len(got) != 0 {
			t.Fatalf("CompletionValues = %v, want none", got)
		}
		// Every candidate assertion refuses the reply, naming the offending
		// entry, so none of them can certify it.
		r.AssertHasCompletions("go")
		r.AssertCompletionValues("go")
		r.AssertCompletionCount(1)
		if len(tb.messages) != 3 {
			t.Fatalf("reported %d failures, want 3: %v", len(tb.messages), tb.messages)
		}
		for _, message := range tb.messages {
			if !strings.Contains(message, "not a string at index 1") {
				t.Fatalf("failure message = %q", message)
			}
		}
	})

	t.Run("reply carries no result object", func(t *testing.T) {
		// An error reply decodes to a nil result; every accessor must report
		// "none" rather than indexing a nil map.
		r := replyFrom(t, nil, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"boom"}}`)
		if got := r.completion(); got != nil {
			t.Fatalf("completion on a result-less reply = %#v, want nil", got)
		}
		if got := r.CompletionValues(); len(got) != 0 {
			t.Fatalf("CompletionValues on a result-less reply = %v, want empty", got)
		}
		if got := r.StructuredContent(); got != nil {
			t.Fatalf("StructuredContent on a result-less reply = %#v, want nil", got)
		}
		if _, ok := r.rawResultPath("completion", "total"); ok {
			t.Fatal("rawResultPath on a result-less reply must report no value")
		}
	})

	t.Run("list entry field is not a string", func(t *testing.T) {
		tb := &recordingTB{}
		r := replyFrom(t, tb, "tools/list", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t","title":42}]}}`)
		r.Tool("t").AssertTitle("Anything")
		if !strings.Contains(tb.last(), `tool "t" has no string title`) {
			t.Fatalf("failure message = %q", tb.last())
		}
	})

	t.Run("a missing entry reports once", func(t *testing.T) {
		// The lookup reports that the entry is absent; the field assertions
		// chained onto it must then say nothing, so the reader gets one failure
		// rather than a cascade of them. recordingTB keeps going after a report,
		// which is what makes the cascade visible here.
		tb := &recordingTB{}
		r := replyFrom(t, tb, "tools/list", `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t"}]}}`)
		r.Tool("ghost").AssertName("ghost").AssertTitle("Ghost").AssertDescription("boo")
		if len(tb.messages) != 1 {
			t.Fatalf("a missing entry reported %d times: %q", len(tb.messages), tb.messages)
		}
		if !strings.Contains(tb.last(), `tool "ghost" was not listed`) {
			t.Fatalf("failure message = %q", tb.last())
		}
	})

	t.Run("completion without a total", func(t *testing.T) {
		// The server always writes total and hasMore, so this shape is reachable
		// only from bytes: an absent total is reported as absent rather than read
		// as zero, and an absent hasMore reads as false (the flag is optional).
		tb := &recordingTB{}
		r := replyFrom(t, tb, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"result":{"completion":{"values":["go"]}}}`)

		r.AssertCompletionTotal(1)
		if !strings.Contains(tb.last(), "completion total = (none), want 1") {
			t.Fatalf("failure message = %q", tb.last())
		}
		r.AssertCompletionHasMore(false)
		if len(tb.messages) != 1 {
			t.Fatalf("an absent hasMore must read as false, got %q", tb.last())
		}
		r.AssertCompletionHasMore(true)
		if !strings.Contains(tb.last(), "completion hasMore = false, want true") {
			t.Fatalf("failure message = %q", tb.last())
		}
	})

	t.Run("completion hasMore is not a boolean", func(t *testing.T) {
		tb := &recordingTB{}
		r := replyFrom(t, tb, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"result":{"completion":{"values":[],"hasMore":"yes"}}}`)
		r.AssertCompletionHasMore(false)
		if !strings.Contains(tb.last(), `completion hasMore is not a boolean: "yes", want false`) {
			t.Fatalf("failure message = %q", tb.last())
		}
	})
}

// stubMCPServer answers inbound frames with whatever handle returns, so the
// harness can be driven against replies a real server cannot produce (a list
// method answering with no list) and against a client notification the server
// answers with a server-initiated frame.
type stubMCPServer struct {
	handle func(raw []byte, emit func(msg []byte) error) server.HandleResult
}

func (s stubMCPServer) Handle(_ context.Context, raw []byte, _ string) server.HandleResult {
	return s.handle(raw, nil)
}

func (s stubMCPServer) HandleStream(_ context.Context, raw []byte, _ string, emit func(msg []byte) error) server.HandleResult {
	return s.handle(raw, emit)
}

// stubHarness builds a Server driving srv, reporting through tb.
func stubHarness(tb testing.TB, srv transport.MCPServer) *Server {
	return &Server{t: tb, fake: transport.NewFake(srv), ctx: context.Background()}
}

// TestListAll_PageWithoutAList covers a server that answers a list method with a
// result carrying no array under the list key. The walk cannot merge such a
// page, so it reports it, and the reply is handed back as it arrived rather than
// rewritten into an empty catalogue that the registration assertions would read
// as "nothing is registered".
func TestListAll_PageWithoutAList(t *testing.T) {
	stub := stubMCPServer{handle: func(raw []byte, _ func(msg []byte) error) server.HandleResult {
		_, id, err := jsonrpc.ParseRequest(raw)
		if err != nil {
			t.Fatalf("stub received a malformed request: %v", err)
		}
		resp, nerr := jsonrpc.NewResult(id, map[string]any{"unrelated": true})
		if nerr != nil {
			t.Fatalf("build stub result: %v", nerr)
		}
		return server.HandleResult{Response: resp, HasResponse: true}
	}}

	tb := &recordingTB{}
	res := stubHarness(tb, stub).ListTools()
	if !strings.Contains(tb.last(), `the reply carries no "tools" list`) {
		t.Fatalf("the driver did not report the invalid page: %q", tb.last())
	}

	// The page was not rewritten, so an assertion chained on it refuses it too.
	res.AssertToolNotListed("add")
	if !strings.Contains(tb.last(), `expected the reply to carry a "tools" list`) {
		t.Fatalf("failure message = %q", tb.last())
	}
}

// TestNotify_AttributesServerFrames covers the notification attribution on the
// Notify path: a streaming server may answer a client notification with frames
// of its own (a log message, say), and those belong to the message that produced
// them, exactly as they do for a request.
func TestNotify_AttributesServerFrames(t *testing.T) {
	stub := stubMCPServer{handle: func(_ []byte, emit func(msg []byte) error) server.HandleResult {
		if emit != nil {
			if err := emit([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)); err != nil {
				t.Fatalf("emit: %v", err)
			}
		}
		return server.HandleResult{HasResponse: false}
	}}

	tb := &recordingTB{}
	stubHarness(tb, stub).Notify("notifications/initialized", nil).
		AssertNoResponse().
		AssertNotificationCount(1).
		AssertSentNotification("notifications/message").
		AssertSentNotificationWith("notifications/message", map[string]any{"level": "info"})
	if len(tb.messages) != 0 {
		t.Fatalf("a notification's own frames must be attributed to it, got %q", tb.messages)
	}
}

// TestMergeListPages_MergesEveryPage pins what the merge carries over from the
// pages it walked: the items in both the decoded and the raw view (the value
// assertions read the raw one), no spent cursor, and the notifications emitted
// while any page was handled, not just the last.
func TestMergeListPages_MergesEveryPage(t *testing.T) {
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"nextCursor":"c1"}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"b"}]}}`,
	}

	fetched := 0
	merged, problem := mergeListPages("tools", func(cursor string) *Response {
		if fetched >= len(frames) {
			t.Fatalf("the walk asked for page %d, only %d exist", fetched, len(frames))
		}
		if want := map[int]string{0: "", 1: "c1"}[fetched]; cursor != want {
			t.Fatalf("page %d fetched with cursor %q, want %q", fetched, cursor, want)
		}
		page := replyFrom(t, t, "tools/list", frames[fetched])
		page.notifications = append(page.notifications, &jsonrpc.Notification{
			JSONRPC: jsonrpc.Version,
			Method:  fmt.Sprintf("page/%d", fetched),
		})
		fetched++
		return page
	})
	if problem != "" {
		t.Fatalf("a well-behaved server must not be reported: %q", problem)
	}
	if fetched != len(frames) {
		t.Fatalf("walked %d pages, want %d", fetched, len(frames))
	}

	merged.AssertToolListed("a", "b").AssertToolCount(2)
	// The raw view is separate bookkeeping from the decoded one, and the value
	// assertions compare against it, so it must carry every page too.
	merged.AssertResult("tools", json.RawMessage(`[{"name":"a"},{"name":"b"}]`))
	if _, ok := merged.rawResultPath("nextCursor"); ok {
		t.Fatal("the merged raw view must not carry the spent cursor")
	}
	// Raw is the documented escape hatch for assertions the fluent API does not
	// cover, so the reply envelope has to carry the merged catalogue as well: a
	// custom assertion reading it must not see the last page alone.
	var envelope struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(merged.Raw().Result, &envelope); err != nil {
		t.Fatalf("decoding the merged envelope: %v", err)
	}
	if len(envelope.Tools) != 2 || envelope.Tools[0].Name != "a" || envelope.Tools[1].Name != "b" {
		t.Fatalf("Raw().Result carries tools %+v, want a then b", envelope.Tools)
	}
	if envelope.NextCursor != nil {
		t.Fatalf("Raw().Result carries the spent cursor %q", *envelope.NextCursor)
	}
	merged.AssertNotificationCount(2).
		AssertSentNotification("page/0").
		AssertSentNotification("page/1")
}

// TestMergeListPages_ErrorPageEndsTheWalk covers a server that answers a later
// page with an error (an expired or rejected cursor). The walk stops there and
// hands back that reply for the error assertions, and it carries the
// notifications emitted while the earlier pages were handled, so what the reply
// reports is the whole exchange and not just its last frame.
func TestMergeListPages_ErrorPageEndsTheWalk(t *testing.T) {
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"nextCursor":"c1"}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Invalid cursor."}}`,
	}

	fetched := 0
	page, problem := mergeListPages("tools", func(string) *Response {
		if fetched >= len(frames) {
			t.Fatalf("the walk asked for page %d, only %d exist", fetched, len(frames))
		}
		p := replyFrom(t, t, "tools/list", frames[fetched])
		p.notifications = append(p.notifications, &jsonrpc.Notification{
			JSONRPC: jsonrpc.Version,
			Method:  fmt.Sprintf("page/%d", fetched),
		})
		fetched++
		return p
	})
	if problem != "" {
		t.Fatalf("an error reply is the server's answer, not a broken walk: %q", problem)
	}
	if fetched != len(frames) {
		t.Fatalf("walked %d pages, want %d", fetched, len(frames))
	}

	page.AssertError("Invalid cursor.").
		AssertNotificationCount(2).
		AssertSentNotification("page/0").
		AssertSentNotification("page/1")
}

// TestMergeListPages_PageCap covers a server whose cursor keeps advancing: the
// repeated-cursor guard never fires, so without a page cap the walk would run
// forever instead of failing.
func TestMergeListPages_PageCap(t *testing.T) {
	fetched := 0
	_, problem := mergeListPages("tools", func(string) *Response {
		fetched++
		if fetched > maxListPages {
			t.Fatal("the walk ran past the page cap")
		}
		return replyFrom(t, nil, "tools/list", fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t"}],"nextCursor":"c%d"}}`, fetched))
	})
	if !strings.Contains(problem, fmt.Sprintf("more than %d pages", maxListPages)) {
		t.Fatalf("problem = %q", problem)
	}
	if fetched != maxListPages {
		t.Fatalf("walked %d pages, want the cap of %d", fetched, maxListPages)
	}
}

// TestMergeListPages_CursorType covers the type of the pagination cursor. The
// specification types "nextCursor" as an optional string token, so a page that
// omits it (or writes a null, or an empty string naming no page, in its place)
// is the last one, while a page that answers with any other JSON type is
// malformed and must stop the walk with a report. Reading a malformed cursor
// as an absent one would hand back the pages gathered so far as a complete
// catalogue, and here that is an empty one, which every "not listed" and
// "count is zero" assertion holds against.
func TestMergeListPages_CursorType(t *testing.T) {
	tests := []struct {
		name string
		// cursor is the JSON value the page writes under "nextCursor", or "" to
		// leave the member out of the page entirely.
		cursor string
		// wantDescribed is how the report renders the offending value; empty
		// when the page is well formed and the walk must not report anything.
		wantDescribed string
	}{
		{name: "absent"},
		{name: "null", cursor: `null`},
		{name: "empty string", cursor: `""`},
		{name: "number", cursor: `123`, wantDescribed: "123"},
		{name: "array", cursor: `["c1"]`, wantDescribed: `["c1"]`},
		{name: "object", cursor: `{"page":2}`, wantDescribed: `{"page":2}`},
		{name: "boolean", cursor: `true`, wantDescribed: "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
			if tt.cursor != "" {
				frame = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"tools":[],"nextCursor":%s}}`, tt.cursor)
			}

			fetched := 0
			page, problem := mergeListPages("tools", func(string) *Response {
				fetched++
				if fetched > 1 {
					t.Fatal("the walk followed a cursor it must not have followed")
				}
				return replyFrom(t, nil, "tools/list", frame)
			})
			if fetched != 1 {
				t.Fatalf("walked %d pages, want 1", fetched)
			}

			if tt.wantDescribed == "" {
				if problem != "" {
					t.Fatalf("a page without a cursor ends the walk, got problem %q", problem)
				}
				return
			}
			want := fmt.Sprintf(`"nextCursor" that is not a string (%s)`, tt.wantDescribed)
			if !strings.Contains(problem, want) {
				t.Fatalf("problem = %q, want it to contain %q", problem, want)
			}
			// The walk stopped, so the merged page is what was gathered before
			// the cursor was rejected, not a catalogue anyone may trust.
			if got := len(page.listItems("tools")); got != 0 {
				t.Fatalf("merged %d items, want 0", got)
			}
		})
	}
}

// TestMergeListPages_MalformedCursorMidWalk places the malformed cursor on a
// later page. By then the walk has followed a well-formed cursor and gathered
// items, so the page the caller would be handed carries a plausible part of the
// catalogue: taking the malformed cursor for an absent one would report that
// part as the whole. The walk stops where the cursor is refused and requests
// nothing past it.
func TestMergeListPages_MalformedCursorMidWalk(t *testing.T) {
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"}],"nextCursor":"c1"}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"b"}],"nextCursor":0}}`,
	}

	fetched := 0
	page, problem := mergeListPages("tools", func(cursor string) *Response {
		if fetched >= len(frames) {
			t.Fatalf("the walk asked for page %d, only %d exist", fetched, len(frames))
		}
		if want := map[int]string{0: "", 1: "c1"}[fetched]; cursor != want {
			t.Fatalf("page %d fetched with cursor %q, want %q", fetched, cursor, want)
		}
		p := replyFrom(t, nil, "tools/list", frames[fetched])
		fetched++
		return p
	})
	if fetched != len(frames) {
		t.Fatalf("walked %d pages, want %d", fetched, len(frames))
	}

	want := `"nextCursor" that is not a string (0)`
	if !strings.Contains(problem, want) {
		t.Fatalf("problem = %q, want it to contain %q", problem, want)
	}
	// Both answered pages are still merged, as they are for the repeated-cursor
	// and page-cap guards: the report is the finding, and the partial catalogue
	// is what a testing.TB whose Fatalf aborts nothing is left holding.
	if got := len(page.listItems("tools")); got != 2 {
		t.Fatalf("merged %d items, want 2", got)
	}
}

// TestListAll_MalformedCursor drives a malformed page through each of the four
// list drivers: the report reaches the test as a failure rather than being
// swallowed into a silently exhausted catalogue. The drivers share one page
// walk, so each has to refuse the cursor under its own method name and list
// key.
func TestListAll_MalformedCursor(t *testing.T) {
	tests := []struct {
		name   string
		method string
		key    string
		list   func(*Server) *Response
	}{
		{"tools", "tools/list", "tools", (*Server).ListTools},
		{"resources", "resources/list", "resources", (*Server).ListResources},
		{"resource templates", "resources/templates/list", "resourceTemplates", (*Server).ListResourceTemplates},
		{"prompts", "prompts/list", "prompts", (*Server).ListPrompts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := stubMCPServer{handle: func(raw []byte, _ func(msg []byte) error) server.HandleResult {
				req, id, err := jsonrpc.ParseRequest(raw)
				if err != nil {
					t.Fatalf("stub received a malformed request: %v", err)
				}
				if req.Method != tt.method {
					t.Fatalf("stub received method %q, want %q", req.Method, tt.method)
				}
				resp, nerr := jsonrpc.NewResult(id, map[string]any{
					tt.key:       []any{},
					"nextCursor": 123,
				})
				if nerr != nil {
					t.Fatalf("build stub result: %v", nerr)
				}
				return server.HandleResult{Response: resp, HasResponse: true}
			}}

			tb := &recordingTB{}
			tt.list(stubHarness(tb, stub))
			want := tt.method + `: server answered with a "nextCursor" that is not a string (123)`
			if !strings.Contains(tb.last(), want) {
				t.Fatalf("failure message = %q, want it to contain %q", tb.last(), want)
			}
		})
	}
}

// TestAssertNotRegistered_RequiresProtocolError pins what the registration
// assertions accept as evidence that a primitive is missing. The specification
// separates a protocol error, which reports that the request could not be
// served at all, from a result, which reports that a handler ran, so only the
// first shape counts however the second one reads, and it counts only under the
// code the specification gives an unknown primitive. The three assertions share
// one helper, so every shape is driven through all of them. The frames are
// driven directly because a wrong error code, and a result carrying the
// not-found text, are shapes the server does not produce.
func TestAssertNotRegistered_RequiresProtocolError(t *testing.T) {
	// errorReply and resultReply build the two reply shapes. The message is
	// JSON-quoted rather than pasted in, because a resource is named by uri.
	errorReply := func(code int, message string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":%s}}`,
			code, strconv.Quote(message))
	}
	resultReply := func(isError bool, text string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"isError":%t,"content":[{"type":"text","text":%s}]}}`,
			isError, strconv.Quote(text))
	}

	kinds := []struct {
		kind string
		// method is the request the reply answers, notFound the message the
		// server writes for a primitive of this kind it does not have, and
		// other the same message for a different primitive, which names the
		// wrong one and must not be accepted.
		method   string
		notFound string
		other    string
		assert   func(*Response)
	}{
		{
			kind:     "tool",
			method:   "tools/call",
			notFound: "Tool [ghost] not found.",
			other:    "Tool [other] not found.",
			assert:   func(r *Response) { r.AssertToolNotRegistered("ghost") },
		},
		{
			kind:     "prompt",
			method:   "prompts/get",
			notFound: "Prompt [ghost] not found.",
			other:    "Prompt [other] not found.",
			assert:   func(r *Response) { r.AssertPromptNotRegistered("ghost") },
		},
		{
			kind:     "resource",
			method:   "resources/read",
			notFound: "Resource [file://ghost.txt] not found.",
			other:    "Resource [file://other.txt] not found.",
			assert:   func(r *Response) { r.AssertResourceNotRegistered("file://ghost.txt") },
		},
	}

	for _, k := range kinds {
		// wantSub is the substring the failure must carry; empty means the
		// reply has to satisfy the assertion instead.
		shapes := []struct {
			name    string
			frame   string
			wantSub string
		}{
			{
				name:  "protocol not found",
				frame: errorReply(jsonrpc.CodeInvalidParams, k.notFound),
			},
			{
				name:    "error result carrying the not-found text",
				frame:   resultReply(true, k.notFound),
				wantSub: "but the reply carries no protocol error; errors: [" + k.notFound + "]",
			},
			{
				name:    "success result carrying the not-found text",
				frame:   resultReply(false, k.notFound),
				wantSub: "but the reply carries no protocol error; errors: (none)",
			},
			{
				name:    "wrong error code",
				frame:   errorReply(jsonrpc.CodeInternalError, k.notFound),
				wantSub: fmt.Sprintf("got error code %d: %s", jsonrpc.CodeInternalError, k.notFound),
			},
			{
				name:  "right code wrong message",
				frame: errorReply(jsonrpc.CodeInvalidParams, k.other),
				wantSub: fmt.Sprintf("expected the %s to be unregistered (error %q), got: %s",
					k.kind, k.notFound, k.other),
			},
		}

		for _, sh := range shapes {
			t.Run(k.kind+"/"+sh.name, func(t *testing.T) {
				tb := &recordingTB{}
				k.assert(replyFrom(t, tb, k.method, sh.frame))
				if sh.wantSub == "" {
					if len(tb.messages) != 0 {
						t.Fatalf("the protocol not-found reply must satisfy the assertion, got %q", tb.last())
					}
					return
				}
				if !strings.Contains(tb.last(), sh.wantSub) {
					t.Fatalf("failure message = %q, want it to contain %q", tb.last(), sh.wantSub)
				}
			})
		}
	}
}

// TestAssertResourceNotRegistered_AcceptsEitherRevisionCode pins that the
// resource assertion reads both codes the specification gives an unresolvable
// uri. A resources/read is answered with Invalid params under revision
// 2026-07-28 and with Resource not found under the revisions before it, so a
// suite that drives either one asserts the same thing; any other code still
// fails, because the assertion claims the resource is absent and not merely
// that something went wrong.
func TestAssertResourceNotRegistered_AcceptsEitherRevisionCode(t *testing.T) {
	const message = "Resource [file://ghost.txt] not found."

	tests := []struct {
		name string
		code int
		// wantSub is the substring the failure must carry; empty means the
		// reply has to satisfy the assertion instead.
		wantSub string
	}{
		{name: "discovery revision", code: jsonrpc.CodeInvalidParams},
		{name: "initialize-era revision", code: jsonrpc.CodeResourceNotFound},
		{
			name:    "any other code",
			code:    jsonrpc.CodeInternalError,
			wantSub: fmt.Sprintf("(protocol error -32602 or -32002 %q), got error code %d", message, jsonrpc.CodeInternalError),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":%s}}`,
				tt.code, strconv.Quote(message))
			tb := &recordingTB{}
			replyFrom(t, tb, "resources/read", frame).AssertResourceNotRegistered("file://ghost.txt")
			if tt.wantSub == "" {
				if len(tb.messages) != 0 {
					t.Fatalf("the reply must satisfy the assertion, got %q", tb.last())
				}
				return
			}
			if !strings.Contains(tb.last(), tt.wantSub) {
				t.Fatalf("failure message = %q, want it to contain %q", tb.last(), tt.wantSub)
			}
		})
	}
}

// TestNotificationWithoutParams covers a notification frame carrying no params
// at all, and one carrying null params. The params member is optional, so
// leaving it out says exactly what {} says and an empty expectation matches it;
// the specification models params as an object, so an explicit null is output no
// server may emit and must not satisfy that same expectation.
// Request.ReportProgress always writes a progressToken, so no handler can emit
// either today; the frames are recorded on the transport the way a streaming
// handler would emit them, and attributed through the same watermark path the
// drivers use.
func TestNotificationWithoutParams(t *testing.T) {
	frames := []struct {
		name string
		// frame is the recorded outbound notification.
		frame string
		// matchesEmpty is whether an empty expectation holds over those params.
		matchesEmpty bool
		// wantDescribed is how the failure message renders those params: what
		// arrived, not a stand-in for it.
		wantDescribed string
	}{
		{"absent params", `{"jsonrpc":"2.0","method":"booking/empty"}`, true, "(none)"},
		{"null params", `{"jsonrpc":"2.0","method":"booking/empty","params":null}`, false, "null"},
	}
	for _, tt := range frames {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{fake: transport.NewFake(nil), ctx: context.Background()}
			if err := s.fake.Send(s.ctx, []byte(tt.frame)); err != nil {
				t.Fatalf("record frame: %v", err)
			}

			tb := &recordingTB{}
			r := s.withNotifications(0, newResponse(tb, "tools/call", nil))
			if len(r.notifications) != 1 {
				t.Fatalf("attributed %d notifications, want 1", len(r.notifications))
			}

			// An empty expectation matches an omitted params member, and only
			// that: a frame that wrote null instead is reported as it arrived.
			r.AssertSentNotificationWith("booking/empty", map[string]any{})
			if tt.matchesEmpty {
				if len(tb.messages) != 0 {
					t.Fatalf("an empty expectation must match a params-less notification, got %q", tb.last())
				}
			} else {
				if len(tb.messages) != 1 {
					t.Fatalf("reported %d failures, want 1: %v", len(tb.messages), tb.messages)
				}
				if want := "with params {}, got: " + tt.wantDescribed; !strings.Contains(tb.last(), want) {
					t.Fatalf("failure message = %q, want it to contain %q", tb.last(), want)
				}
			}

			// A non-empty expectation matches neither, and the failure reports
			// what arrived rather than a decoded stand-in.
			r.AssertSentNotificationWith("booking/empty", map[string]any{"step": 1})
			if want := `with params {"step":1}, got: ` + tt.wantDescribed; !strings.Contains(tb.last(), want) {
				t.Fatalf("failure message = %q, want it to contain %q", tb.last(), want)
			}
		})
	}
}

// TestMergeListPages_CursorLoop covers the guard on a server that keeps handing
// out a cursor it has already issued. A real server's cursor advances, so this
// drives the page walk directly: without the guard the walk would never return.
func TestMergeListPages_CursorLoop(t *testing.T) {
	calls := 0
	page, problem := mergeListPages("tools", func(cursor string) *Response {
		calls++
		if calls > 10 {
			t.Fatal("mergeListPages did not stop on a repeated cursor")
		}
		return replyFrom(t, nil, "tools/list",
			`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t"}],"nextCursor":"same"}}`)
	})
	if problem == "" {
		t.Fatal("a repeated cursor must be reported")
	}
	if !strings.Contains(problem, `repeated pagination cursor "same"`) {
		t.Fatalf("problem = %q", problem)
	}
	// The pages walked so far are still merged, so the reported failure is the
	// cursor, not a confusing empty catalogue.
	if got := len(page.listItems("tools")); got != 2 {
		t.Fatalf("merged %d items before the guard fired, want 2", got)
	}
}

// recordingTB is a minimal testing.TB that records Fatalf instead of aborting,
// so the branches below can be driven without a goroutine dance. Every assertion
// returns immediately after reporting, so execution continuing past the call is
// harmless here.
type recordingTB struct {
	testing.TB
	messages []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *recordingTB) last() string {
	if len(r.messages) == 0 {
		return ""
	}
	return r.messages[len(r.messages)-1]
}
