package content

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// marshalToMap round-trips a Content through MarshalJSON into a generic map so
// tests can assert on the exact wire keys/values regardless of map ordering.
func marshalToMap(t *testing.T, c Content) map[string]any {
	t.Helper()
	b, err := c.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return m
}

func TestText_WireShapes(t *testing.T) {
	tests := []struct {
		name string
		got  func() (map[string]any, error)
		want map[string]any
	}{
		{
			name: "default",
			got:  func() (map[string]any, error) { return NewText("hi").ToTool() },
			want: map[string]any{"type": "text", "text": "hi"},
		},
		{
			name: "prompt",
			got:  func() (map[string]any, error) { return NewText("hi").ToPrompt() },
			want: map[string]any{"type": "text", "text": "hi"},
		},
		{
			name: "resource",
			got:  func() (map[string]any, error) { return NewText("hi").ToResource("file://x", "text/plain") },
			want: map[string]any{"text": "hi", "uri": "file://x", "mimeType": "text/plain"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.got()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestText_MarshalJSON(t *testing.T) {
	m := marshalToMap(t, NewText("hello"))
	if m["type"] != "text" || m["text"] != "hello" {
		t.Errorf("unexpected marshal: %#v", m)
	}
}

func TestText_String(t *testing.T) {
	if got := NewText("abc").String(); got != "abc" {
		t.Errorf("String() = %q, want abc", got)
	}
}

func TestImage_WireShapes(t *testing.T) {
	raw := []byte{0x01, 0x02, 0x03}
	enc := base64.StdEncoding.EncodeToString(raw)

	t.Run("default mime", func(t *testing.T) {
		got, _ := NewImage(raw, "").ToTool()
		want := map[string]any{"type": "image", "data": enc, "mimeType": DefaultImageMimeType}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("custom mime", func(t *testing.T) {
		got, _ := NewImage(raw, "image/jpeg").ToPrompt()
		want := map[string]any{"type": "image", "data": enc, "mimeType": "image/jpeg"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("resource uses blob and own mime", func(t *testing.T) {
		got, _ := NewImage(raw, "image/jpeg").ToResource("file://i", "ignored/mime")
		want := map[string]any{"blob": enc, "uri": "file://i", "mimeType": "image/jpeg"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("marshal", func(t *testing.T) {
		m := marshalToMap(t, NewImage(raw, ""))
		if m["data"] != enc {
			t.Errorf("data = %v, want %v", m["data"], enc)
		}
	})

	t.Run("string is raw bytes", func(t *testing.T) {
		if got := NewImage(raw, "").String(); got != string(raw) {
			t.Errorf("String() = %q, want raw", got)
		}
	})
}

func TestAudio_WireShapes(t *testing.T) {
	raw := []byte("sound")
	enc := base64.StdEncoding.EncodeToString(raw)

	t.Run("default mime", func(t *testing.T) {
		got, _ := NewAudio(raw, "").ToTool()
		want := map[string]any{"type": "audio", "data": enc, "mimeType": DefaultAudioMimeType}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("custom mime prompt", func(t *testing.T) {
		got, _ := NewAudio(raw, "audio/mpeg").ToPrompt()
		want := map[string]any{"type": "audio", "data": enc, "mimeType": "audio/mpeg"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("resource uses blob and own mime", func(t *testing.T) {
		got, _ := NewAudio(raw, "audio/mpeg").ToResource("file://a", "ignored")
		want := map[string]any{"blob": enc, "uri": "file://a", "mimeType": "audio/mpeg"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("string is raw bytes", func(t *testing.T) {
		if got := NewAudio(raw, "").String(); got != "sound" {
			t.Errorf("String() = %q, want sound", got)
		}
	})

	t.Run("marshal", func(t *testing.T) {
		m := marshalToMap(t, NewAudio(raw, ""))
		if m["type"] != "audio" || m["data"] != enc {
			t.Errorf("unexpected marshal: %#v", m)
		}
	})
}

func TestBlob_WireShapes(t *testing.T) {
	raw := []byte("payload")
	enc := base64.StdEncoding.EncodeToString(raw)

	t.Run("default shape carries raw content", func(t *testing.T) {
		// MarshalJSON / toArray carries content verbatim (no base64).
		m := marshalToMap(t, NewBlob(raw))
		want := map[string]any{"type": "blob", "blob": "payload"}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("got %#v, want %#v", m, want)
		}
	})

	t.Run("resource base64-encodes", func(t *testing.T) {
		got, err := NewBlob(raw).ToResource("file://b", "application/octet-stream")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]any{"blob": enc, "uri": "file://b", "mimeType": "application/octet-stream"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("string is raw bytes", func(t *testing.T) {
		if got := NewBlob(raw).String(); got != "payload" {
			t.Errorf("String() = %q, want payload", got)
		}
	})
}

func TestBlob_NotAllowedContexts(t *testing.T) {
	b := NewBlob([]byte("x"))
	for _, tc := range []struct {
		name string
		fn   func() (map[string]any, error)
	}{
		{"tool", b.ToTool},
		{"prompt", b.ToPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if !errors.Is(err, ErrNotAllowed) {
				t.Errorf("err = %v, want ErrNotAllowed", err)
			}
			if got != nil {
				t.Errorf("got = %#v, want nil", got)
			}
		})
	}
}

func TestNotification_WireShape(t *testing.T) {
	t.Run("with params", func(t *testing.T) {
		n := NewNotification("notifications/message", map[string]any{"level": "info"})
		got, _ := n.ToTool()
		want := map[string]any{
			"method": "notifications/message",
			"params": map[string]any{"level": "info"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("nil params becomes empty object", func(t *testing.T) {
		n := NewNotification("ping", nil)
		got, _ := n.ToPrompt()
		params, ok := got["params"].(map[string]any)
		if !ok || len(params) != 0 {
			t.Errorf("params = %#v, want empty map", got["params"])
		}
	})

	t.Run("resource ignores uri/mime", func(t *testing.T) {
		n := NewNotification("m", map[string]any{"a": 1})
		got, _ := n.ToResource("file://x", "text/plain")
		if _, ok := got["uri"]; ok {
			t.Errorf("resource shape should not carry uri: %#v", got)
		}
		if got["method"] != "m" {
			t.Errorf("method = %v, want m", got["method"])
		}
	})

	t.Run("string is method", func(t *testing.T) {
		if got := NewNotification("foo", nil).String(); got != "foo" {
			t.Errorf("String() = %q, want foo", got)
		}
	})

	t.Run("does not mutate caller params", func(t *testing.T) {
		params := map[string]any{"k": "v"}
		n := NewNotification("m", params)
		n.SetMeta("trace", "id")
		_, _ = n.ToTool()
		if _, leaked := params["_meta"]; leaked {
			t.Error("ToTool mutated the caller's params map")
		}
	})
}

func TestNotification_MetaFoldedIntoParams(t *testing.T) {
	n := NewNotification("m", map[string]any{"a": 1})
	n.SetMeta("trace", "abc")
	got, _ := n.ToTool()

	params := got["params"].(map[string]any)
	metaMap, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params._meta missing: %#v", params)
	}
	if metaMap["trace"] != "abc" {
		t.Errorf("_meta.trace = %v, want abc", metaMap["trace"])
	}
	// Must NOT appear at top level for notifications.
	if _, top := got["_meta"]; top {
		t.Error("_meta must not appear at top level for notifications")
	}
}

func TestNotification_ExplicitMetaParamWins(t *testing.T) {
	// When params already supplies _meta, SetMeta must not overwrite it.
	explicit := map[string]any{"src": "params"}
	n := NewNotification("m", map[string]any{"_meta": explicit})
	n.SetMeta("trace", "abc")
	got, _ := n.ToTool()
	params := got["params"].(map[string]any)
	if !reflect.DeepEqual(params["_meta"], explicit) {
		t.Errorf("_meta = %#v, want explicit params value", params["_meta"])
	}
}

func TestResourceLink_WireShape(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		got, _ := NewResourceLink("file://r", "report").ToTool()
		want := map[string]any{"type": "resource_link", "uri": "file://r", "name": "report"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("full", func(t *testing.T) {
		link := NewResourceLink("file://r", "report").
			WithTitle("Q1 Report").
			WithDescription("desc").
			WithMimeType("application/pdf").
			WithSize(2048).
			WithAnnotations(map[string]any{"audience": "user"}).
			WithIcons(Icon{Src: "icon.png", MimeType: "image/png", Sizes: []string{"48x48"}, Theme: "dark"})

		got, _ := link.ToPrompt()
		want := map[string]any{
			"type":        "resource_link",
			"uri":         "file://r",
			"name":        "report",
			"title":       "Q1 Report",
			"description": "desc",
			"mimeType":    "application/pdf",
			"size":        2048,
			"annotations": map[string]any{"audience": "user"},
			"icons": []map[string]any{
				{"src": "icon.png", "mimeType": "image/png", "sizes": []string{"48x48"}, "theme": "dark"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("size zero is emitted (set explicitly)", func(t *testing.T) {
		got, _ := NewResourceLink("u", "n").WithSize(0).ToTool()
		if v, ok := got["size"]; !ok || v != 0 {
			t.Errorf("size = %v (ok=%v), want explicit 0", v, ok)
		}
	})

	t.Run("string is uri", func(t *testing.T) {
		if got := NewResourceLink("file://r", "n").String(); got != "file://r" {
			t.Errorf("String() = %q, want file://r", got)
		}
	})

	t.Run("marshal", func(t *testing.T) {
		m := marshalToMap(t, NewResourceLink("u", "n"))
		if m["type"] != "resource_link" {
			t.Errorf("unexpected marshal: %#v", m)
		}
	})
}

func TestResourceLink_NotAllowedInResource(t *testing.T) {
	got, err := NewResourceLink("u", "n").ToResource("u", "m")
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("err = %v, want ErrNotAllowed", err)
	}
	if got != nil {
		t.Errorf("got = %#v, want nil", got)
	}
}

func TestIcon_ToArray(t *testing.T) {
	tests := []struct {
		name string
		icon Icon
		want map[string]any
	}{
		{
			name: "src only",
			icon: Icon{Src: "a.png"},
			want: map[string]any{"src": "a.png"},
		},
		{
			name: "all fields",
			icon: Icon{Src: "a.png", MimeType: "image/png", Sizes: []string{"16x16", "32x32"}, Theme: "light"},
			want: map[string]any{
				"src":      "a.png",
				"mimeType": "image/png",
				"sizes":    []string{"16x16", "32x32"},
				"theme":    "light",
			},
		},
		{
			name: "empty sizes dropped",
			icon: Icon{Src: "a.png", Sizes: []string{}},
			want: map[string]any{"src": "a.png"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.icon.toArray(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMeta_MergedIntoWireShape(t *testing.T) {
	// Text/Image/Audio/Blob/ResourceLink put _meta at the top level.
	t.Run("set single key", func(t *testing.T) {
		txt := NewText("x")
		txt.SetMeta("k", "v")
		got, _ := txt.ToTool()
		metaMap, ok := got["_meta"].(map[string]any)
		if !ok || metaMap["k"] != "v" {
			t.Errorf("_meta = %#v", got["_meta"])
		}
	})

	t.Run("merge map", func(t *testing.T) {
		img := NewImage([]byte("d"), "")
		img.MergeMeta(map[string]any{"a": 1, "b": 2})
		got, _ := img.ToTool()
		metaMap := got["_meta"].(map[string]any)
		if metaMap["a"] != 1 || metaMap["b"] != 2 {
			t.Errorf("_meta = %#v", metaMap)
		}
	})

	t.Run("no meta means no key", func(t *testing.T) {
		got, _ := NewText("x").ToTool()
		if _, ok := got["_meta"]; ok {
			t.Error("_meta present without metadata set")
		}
	})

	t.Run("empty merge is noop", func(t *testing.T) {
		txt := NewText("x")
		txt.MergeMeta(nil)
		txt.MergeMeta(map[string]any{})
		got, _ := txt.ToTool()
		if _, ok := got["_meta"]; ok {
			t.Error("_meta present after empty merge")
		}
	})

	t.Run("meta appears in resource shape too", func(t *testing.T) {
		txt := NewText("x")
		txt.SetMeta("k", "v")
		got, _ := txt.ToResource("u", "m")
		if _, ok := got["_meta"]; !ok {
			t.Error("_meta missing from resource shape")
		}
	})
}

func TestMeta_MarshalIsolation(t *testing.T) {
	// Mutating the marshaled _meta map must not affect subsequent marshals.
	txt := NewText("x")
	txt.SetMeta("k", "v")
	got1, _ := txt.ToTool()
	m1 := got1["_meta"].(map[string]any)
	m1["k"] = "tampered"

	got2, _ := txt.ToTool()
	m2 := got2["_meta"].(map[string]any)
	if m2["k"] != "v" {
		t.Errorf("internal meta was mutated through returned map: %v", m2["k"])
	}
}

func TestMeta_Concurrent(t *testing.T) {
	// Independent content values used across goroutines must not race. Each
	// goroutine owns its own value; this guards the marshal/merge read path.
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c := NewText("concurrent")
			c.SetMeta("id", "x")
			if _, err := c.MarshalJSON(); err != nil {
				t.Errorf("MarshalJSON: %v", err)
			}
			if _, err := c.ToResource("u", "m"); err != nil {
				t.Errorf("ToResource: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestContent_InterfaceSatisfied(t *testing.T) {
	// Exercise each type through the Content interface to confirm uniform use.
	contents := []Content{
		NewText("t"),
		NewImage([]byte("i"), ""),
		NewAudio([]byte("a"), ""),
		NewBlob([]byte("b")),
		NewResourceLink("u", "n"),
		NewNotification("m", nil),
	}
	for _, c := range contents {
		if c.String() == "" && reflect.TypeOf(c) != reflect.TypeOf(NewText("")) {
			// String can legitimately be empty only for empty inputs; just
			// ensure the call does not panic and MarshalJSON works.
		}
		if _, err := c.MarshalJSON(); err != nil {
			t.Errorf("%T MarshalJSON: %v", c, err)
		}
	}
}
