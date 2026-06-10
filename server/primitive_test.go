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
