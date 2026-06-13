// Package ui carries the metadata for MCP UI app resources: the optional
// extension where a resource serves an interactive HTML application (mimeType
// "text/html;profile=mcp-app") that an MCP host renders in a sandboxed frame.
//
// The types here are pure value builders with no dependency on the rest of the
// SDK (a leaf package, stdlib only). A resource advertises its app metadata
// under the "_meta.ui" key of its resources/list entry; the host uses it to
// size the frame, grant browser permissions, and constrain the Content Security
// Policy. AppMeta is the top-level builder; Csp, Permissions, and Library shape
// its parts.
//
// This metadata is an MCP UI extension, not part of the core protocol; a host
// that does not understand it ignores the _meta.ui key and treats the resource
// as ordinary HTML.
package ui
