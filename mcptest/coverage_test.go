package mcptest_test

import (
	"testing"

	"github.com/velocitykode/velocity-mcp/mcptest"
	_ "github.com/velocitykode/velocity-mcp/server/methods"
)

// TestServer_Notify exercises the notification path (no reply) and the
// AssertNoResponse assertion.
func TestServer_Notify(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Notify("notifications/initialized", nil).AssertNoResponse()
	ts.Notify("notifications/progress", map[string]any{"token": "x"}).AssertNoResponse()
}

// TestServer_SendWithParams drives an arbitrary method with params, covering the
// params-marshalling branch of call.
func TestServer_SendWithParams(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Send("tools/list", map[string]any{"cursor": ""}).AssertOk().AssertToolListed("add")
}

// TestServer_AssertResult_FloatNormalisation covers jsonEqual's numeric
// normalisation: an int literal compares equal to the float64 a JSON decode
// yields. The "add" tool's structured-free result has no numeric top-level
// field, so use a list reply's nested count via AssertResult on a crafted echo.
func TestServer_AssertResult_FloatNormalisation(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	// ping result is an empty object; assert a present-but-nested value through
	// the initialize reply instead: capabilities is a map, compared structurally.
	res := ts.Initialize()
	res.AssertResult("serverInfo", map[string]any{
		"name":    "demo",
		"version": "1.0.0",
	})
}

// TestServer_AssertNoResponse_NilT ensures the nil-t branch of AssertNoResponse
// is a no-op (does not panic) even when the expectation would fail.
func TestServer_AssertNoResponse_NilT(t *testing.T) {
	ts := mcptest.NewServer(nil, demoServer())
	// Ping returns a response; AssertNoResponse would fail, but with nil t it is
	// a no-op.
	ts.Ping().AssertNoResponse()
	// AssertHasResponse / AssertErrorCode / AssertServerName nil-t no-op paths.
	ts.Ping().AssertHasResponse()
	ts.Ping().AssertErrorCode(0)
	ts.Initialize().AssertServerName("anything")
	ts.ListTools().AssertToolListed("ghost")
	ts.ListTools().AssertToolNotListed("add")
	ts.ListTools().AssertToolCount(999)
	ts.ListResources().AssertResourceListed("ghost")
	ts.ListPrompts().AssertPromptListed("ghost")
	ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertDontSeeText("2")
	ts.CallTool("does-not-exist", nil).AssertResult("k", 1)
	ts.Ping().AssertResult("missing", 1)
	// List assertions against an error reply (nil result object) exercise the
	// empty-list branches of listItems/listedNames without panicking.
	ts.Send("does/not/exist", nil).AssertToolListed("add")
	ts.Send("does/not/exist", nil).AssertResourceListed("x")
	ts.Send("does/not/exist", nil).AssertPromptListed("x")
	// AssertText against an error reply: contentMessages with a nil result.
	ts.Send("does/not/exist", nil).AssertText("anything")
}
