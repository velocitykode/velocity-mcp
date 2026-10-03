package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// This file covers a request the client stops waiting for over streamable HTTP,
// where the revision decides how it is withdrawn. The revision without a
// session (2026-07-28, basic/utilities/cancellation) makes closing the response
// stream the cancellation. The revisions before it (2025-11-25,
// basic/transports#streamable-http) say a disconnection is not to be read as
// one and have the client send notifications/cancelled. Under either the
// connection is not what was abandoned, and it stands.

// holdingEndpoint negotiates the given era and holds every prompts/list on an
// open event stream until the request is withdrawn, then answers the ones after
// the first. It answers DELETE and accepts every notification.
func holdingEndpoint(t *testing.T, legacy bool) *recordingEndpoint {
	t.Helper()
	var endpoint *recordingEndpoint
	held := false
	endpoint = newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch {
		case request.httpMethod == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case request.method == "server/discover" && legacy:
			w.WriteHeader(http.StatusBadRequest)
		case request.method == "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType":        "complete",
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{"prompts": map[string]any{}},
				"ttlMs":             600000,
			}}))
		case request.method == "initialize":
			w.Header().Set(sessionHeader, "session-1")
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"protocolVersion": ProtocolV20251125,
				"capabilities":    map[string]any{"prompts": map[string]any{}},
				"serverInfo":      map[string]any{"name": "s", "version": "1"},
			}}))
		case request.method == "prompts/list":
			endpoint.mu.Lock()
			first := !held
			held = true
			serving := endpoint.serving
			endpoint.mu.Unlock()
			if !first {
				writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
					"resultType": "complete",
					"prompts":    []any{map[string]any{"name": "p"}},
					"ttlMs":      0,
				}}))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
			<-serving.Context().Done()
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	return endpoint
}

// TestAnAbandonedHTTPRequestLeavesTheConnectionStanding asserts what a request
// the client stops waiting for costs over streamable HTTP: the notification
// where the revision asks for one, nothing where closing the stream says it, and
// in neither case the connection. The session is not released, nothing is
// negotiated again, and the next request is answered.
func TestAnAbandonedHTTPRequestLeavesTheConnectionStanding(t *testing.T) {
	const short = 300 * time.Millisecond
	withdraw := func(c *WebClient) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(short, cancel)
		_, err := c.Prompts(ctx)
		return err
	}
	deadline := func(c *WebClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), short)
		defer cancel()
		_, err := c.Prompts(ctx)
		return err
	}

	tests := []struct {
		name        string
		legacy      bool
		abandon     func(c *WebClient) error
		wantTimeout bool
		// wantNotices is how many notifications/cancelled the server is sent.
		wantNotices int
		wantReason  string
		// handshake is the request that settles a connection of the era.
		handshake string
	}{
		{name: "a session revision, the caller withdraws", legacy: true, abandon: withdraw,
			wantNotices: 1, wantReason: "the caller withdrew the request", handshake: "initialize"},
		{name: "a session revision, the deadline passes", legacy: true, abandon: deadline, wantTimeout: true,
			wantNotices: 1, wantReason: "the request timed out", handshake: "initialize"},
		{name: "the revision without a session, the caller withdraws", abandon: withdraw,
			handshake: "server/discover"},
		{name: "the revision without a session, the deadline passes", abandon: deadline, wantTimeout: true,
			handshake: "server/discover"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := holdingEndpoint(t, tt.legacy)
			c := Web(endpoint.URL)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			err := tt.abandon(c)
			if err == nil {
				t.Fatal("a request that was never answered reported success")
			}
			var timeoutErr *TimeoutError
			if got := errors.As(err, &timeoutErr); got != tt.wantTimeout {
				t.Fatalf("error = %v (%T), timeout = %v, want %v", err, err, got, tt.wantTimeout)
			}

			notices := endpoint.requestsFor("notifications/cancelled")
			if len(notices) != tt.wantNotices {
				t.Fatalf("notifications/cancelled was POSTed %d time(s), want %d", len(notices), tt.wantNotices)
			}
			if tt.wantNotices > 0 {
				abandoned := endpoint.request(t, "prompts/list")
				var notice struct {
					ID     json.RawMessage `json:"id"`
					Params struct {
						RequestID json.RawMessage `json:"requestId"`
						Reason    string          `json:"reason"`
					} `json:"params"`
				}
				if err := json.Unmarshal([]byte(notices[0].body), &notice); err != nil {
					t.Fatalf("notice: %v", err)
				}
				if len(notice.ID) != 0 {
					t.Fatalf("the cancellation carries the id %s; it is a notification", notice.ID)
				}
				if string(notice.Params.RequestID) != string(abandoned.id) {
					t.Fatalf("the cancellation names request %s, want %s", notice.Params.RequestID, abandoned.id)
				}
				if notice.Params.Reason != tt.wantReason {
					t.Fatalf("reason = %q, want %q", notice.Params.Reason, tt.wantReason)
				}
				if got := notices[0].headers.Get(sessionHeader); got != "session-1" {
					t.Fatalf("the cancellation presented session %q, want session-1", got)
				}
			}

			if !c.Connected() {
				t.Fatal("the abandoned request took the connection down")
			}
			prompts, err := c.Prompts(context.Background())
			if err != nil {
				t.Fatalf("the request after the abandoned one: %v", err)
			}
			if len(prompts) != 1 || prompts[0].Name != "p" {
				t.Fatalf("prompts = %+v, want the one the server has", prompts)
			}
			if got := len(endpoint.requestsFor(tt.handshake)); got != 1 {
				t.Fatalf("the connection was settled %d time(s), want once", got)
			}
			endpoint.mu.Lock()
			defer endpoint.mu.Unlock()
			for _, request := range endpoint.requests {
				if request.httpMethod == http.MethodDelete {
					t.Fatal("the session was released over a request the client gave up on")
				}
			}
		})
	}
}
