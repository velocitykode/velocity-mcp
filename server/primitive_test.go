package server

import (
	"context"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

type WeatherTool struct{}

func (WeatherTool) Name() string            { return "weather" }
func (WeatherTool) Description() string     { return "get weather" }
func (WeatherTool) Schema(s *schema.Object) {}
func (WeatherTool) Handle(context.Context, *Request) (*Response, error) {
	return Text("sunny"), nil
}

func TestDefaultName(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"pointer to struct", &WeatherTool{}, "weather-tool"},
		{"value struct", WeatherTool{}, "weather-tool"},
		{"nil", nil, ""},
		{"unnamed type", struct{ X int }{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultName(tt.in); got != tt.want {
				t.Fatalf("DefaultName(%T) = %q want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPromptArgumentToMap(t *testing.T) {
	a := NewPromptArgument("city", "the city", true)
	m := a.ToMap()
	if m["name"] != "city" || m["description"] != "the city" || m["required"] != true {
		t.Fatalf("arg map = %v", m)
	}
}

func TestResourceAnnotationsToMap(t *testing.T) {
	if m := (ResourceAnnotations{}).ToMap(); len(m) != 0 {
		t.Fatalf("empty annotations = %v, want empty map", m)
	}
	p := 0.5
	m := ResourceAnnotations{
		Audience:     []Role{RoleUser},
		Priority:     &p,
		LastModified: "2026-06-13",
	}.ToMap()
	if m["priority"] != 0.5 || m["lastModified"] != "2026-06-13" {
		t.Fatalf("annotations = %v", m)
	}
	aud := m["audience"].([]string)
	if len(aud) != 1 || aud[0] != "user" {
		t.Fatalf("audience = %v", aud)
	}
	// A zero priority is distinct from unset and must be emitted.
	zero := 0.0
	if got := (ResourceAnnotations{Priority: &zero}).ToMap(); got["priority"] != 0.0 {
		t.Fatalf("zero priority dropped: %v", got)
	}
}

type titledTool struct{ WeatherTool }

func (titledTool) Title() string { return "Custom Title" }

func TestTitleOf(t *testing.T) {
	if got := titleOf("weather-tool", WeatherTool{}); got != "Weather Tool" {
		t.Fatalf("headline title = %q", got)
	}
	if got := titleOf("weather-tool", titledTool{}); got != "Custom Title" {
		t.Fatalf("explicit title = %q", got)
	}
}
