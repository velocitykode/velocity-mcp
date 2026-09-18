package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/velocitykode/velocity/httpclient"
)

// hostPosture names what the endpoint client may open a connection to. Every
// URL it is handed was advertised by the protected resource or by the
// authorization server the resource named, so the posture is what stands
// between a hostile one of those and the network this application runs in.
type hostPosture int

const (
	// publicHosts is the default. velocity's dial-time guard resolves the host
	// of every connection, refuses it when any address it resolves to is
	// loopback, private, link-local or otherwise internal, and connects to the
	// address it checked. A name that resolves inward is therefore refused
	// however it is spelled, and an answer that changes between the check and
	// the connection (DNS rebinding) has nothing to change.
	publicHosts hostPosture = iota
	// loopbackHosts keeps that guard and exempts the loopback names alone. It
	// applies when the protected resource the application configured is itself
	// on the loopback interface, the development arrangement in which the
	// authorization server is another port of the same machine.
	loopbackHosts
	// privateHosts lifts the guard. It is Config.AllowPrivateHosts, the opt-in
	// of a consumer that vouches for endpoints on a private network.
	privateHosts
)

// loopbackNames are the hosts recognised as the loopback interface: the names
// isLocalhost answers for, and the ones the loopbackHosts posture exempts from
// the dial-time guard. They are compared exactly, so a name that merely
// resolves to the loopback interface is not one of them.
var loopbackNames = []string{"localhost", "127.0.0.1", "::1"}

// postureFor picks the posture of a flow against resourceURL. The resource URL
// is the one address in a flow the application supplied itself, which is why it
// alone may widen what the flow connects to.
func postureFor(allowPrivate bool, resourceURL string) hostPosture {
	if allowPrivate {
		return privateHosts
	}
	if u, err := url.Parse(resourceURL); err == nil && isLocalhost(normalizedHost(u.Hostname())) {
		return loopbackHosts
	}
	return publicHosts
}

// endpointClient builds the velocity httpclient used for OAuth metadata,
// registration, and token requests. Redirects are not followed (max redirects
// 0) and timeouts are short, matching the conservative posture expected of
// authorization-server interactions. What it may connect to is decided by
// posture, and enforced by velocity's guard where the connection is opened
// rather than by inspecting the URL beforehand: only there is the address known.
func endpointClient(posture hostPosture) *httpclient.Client {
	opts := []httpclient.Option{
		httpclient.WithTimeout(5 * time.Second),
		httpclient.WithMaxRedirects(0),
	}
	switch posture {
	case loopbackHosts:
		opts = append(opts, httpclient.WithAllowedHosts(loopbackNames...))
	case privateHosts:
		opts = append(opts, httpclient.WithoutPrivateIPDeny())
	}
	return httpclient.New(opts...)
}

// getJSON performs a GET expecting a JSON object body, returning the HTTP status
// and decoded map. A non-object body yields a nil map with the status.
func getJSON(ctx context.Context, c *httpclient.Client, endpoint string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, wrapError(err, "unable to build request to [%s]", endpoint)
	}
	req.Header.Set("Accept", "application/json")
	return doJSON(c, req, endpoint)
}

// postForm performs an application/x-www-form-urlencoded POST expecting a JSON
// object body. When basicAuth is non-nil it is applied as HTTP Basic credentials.
//
// The client id and the secret are each encoded with the
// application/x-www-form-urlencoded algorithm before they become the user name
// and the password (RFC 6749 2.3.1). The server decodes them the same way after
// splitting at the first colon, so credentials sent as they stand reach it
// altered as soon as they hold a colon, a plus or percent sign, or a space.
func postForm(ctx context.Context, c *httpclient.Client, endpoint string, form url.Values, basicAuth *[2]string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, wrapError(err, "unable to build request to [%s]", endpoint)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth != nil {
		req.SetBasicAuth(url.QueryEscape(basicAuth[0]), url.QueryEscape(basicAuth[1]))
	}
	return doJSON(c, req, endpoint)
}

// postJSON performs an application/json POST expecting a JSON object body.
func postJSON(ctx context.Context, c *httpclient.Client, endpoint string, payload any) (int, map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, wrapError(err, "unable to encode request body for [%s]", endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, wrapError(err, "unable to build request to [%s]", endpoint)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return doJSON(c, req, endpoint)
}

// doJSON executes a prepared request and decodes a JSON object body. A body that
// is empty or not a JSON object decodes to a nil map without error, leaving the
// caller to interpret the status code.
func doJSON(c *httpclient.Client, req *http.Request, endpoint string) (int, map[string]any, error) {
	resp, err := c.Do(req.Context(), req)
	if err != nil {
		return 0, nil, wrapError(err, "request to [%s] failed", endpoint)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, wrapError(err, "unable to read response from [%s]", endpoint)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return resp.StatusCode, nil, nil
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, data, nil
}

// successful reports whether status is in the 2xx range.
func successful(status int) bool {
	return status >= 200 && status < 300
}
