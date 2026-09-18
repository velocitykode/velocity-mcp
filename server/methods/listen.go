package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// Listen handles "subscriptions/listen": a client opens a subscription and the
// server streams change notifications over it for as long as it holds it open.
//
// This server holds no subscriptions. It acknowledges the request with an empty
// notification filter and then closes the subscription immediately by returning
// its result, so a client learns at once that nothing will be pushed instead of
// waiting on a stream that never produces. Both frames carry the subscription
// id, which is the id of the request that opened it.
type Listen struct{}

// Compile-time assertion that the handler satisfies server.Method.
var _ server.Method = Listen{}

// subscriptionAcknowledged is the notification confirming which of the
// requested notification kinds the server will actually deliver.
const subscriptionAcknowledged = "notifications/subscriptions/acknowledged"

// Handle acknowledges the subscription and closes it.
func (Listen) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	// Notify is a no-op on a transport that cannot push frames, in which case
	// the client still learns the outcome from the result below.
	err := c.Notify(subscriptionAcknowledged, map[string]any{
		"_meta": subscriptionMeta(req),
		// An empty object, not an empty list: the acknowledgement mirrors the
		// shape of the requested filter and this server subscribes to nothing.
		"notifications": map[string]any{},
	})
	if err != nil {
		return nil, err
	}

	return jsonrpc.NewResult(req.ID, map[string]any{"_meta": subscriptionMeta(req)})
}

// subscriptionMeta builds the _meta bag correlating a frame with the
// subscription, keyed by the opening request's id and echoing it in its
// original JSON form (a number stays a number, a string stays a string).
func subscriptionMeta(req *jsonrpc.Request) map[string]any {
	return map[string]any{server.MetaKeySubscriptionID: req.ID}
}
