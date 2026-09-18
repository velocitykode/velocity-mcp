package client

import (
	"context"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/server"
)

// initialize performs the initialize request directly on the transport (used
// during connect, before the connected flag is set) and parses the result.
func (p *protocol) initialize(ctx context.Context) (*InitializeResult, error) {
	// initialize is the legacy handshake, so it may only ask for a version that
	// handshake negotiates over. The newest revision opens with server/discover
	// instead and is deliberately never requested here.
	params := map[string]any{
		"protocolVersion": server.InitializeSupportedVersions()[0],
		"capabilities":    map[string]any{},
		"clientInfo":      p.clientInfo.ToMap(),
	}
	raw, err := p.attempt(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	result, err := parseInitializeResult(raw)
	if err != nil {
		return nil, err
	}
	// Later requests must advertise the version that was actually settled on,
	// not the newest one this package knows: a server that negotiated an older
	// revision rejects a request stamped with a version it does not speak.
	if pinner, ok := p.transport.(protocolPinner); ok {
		pinner.useProtocol(result.ProtocolVersion)
	}
	return result, nil
}

// list fetches every entry of a paginated list method (tools/list, etc.),
// transparently following nextCursor. listType is the plural primitive name and
// also the result key holding the page. A non-empty limit caps the number of
// entries returned (a limit of zero returns none).
func (c *Client) list(ctx context.Context, listType string, limit []int) ([]map[string]any, error) {
	lim, hasLimit, err := resolveLimit(listType, limit)
	if err != nil {
		return nil, err
	}
	if hasLimit && lim == 0 {
		return nil, nil
	}

	var items []map[string]any
	cursor := ""
	seen := map[string]bool{}

	for {
		if cursor != "" {
			if seen[cursor] {
				return nil, newError("repeated " + listType + "/list cursor [" + cursor + "] received from server")
			}
			seen[cursor] = true
		}

		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}

		raw, err := c.proto.dispatch(ctx, listType+"/list", params)
		if err != nil {
			return nil, err
		}

		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, newError("invalid " + listType + "/list response from server")
		}
		page, ok := result[listType].([]any)
		if !ok {
			return nil, newError("invalid " + listType + "/list response from server")
		}
		for _, entry := range page {
			m, ok := entry.(map[string]any)
			if !ok {
				return nil, newError("invalid " + listType + " payload from server")
			}
			if hasLimit && len(items) >= lim {
				return items, nil
			}
			items = append(items, m)
		}

		next, _ := result["nextCursor"].(string)
		if next == "" {
			return items, nil
		}
		cursor = next
	}
}

// resolveLimit interprets the optional variadic limit: absent means unlimited, a
// negative value is an error, and a non-negative value caps the result count.
func resolveLimit(listType string, limit []int) (int, bool, error) {
	if len(limit) == 0 {
		return 0, false, nil
	}
	if limit[0] < 0 {
		return 0, false, newError(listType + " list limit must be greater than or equal to zero")
	}
	return limit[0], true, nil
}
