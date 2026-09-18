package server_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/velocitykode/velocity/contract"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/server"
)

// silentTool returns no response at all, the case a result map has to render as
// an empty, non-error result.
func silentTool() server.Tool {
	return server.NewTool("silent-tool", "Returns no response").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return nil, nil
		})
}

// blobTool returns content that has no shape in a tool result.
func blobTool() server.Tool {
	return server.NewTool("blob-tool", "Returns a blob").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.NewResponse(content.NewBlob([]byte("raw"))), nil
		})
}

func TestInvokeToolResults(t *testing.T) {
	tests := []struct {
		name string
		tool server.Tool
		req  *server.Request
		// wantText is the text of the result's single content item.
		wantText    string
		wantIsError bool
	}{
		{
			name:        "a handler response becomes the result content",
			tool:        sayHiTool(),
			req:         server.NewRequest(map[string]any{"name": "Ada"}),
			wantText:    "Hello, Ada!",
			wantIsError: false,
		},
		{
			// A nil request must not be dereferenced: it runs as a request
			// carrying no arguments, which the tool rejects on its own terms.
			name:        "a nil request is run as a request with no arguments",
			tool:        sayHiTool(),
			req:         nil,
			wantText:    "The name field is required.",
			wantIsError: true,
		},
		{
			name:        "a validation failure becomes a tool error result",
			tool:        sayHiTool(),
			req:         server.NewRequest(map[string]any{}),
			wantText:    "The name field is required.",
			wantIsError: true,
		},
		{
			name:        "content with no tool shape becomes a tool error result",
			tool:        blobTool(),
			req:         server.NewRequest(nil),
			wantText:    "The tool returned content that cannot be represented in a tool result.",
			wantIsError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := server.InvokeTool(context.Background(), tc.tool, tc.req)
			if err != nil {
				t.Fatalf("InvokeTool returned %v, want no error", err)
			}
			if got := result["isError"]; got != tc.wantIsError {
				t.Errorf("isError = %v, want %v", got, tc.wantIsError)
			}
			if got := firstText(t, result); got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
		})
	}

	t.Run("a handler that returns nothing yields an empty result", func(t *testing.T) {
		result, err := server.InvokeTool(context.Background(), silentTool(), server.NewRequest(nil))
		if err != nil {
			t.Fatalf("InvokeTool returned %v, want no error", err)
		}
		if got, want := string(mustJSON(result)), `{"content":[],"isError":false}`; got != want {
			t.Errorf("result = %s, want %s", got, want)
		}
	})
}

func TestInvokeToolReportsFailures(t *testing.T) {
	t.Run("a nil tool is reported rather than dereferenced", func(t *testing.T) {
		// Library code never panics: the caller resolved nothing and gets an
		// error to map, which the protocol masks as a generic internal error.
		result, err := server.InvokeTool(context.Background(), nil, server.NewRequest(nil))
		if !errors.Is(err, server.ErrNoTool) {
			t.Fatalf("error = %v, want ErrNoTool", err)
		}
		if result != nil {
			t.Errorf("result = %v, want none", result)
		}
	})

	t.Run("a handler failure is returned unchanged for the caller to map", func(t *testing.T) {
		result, err := server.InvokeTool(context.Background(), failingTool(), server.NewRequest(nil))
		if err == nil {
			t.Fatal("InvokeTool returned no error, want the handler failure")
		}
		// The caller decides what reaches the client; the error itself keeps
		// the detail the server needs to diagnose the failure.
		if got, want := err.Error(), "connection to 10.0.0.5:5432 refused: secret-token"; got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
		if result != nil {
			t.Errorf("result = %v, want none", result)
		}
	})
}

func TestToolResultShapes(t *testing.T) {
	t.Run("a nil response is an empty, non-error result", func(t *testing.T) {
		result, err := server.ToolResult(nil)
		if err != nil {
			t.Fatalf("ToolResult returned %v, want no error", err)
		}
		if got, want := string(mustJSON(result)), `{"content":[],"isError":false}`; got != want {
			t.Errorf("result = %s, want %s", got, want)
		}
	})

	t.Run("structured content and meta are merged into the result", func(t *testing.T) {
		resp := server.Text("ok").
			WithStructuredContent(map[string]any{"count": 1}).
			WithMeta("trace", "t-1")
		result, err := server.ToolResult(resp)
		if err != nil {
			t.Fatalf("ToolResult returned %v, want no error", err)
		}
		want := `{"_meta":{"trace":"t-1"},"content":[{"text":"ok","type":"text"}],"isError":false,"structuredContent":{"count":1}}`
		if got := string(mustJSON(result)); got != want {
			t.Errorf("result = %s, want %s", got, want)
		}
	})

	t.Run("content with no tool shape is an error", func(t *testing.T) {
		result, err := server.ToolResult(server.NewResponse(content.NewBlob([]byte("raw"))))
		if err == nil {
			t.Fatal("ToolResult returned no error, want one for blob content")
		}
		if result != nil {
			t.Errorf("result = %v, want none", result)
		}
	})
}

func TestValidationMessageRendering(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "field messages are joined in field order",
			err: fmt.Errorf("%w: %w", server.ErrValidation, contract.ValidationErrors{Errors: map[string][]string{
				"name":  {"The name field is required."},
				"email": {"The email field must be a string.", "The email field is invalid."},
			}}),
			want: "The email field must be a string. The email field is invalid. The name field is required.",
		},
		{
			name: "an error carrying no field messages falls back",
			err:  errors.New("connection to 10.0.0.5:5432 refused: secret-token"),
			want: "The given data was invalid.",
		},
		{
			name: "an empty message set falls back rather than rendering nothing",
			err: fmt.Errorf("%w: %w", server.ErrValidation, contract.ValidationErrors{Errors: map[string][]string{
				"name": {},
			}}),
			want: "The given data was invalid.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := server.ValidationMessage(tc.err); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}
}
