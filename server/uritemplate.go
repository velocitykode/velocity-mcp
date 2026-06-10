package server

import "strings"

// MatchURITemplate matches a concrete uri against an RFC 6570-style template
// containing simple "{var}" placeholders (e.g. "file://users/{id}") and returns
// the extracted variables. ok is false when the uri does not match the template.
//
// The matcher supports the common simple-expansion case used by MCP resource
// templates: each "{name}" placeholder matches a single path segment (no "/")
// by default. The trailing placeholder may match the remainder of the uri
// (including slashes) so templates like "file://docs/{path}" capture nested
// paths. A template with no placeholders matches only an identical uri.
//
// This is intentionally a small, dependency-free matcher rather than a full RFC
// 6570 implementation: MCP resource templates in practice use only simple {var}
// expansion, and pulling a heavyweight URI-template library would violate the
// leaf/light dependency goals of this package.
func MatchURITemplate(template, uri string) (vars map[string]string, ok bool) {
	literals, names := parseTemplate(template)
	if len(names) == 0 {
		// No placeholders: exact match only.
		if template == uri {
			return map[string]string{}, true
		}
		return nil, false
	}

	vars = make(map[string]string, len(names))
	pos := 0

	// The text before the first placeholder must be a literal prefix.
	if !strings.HasPrefix(uri[pos:], literals[0]) {
		return nil, false
	}
	pos += len(literals[0])

	for i, name := range names {
		nextLiteral := literals[i+1]
		isLast := i == len(names)-1

		if isLast {
			rest := uri[pos:]
			if nextLiteral != "" {
				// Placeholder followed by a trailing literal: the value is the
				// text up to the final occurrence of that literal.
				idx := strings.LastIndex(rest, nextLiteral)
				if idx < 0 || idx+len(nextLiteral) != len(rest) {
					return nil, false
				}
				value := rest[:idx]
				if value == "" {
					return nil, false
				}
				vars[name] = value
				return vars, true
			}
			// Final placeholder captures the remainder (may include slashes).
			if rest == "" {
				return nil, false
			}
			vars[name] = rest
			return vars, true
		}

		// Non-final placeholder: capture up to the next literal, within a single
		// path segment (no "/") so adjacent placeholders stay unambiguous.
		idx := strings.Index(uri[pos:], nextLiteral)
		if idx <= 0 {
			return nil, false
		}
		value := uri[pos : pos+idx]
		if strings.Contains(value, "/") {
			return nil, false
		}
		vars[name] = value
		pos += idx + len(nextLiteral)
	}

	return vars, true
}

// parseTemplate splits a template into the literal segments surrounding each
// "{name}" placeholder and the ordered placeholder names. literals always has
// len(names)+1 elements (the text before the first, between each, and after the
// last placeholder).
func parseTemplate(template string) (literals []string, names []string) {
	rest := template
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			literals = append(literals, rest)
			break
		}
		close := strings.Index(rest[open:], "}")
		if close < 0 {
			// Unterminated placeholder: treat the remainder as a literal.
			literals = append(literals, rest)
			break
		}
		close += open
		literals = append(literals, rest[:open])
		names = append(names, rest[open+1:close])
		rest = rest[close+1:]
	}
	return literals, names
}
