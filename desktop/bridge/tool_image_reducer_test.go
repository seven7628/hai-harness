package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// TestReducerCarriesToolImages 工具图片必须进入 checkpoint（Block.Images）：
// 这是「恢复后 UI 仍能展示模型看过的图」的唯一数据源（事件日志里也带，但
// checkpoint 是渲染主路径）。
func TestReducerCarriesToolImages(t *testing.T) {
	r := NewViewReducer()
	now := time.Now()
	// 工具开始 → 结束（带图片）
	r.Apply(&events.ToolStart{
		Id: "t1", Name: "computer_screenshot", RunId: "run-1",
		Timestamp: now, EventType: events.ToolRunStartType,
	}, EventMeta{})
	r.Apply(&events.ToolResponse{
		Id: "t1", Name: "computer_screenshot", RunId: "run-1",
		Result: "shot 800x600", Images: []core.Content{
			{Type: core.ContentTypeImage, Content: "data:image/png;base64,SHOT", MimeType: "image/png"},
		},
		Timestamp: now.Add(time.Second), EventType: events.ToolRunEndType,
	}, EventMeta{})

	cp := r.Snapshot()
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Blocks []struct {
			Kind   string `json:"kind"`
			Images []struct {
				Type     string `json:"type"`
				Content  string `json:"content"`
				MimeType string `json:"mime_type"`
			} `json:"images"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range parsed.Blocks {
		if b.Kind != "tool" || len(b.Images) == 0 {
			continue
		}
		found = true
		if b.Images[0].Type != "image" || b.Images[0].Content != "data:image/png;base64,SHOT" || b.Images[0].MimeType != "image/png" {
			t.Fatalf("checkpoint image block wrong: %+v", b.Images[0])
		}
	}
	if !found {
		t.Fatalf("tool block must carry images in checkpoint: %s", raw)
	}
}

// TestReducerToolImagesNilForTextTools 纯文本工具不产生 images 字段（omitempty）。
func TestReducerToolImagesNilForTextTools(t *testing.T) {
	r := NewViewReducer()
	now := time.Now()
	r.Apply(&events.ToolResponse{
		Id: "t2", Name: "grep", Result: "found 3 matches",
		Timestamp: now, EventType: events.ToolRunEndType,
	}, EventMeta{})
	raw, _ := json.Marshal(r.Snapshot())
	if strings.Contains(string(raw), `"images"`) {
		t.Fatalf("text-only tool must not emit images field: %s", raw)
	}
}

// TestLegacyMigrationCarriesToolImages 冷迁移路径（无 record 的旧会话 → 重放
// events.jsonl 生成 checkpoint）必须带上工具图片：旧会话恢复后 UI 仍能看到模型看过的图。
// 这条路径与实时事件走同一 reducer，但序列化/反序列化多一跳（events.jsonl 文案 → typed event）。
func TestLegacyMigrationCarriesToolImages(t *testing.T) {
	line := `{"event_type":"tool_run_end","id":"t1","name":"computer_screenshot","run_id":"run-1",` +
		`"result":"shot","images":[{"type":"image","content":"data:image/png;base64,SHOT","mime_type":"image/png"}],` +
		`"timestamp":"2026-09-13T10:00:00Z"}`
	ev, err := parseLegacyEvent(line)
	if err != nil || ev == nil {
		t.Fatalf("parseLegacyEvent: %v (%v)", err, ev)
	}
	tr, ok := ev.(*events.ToolResponse)
	if !ok {
		t.Fatalf("want *events.ToolResponse, got %T", ev)
	}
	if len(tr.Images) != 1 || tr.Images[0].Content != "data:image/png;base64,SHOT" {
		t.Fatalf("images lost in legacy parse: %+v", tr.Images)
	}
	// 重放到 reducer → checkpoint 带图
	r := NewViewReducer()
	if err := r.Apply(ev, EventMeta{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r.Snapshot())
	if !strings.Contains(string(raw), "data:image/png;base64,SHOT") {
		t.Fatalf("checkpoint must carry migrated tool image: %s", raw)
	}
}
