package ui

// Permission is a browser capability an app resource requests for its sandboxed
// frame. The host decides whether to grant it.
type Permission string

const (
	// PermissionCamera requests access to camera input.
	PermissionCamera Permission = "camera"
	// PermissionMicrophone requests access to microphone input.
	PermissionMicrophone Permission = "microphone"
	// PermissionGeolocation requests access to device location.
	PermissionGeolocation Permission = "geolocation"
	// PermissionClipboardWrite requests permission to write to the clipboard.
	PermissionClipboardWrite Permission = "clipboardWrite"
)

// Permissions is the set of browser capabilities an app resource requests,
// rendered as an object keyed by permission name (each value an empty object,
// leaving room for per-permission options in the wire format).
type Permissions struct {
	enabled []Permission
}

// NewPermissions builds an empty permission set.
func NewPermissions() Permissions { return Permissions{} }

// Allow adds the given permissions to the set, preserving order and ignoring
// duplicates.
func (p Permissions) Allow(perms ...Permission) Permissions {
	seen := make(map[Permission]struct{}, len(p.enabled))
	for _, e := range p.enabled {
		seen[e] = struct{}{}
	}
	for _, perm := range perms {
		if _, dup := seen[perm]; dup {
			continue
		}
		seen[perm] = struct{}{}
		p.enabled = append(p.enabled, perm)
	}
	return p
}

// isEmpty reports whether no permission is enabled.
func (p Permissions) isEmpty() bool { return len(p.enabled) == 0 }

// ToMap renders the set as an object keyed by permission name, each value an
// empty object.
func (p Permissions) ToMap() map[string]any {
	m := make(map[string]any, len(p.enabled))
	for _, perm := range p.enabled {
		m[string(perm)] = map[string]any{}
	}
	return m
}
