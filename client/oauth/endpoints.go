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

// endpointClient builds the velocity httpclient used for OAuth metadata,
// registration, and token requests. Redirects are not followed (max redirects
// 0) and timeouts are short, matching the conservative posture expected of
// authorization-server interactions. Private-IP denial is disabled here because
// discovery applies its own RFC-aligned internal-host checks (which permit a
// localhost authorization server during development); the token and
// registration endpoints it then calls have already been validated.
func endpointClient() *httpclient.Client {
	return httpclient.New(
		httpclient.WithTimeout(5*time.Second),
		httpclient.WithMaxRedirects(0),
		httpclient.WithoutPrivateIPDeny(),
	)
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
func postForm(ctx context.Context, c *httpclient.Client, endpoint string, form url.Values, basicAuth *[2]string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, wrapError(err, "unable to build request to [%s]", endpoint)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth != nil {
		req.SetBasicAuth(basicAuth[0], basicAuth[1])
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
