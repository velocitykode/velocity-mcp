package content

import "testing"

func TestSetPriority(t *testing.T) {
	c := NewText("x")
	for _, p := range []float64{0.0, 0.5, 1.0} {
		if err := c.SetPriority(p); err != nil {
			t.Fatalf("SetPriority(%v) = %v, want nil", p, err)
		}
	}
	for _, p := range []float64{-0.1, 1.1, 2} {
		if err := c.SetPriority(p); err == nil {
			t.Fatalf("SetPriority(%v) = nil, want range error", p)
		}
	}
}

func TestSetLastModified(t *testing.T) {
	c := NewText("x")
	for _, ts := range []string{"2026-06-13T08:15:24Z", "2026-06-13T08:15:24+05:00", "2026-06-13"} {
		if err := c.SetLastModified(ts); err != nil {
			t.Fatalf("SetLastModified(%q) = %v, want nil", ts, err)
		}
	}
	for _, ts := range []string{"not-a-date", "13/06/2026", "2026-13-40"} {
		if err := c.SetLastModified(ts); err == nil {
			t.Fatalf("SetLastModified(%q) = nil, want parse error", ts)
		}
	}
	// Empty string clears.
	if err := c.SetLastModified(""); err != nil {
		t.Fatalf("clear = %v", err)
	}
	if c.lastModified != "" {
		t.Fatalf("lastModified = %q, want cleared", c.lastModified)
	}
}

func TestSetAudience(t *testing.T) {
	c := NewText("x")
	if err := c.SetAudience(RoleUser, RoleAssistant); err != nil {
		t.Fatalf("SetAudience = %v", err)
	}
	if err := c.SetAudience(RoleUser, "robot"); err == nil {
		t.Fatal("invalid role accepted")
	}
	// Duplicates collapse, order preserved.
	if err := c.SetAudience(RoleAssistant, RoleAssistant, RoleUser); err != nil {
		t.Fatalf("SetAudience dedupe = %v", err)
	}
	if len(c.audience) != 2 || c.audience[0] != RoleAssistant || c.audience[1] != RoleUser {
		t.Fatalf("audience = %v, want [assistant user]", c.audience)
	}
	// No roles clears.
	if err := c.SetAudience(); err != nil {
		t.Fatalf("clear = %v", err)
	}
	if c.audience != nil {
		t.Fatalf("audience = %v, want nil", c.audience)
	}
}

func TestAnnotationsUnsetOmitted(t *testing.T) {
	// A content value with no annotations carries no "annotations" key.
	got := NewText("x").toArray()
	if _, ok := got["annotations"]; ok {
		t.Fatalf("unset annotations leaked into wire shape: %v", got)
	}
}

func TestAnnotationsInWireShape(t *testing.T) {
	c := NewText("hello")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.SetAudience(RoleUser))
	must(c.SetPriority(0.8))
	must(c.SetLastModified("2026-06-13T08:15:24Z"))

	check := func(m map[string]any) {
		ann, ok := m["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("missing annotations object: %v", m)
		}
		aud, ok := ann["audience"].([]string)
		if !ok || len(aud) != 1 || aud[0] != "user" {
			t.Fatalf("audience = %v", ann["audience"])
		}
		if ann["priority"] != 0.8 {
			t.Fatalf("priority = %v", ann["priority"])
		}
		if ann["lastModified"] != "2026-06-13T08:15:24Z" {
			t.Fatalf("lastModified = %v", ann["lastModified"])
		}
	}

	tool, err := c.ToTool()
	if err != nil {
		t.Fatal(err)
	}
	check(tool)

	res, err := c.ToResource("file:///x", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	check(res)
}

func TestAnnotationsAndMetaCoexist(t *testing.T) {
	c := NewText("x")
	c.SetMeta("trace", "abc")
	if err := c.SetPriority(0.5); err != nil {
		t.Fatal(err)
	}
	got := c.toArray()
	if _, ok := got["annotations"]; !ok {
		t.Fatalf("annotations missing: %v", got)
	}
	if _, ok := got["_meta"]; !ok {
		t.Fatalf("_meta missing: %v", got)
	}
}
