package client

import (
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/schema"
)

// DiscoverResult is the outcome of a server/discover request: the protocol
// versions the server speaks, its capabilities, and the optional identity and
// instructions it advertises. The server identity travels in the result _meta
// rather than in a top-level member.
type DiscoverResult struct {
	// SupportedVersions lists the protocol versions the server speaks, in the
	// order it advertised them.
	SupportedVersions []string
	// Capabilities is the server's capability map.
	Capabilities map[string]any
	// ServerInfo identifies the server. Its Name is empty when the server did
	// not advertise an identity.
	ServerInfo schema.Implementation
	// Instructions is the optional usage guidance the server advertises.
	Instructions string
}

// parseDiscoverResult decodes and validates a server/discover result. A payload
// without a supportedVersions array and a capabilities object is rejected:
// those two members are what makes the answer a discover result rather than
// some other server's idea of an empty success.
func parseDiscoverResult(raw json.RawMessage) (*DiscoverResult, error) {
	var payload struct {
		SupportedVersions json.RawMessage `json:"supportedVersions"`
		Capabilities      json.RawMessage `json:"capabilities"`
		Instructions      json.RawMessage `json:"instructions"`
		Meta              json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid discover response from server")
	}

	var offered []any
	if err := decodeMember(payload.SupportedVersions, &offered); err != nil || offered == nil {
		return nil, newError("invalid discover response from server")
	}
	var capabilities map[string]any
	if err := decodeMember(payload.Capabilities, &capabilities); err != nil || capabilities == nil {
		return nil, newError("invalid discover response from server")
	}

	// Non-string entries are ignored rather than rejected: an unknown future
	// entry must not stop the client negotiating a version it does understand.
	versions := make([]string, 0, len(offered))
	for _, entry := range offered {
		if version, ok := entry.(string); ok {
			versions = append(versions, version)
		}
	}

	result := &DiscoverResult{SupportedVersions: versions, Capabilities: capabilities}
	// Instructions and the server identity are optional: a payload that carries
	// them in another shape is read as if it carried none.
	_ = decodeMember(payload.Instructions, &result.Instructions)

	var meta map[string]json.RawMessage
	if err := decodeMember(payload.Meta, &meta); err == nil {
		result.ServerInfo, _ = parseImplementation(meta[MetaServerInfo])
	}
	return result, nil
}

// decodeMember decodes a result member into dest, reporting an error when the
// member is absent. A member present as JSON null decodes to the zero value,
// which callers check for when the member is required.
func decodeMember(raw json.RawMessage, dest any) error {
	if len(raw) == 0 {
		return newError("missing member")
	}
	return json.Unmarshal(raw, dest)
}

// parseImplementation decodes an implementation identity, reporting false when
// the payload is absent or carries no name and version.
func parseImplementation(raw json.RawMessage) (schema.Implementation, bool) {
	if len(raw) == 0 {
		return schema.Implementation{}, false
	}
	var payload struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Title       string `json:"title"`
		Description string `json:"description"`
		WebsiteURL  string `json:"websiteUrl"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return schema.Implementation{}, false
	}
	if payload.Name == "" || payload.Version == "" {
		return schema.Implementation{}, false
	}
	info := schema.NewImplementation(payload.Name, payload.Version)
	info.Title = payload.Title
	info.Description = payload.Description
	info.WebsiteURL = payload.WebsiteURL
	return info, true
}
