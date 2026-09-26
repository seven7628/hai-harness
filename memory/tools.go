package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

// ViewTool 让主 Agent 按索引描述读取一个 Memory Topic。
// 第一版只读；工具错误直接回传模型，保留恢复机会。
type ViewTool struct{ service *MemoryService }

func NewViewTool(service *MemoryService) *ViewTool { return &ViewTool{service: service} }

func (t *ViewTool) Name() string { return "memory_view" }
func (t *ViewTool) Description() string {
	return "Read one repository memory topic when its title or description in MEMORY.md matches the current task. Returns the full Markdown topic."
}
func (t *ViewTool) Parameters() any {
	return tools.Obj(map[string]any{"ref": tools.Str("Memory topic name, for example build-recovery, or index")}, "ref")
}
func (t *ViewTool) CanParallel() bool { return true }
func (t *ViewTool) ValidParams(ctx context.Context, _, arguments string) error {
	var a struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("memory_view: %w", err)
	}
	if strings.TrimSpace(a.Ref) == "" {
		return errors.New("memory_view: ref is required")
	}
	return nil
}
func (t *ViewTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t *ViewTool) Call(ctx context.Context, _, arguments string) (string, error) {
	if err := rejectSubagent(ctx); err != nil {
		return "", err
	}
	var a struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", fmt.Errorf("memory_view: %w", err)
	}
	return t.service.View(a.Ref)
}
func (t *ViewTool) AfterCall(context.Context, core.ToolCall) {}

// WriteTool 让主 Agent 写入一个经过基础边界检查的 Topic。
// 第一版不提供删除、晋级、淘汰或回滚工具；同名 Topic 由 MemoryService 原子覆盖。
type WriteTool struct{ service *MemoryService }

func NewWriteTool(service *MemoryService) *WriteTool { return &WriteTool{service: service} }

func (t *WriteTool) Name() string { return "memory_write" }
func (t *WriteTool) Description() string {
	return "Write one reusable repository memory topic. Use only for non-obvious project knowledge that is not derivable from code, README, CI, or AGENTS.md. The write is validated and atomically stored."
}
func (t *WriteTool) Parameters() any {
	return tools.Obj(map[string]any{
		"name":    tools.Str("Short topic name in lowercase kebab-case"),
		"title":   tools.Str("Short human-readable title"),
		"summary": tools.Str("When should an agent read this topic?"),
		"body":    tools.Str("Full Markdown topic content"),
		"tags":    tools.ArrayStr("Optional short tags"),
	}, "name", "title", "summary", "body")
}
func (t *WriteTool) CanParallel() bool { return false }
func (t *WriteTool) ValidParams(ctx context.Context, _, arguments string) error {
	var a struct {
		Name    string   `json:"name"`
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Body    string   `json:"body"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("memory_write: %w", err)
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Body) == "" {
		return errors.New("memory_write: name and body are required")
	}
	return nil
}
func (t *WriteTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t *WriteTool) Call(ctx context.Context, _, arguments string) (string, error) {
	if err := rejectSubagent(ctx); err != nil {
		return "", err
	}
	var a struct {
		Name    string   `json:"name"`
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Body    string   `json:"body"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", fmt.Errorf("memory_write: %w", err)
	}
	if err := t.service.WriteTopic(&Topic{Name: a.Name, Title: a.Title, Summary: a.Summary, Body: a.Body, Tags: a.Tags}); err != nil {
		return "", err
	}
	return fmt.Sprintf("memory topic %q written and indexed", a.Name), nil
}
func (t *WriteTool) AfterCall(context.Context, core.ToolCall) {}

func rejectSubagent(ctx context.Context) error {
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.Depth > 0 {
		return errors.New("memory tools are available to the main agent only; subagents must use explicit memory refs")
	}
	return nil
}
