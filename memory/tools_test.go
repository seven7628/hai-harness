package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

func TestMemoryToolsValidateAndCall(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapIndex(); err != nil {
		t.Fatal(err)
	}
	view := NewViewTool(s)
	write := NewWriteTool(s)
	ctx := events.WithToolContextForRun(context.Background(), "main-run", "", 0, nil, nil, nil)
	if err := view.ValidParams(ctx, "", `{"ref":"index"}`); err != nil {
		t.Fatal(err)
	}
	if err := view.ValidParams(ctx, "", `{}`); err == nil {
		t.Fatal("view should require ref")
	}
	if err := write.ValidParams(ctx, "", `{"name":"notes","body":"body"}`); err != nil {
		t.Fatal(err)
	}
	if err := write.ValidParams(ctx, "", `{"name":"notes"}`); err == nil {
		t.Fatal("write should require body")
	}
	if _, err := write.Call(ctx, "", `{"name":"notes","title":"Notes","summary":"Use for notes","body":"body","tags":["x"]}`); err != nil {
		t.Fatal(err)
	}
	out, err := view.Call(ctx, "", `{"ref":"notes"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id: notes") || !strings.Contains(out, "Use for notes") {
		t.Fatalf("view did not return topic: %q", out)
	}
}

func TestMemoryToolsRejectSubagent(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := events.WithToolContextForRun(context.Background(), "child-run", "parent-run", 1, nil, nil, nil)
	if _, err := NewViewTool(s).Call(ctx, "", `{"ref":"index"}`); err == nil {
		t.Fatal("subagent memory_view should be rejected")
	}
	if _, err := NewWriteTool(s).Call(ctx, "", `{"name":"x","body":"y"}`); err == nil {
		t.Fatal("subagent memory_write should be rejected")
	}
}

func TestTopicFrontmatterRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	topic := &Topic{Name: "build", Title: "Build", Summary: "When build fails", Kind: "experience", Scope: ScopeRepo, Tags: []string{"go"}, Created: now, Updated: now, Body: "Body"}
	raw := renderTopic(topic)
	parsed := parseTopic("build", "topics/build.md", raw)
	if parsed.Name != "build" || parsed.Title != "Build" || parsed.Summary != "When build fails" || parsed.Kind != "experience" || parsed.Body != "Body\n" {
		t.Fatalf("frontmatter round trip mismatch: %+v\n%s", parsed, raw)
	}
}
