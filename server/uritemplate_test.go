package server

import "testing"

func TestMatchURITemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		uri      string
		wantOK   bool
		wantVars map[string]string
	}{
		{"single var", "file://users/{id}", "file://users/42", true, map[string]string{"id": "42"}},
		{"no match prefix", "file://users/{id}", "http://users/42", false, nil},
		{"two vars", "db://{table}/{id}", "db://users/7", true, map[string]string{"table": "users", "id": "7"}},
		{"trailing literal", "file://docs/{name}.txt", "file://docs/readme.txt", true, map[string]string{"name": "readme"}},
		{"final captures slashes", "file://docs/{path}", "file://docs/a/b/c", true, map[string]string{"path": "a/b/c"}},
		{"no placeholders exact", "file://static", "file://static", true, map[string]string{}},
		{"no placeholders mismatch", "file://static", "file://other", false, nil},
		{"empty var rejected", "file://users/{id}", "file://users/", false, nil},
		{"final captures remaining slashes", "db://{table}/{id}", "db://a/b/7", true, map[string]string{"table": "a", "id": "b/7"}},
		{"non-final segment must not span slash", "x/{a}-{b}", "x/1/2-3", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars, ok := MatchURITemplate(tt.template, tt.uri)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if len(vars) != len(tt.wantVars) {
				t.Fatalf("vars = %v want %v", vars, tt.wantVars)
			}
			for k, v := range tt.wantVars {
				if vars[k] != v {
					t.Fatalf("vars[%q] = %q want %q", k, vars[k], v)
				}
			}
		})
	}
}

func TestParseTemplate(t *testing.T) {
	literals, names := parseTemplate("a{x}b{y}c")
	if len(literals) != 3 || len(names) != 2 {
		t.Fatalf("literals=%v names=%v", literals, names)
	}
	if literals[0] != "a" || literals[1] != "b" || literals[2] != "c" {
		t.Fatalf("literals = %v", literals)
	}
	if names[0] != "x" || names[1] != "y" {
		t.Fatalf("names = %v", names)
	}

	// Unterminated placeholder is treated as a trailing literal.
	lit, nm := parseTemplate("a{unterminated")
	if len(nm) != 0 || lit[len(lit)-1] != "a{unterminated" {
		t.Fatalf("unterminated handling: lit=%v nm=%v", lit, nm)
	}
}
