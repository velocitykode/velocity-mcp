package server_test

import (
	"context"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/ui"
)

// dashboardApp is an app resource: registering one is what makes a server
// advertise the MCP Apps extension.
func dashboardApp() server.Resource {
	return server.NewAppResource("dashboard", "ui://dashboard").
		HTMLFunc(func(ctx context.Context, req *server.Request) (string, error) {
			return "<p>hi</p>", nil
		})
}

// appTemplate is an app resource addressed by a URI template, so it is
// registered on the template set rather than the plain resource set.
type appTemplate struct{}

func (appTemplate) Name() string        { return "app-user" }
func (appTemplate) Description() string { return "a per-user app" }
func (appTemplate) URI() string         { return "ui://users/{id}" }
func (appTemplate) URITemplate() string { return "ui://users/{id}" }
func (appTemplate) MimeType() string    { return server.AppResourceMimeType }
func (appTemplate) AppMeta() ui.AppMeta { return ui.NewAppMeta() }
func (appTemplate) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("<p>hi</p>"), nil
}

// extensionsOf reads the advertised extensions object out of a discover result.
func extensionsOf(t *testing.T, s *server.Server) (map[string]any, bool) {
	t.Helper()
	result := decodeResult(t, handle(t, s, modernRequest(1, "server/discover")).Response)
	caps := result["capabilities"].(map[string]any)
	extensions, ok := caps["extensions"].(map[string]any)
	return extensions, ok
}

// TestUICapabilityAdvertisedForAppResources asserts that registering an app
// resource advertises the MCP Apps extension, without the author declaring it:
// the host has to know before it reads the resource that it may render it.
func TestUICapabilityAdvertisedForAppResources(t *testing.T) {
	tests := []struct {
		name     string
		resource server.Resource
	}{
		{"plain app resource", dashboardApp()},
		{"templated app resource", appTemplate{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithResources(tt.resource))
			extensions, ok := extensionsOf(t, s)
			if !ok {
				t.Fatal("no extensions advertised")
			}
			entry, present := extensions[server.ExtensionUI]
			if !present {
				t.Fatalf("extensions = %v, want the ui extension", extensions)
			}
			if m, isObject := entry.(map[string]any); !isObject || len(m) != 0 {
				t.Fatalf("ui extension = %#v, want an empty object", entry)
			}
		})
	}
}

// TestNoExtensionsWithoutAppResources asserts a server with only ordinary
// resources advertises no extensions object at all, rather than an empty one a
// host would have to interpret.
func TestNoExtensionsWithoutAppResources(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithTools(addTool()))
	if extensions, ok := extensionsOf(t, s); ok {
		t.Fatalf("extensions advertised without an app resource: %v", extensions)
	}
}

// TestWithExtensions asserts a server can advertise an extension explicitly,
// that doing so twice is harmless, and that an empty name is ignored.
func TestWithExtensions(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithExtensions("example.com/experiment", ""),
		server.WithExtensions("example.com/experiment", "example.com/other"),
	)
	extensions, ok := extensionsOf(t, s)
	if !ok {
		t.Fatal("no extensions advertised")
	}
	if len(extensions) != 2 {
		t.Fatalf("extensions = %v, want exactly two", extensions)
	}
	for _, want := range []string{"example.com/experiment", "example.com/other"} {
		if _, present := extensions[want]; !present {
			t.Fatalf("extension %q missing from %v", want, extensions)
		}
	}
}

// TestExplicitAndDetectedExtensionsCoexist asserts auto-detection adds to the
// declared set rather than replacing it.
func TestExplicitAndDetectedExtensionsCoexist(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithExtensions("example.com/experiment"),
		server.WithResources(dashboardApp()),
	)
	extensions, ok := extensionsOf(t, s)
	if !ok {
		t.Fatal("no extensions advertised")
	}
	if _, present := extensions[server.ExtensionUI]; !present {
		t.Fatalf("ui extension missing from %v", extensions)
	}
	if _, present := extensions["example.com/experiment"]; !present {
		t.Fatalf("declared extension missing from %v", extensions)
	}
}

// TestUICapabilityIsNotATopLevelCapability asserts the extension is advertised
// only under "extensions": a top-level entry of the same name would be a second,
// conflicting statement of the same fact.
func TestUICapabilityIsNotATopLevelCapability(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithResources(dashboardApp()))
	result := decodeResult(t, handle(t, s, modernRequest(1, "server/discover")).Response)
	caps := result["capabilities"].(map[string]any)
	if _, present := caps[server.ExtensionUI]; present {
		t.Fatalf("ui advertised as a top-level capability: %v", caps)
	}
}
