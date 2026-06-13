package content

import (
	"fmt"
	"time"
)

// Role identifies an intended audience for a piece of content. Per the MCP
// spec a content annotation's "audience" is a set of roles describing who the
// content is for, so a client can decide whether to show it.
type Role string

const (
	// RoleUser marks content intended for the human user.
	RoleUser Role = "user"
	// RoleAssistant marks content intended for the assistant model.
	RoleAssistant Role = "assistant"
)

// valid reports whether r is a role defined by the MCP spec.
func (r Role) valid() bool { return r == RoleUser || r == RoleAssistant }

// annotations is the embedded helper carrying a content value's optional MCP
// annotations: audience, priority, and lastModified. Each is omitted from the
// wire shape until set, and the populated set is emitted under the
// "annotations" key. The fields are hints only: clients may use them to
// prioritize or filter content but are not required to honor them.
type annotations struct {
	audience     []Role
	priority     *float64
	lastModified string
}

// SetAudience sets the intended audience to the given roles, replacing any
// previously set audience. Unknown roles are rejected so an invalid audience
// never reaches the wire. Passing no roles clears the audience.
func (a *annotations) SetAudience(roles ...Role) error {
	seen := make(map[Role]struct{}, len(roles))
	out := make([]Role, 0, len(roles))
	for _, r := range roles {
		if !r.valid() {
			return fmt.Errorf("content: invalid audience role %q", string(r))
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if len(out) == 0 {
		a.audience = nil
		return nil
	}
	a.audience = out
	return nil
}

// SetPriority sets the content priority, a hint in the inclusive range 0.0 to
// 1.0 where higher means more important. A value outside that range is
// rejected.
func (a *annotations) SetPriority(p float64) error {
	if p < 0.0 || p > 1.0 {
		return fmt.Errorf("content: priority must be between 0.0 and 1.0, got %v", p)
	}
	a.priority = &p
	return nil
}

// SetLastModified sets the content's last-modified timestamp, which must be an
// ISO 8601 / RFC 3339 string (optionally date-only). An empty string clears it.
func (a *annotations) SetLastModified(ts string) error {
	if ts == "" {
		a.lastModified = ""
		return nil
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		if _, err2 := time.Parse("2006-01-02", ts); err2 != nil {
			return fmt.Errorf("content: lastModified must be an ISO 8601 timestamp, got %q", ts)
		}
	}
	a.lastModified = ts
	return nil
}

// annotationsMap returns the populated annotation keys, or nil when none are
// set. Audience is copied so callers cannot mutate internal state through the
// marshaled output.
func (a *annotations) annotationsMap() map[string]any {
	m := map[string]any{}
	if len(a.audience) > 0 {
		aud := make([]string, len(a.audience))
		for i, r := range a.audience {
			aud[i] = string(r)
		}
		m["audience"] = aud
	}
	if a.priority != nil {
		m["priority"] = *a.priority
	}
	if a.lastModified != "" {
		m["lastModified"] = a.lastModified
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// mergeAnnotations adds the "annotations" key to base when any annotation is
// set, without overwriting an existing key. It does not mutate base otherwise.
func (a *annotations) mergeAnnotations(base map[string]any) map[string]any {
	am := a.annotationsMap()
	if am == nil {
		return base
	}
	if _, exists := base["annotations"]; !exists {
		base["annotations"] = am
	}
	return base
}
