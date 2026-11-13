package main

// P4-UI-12：任务卡产物预览（task_id + path 白名单寻址、根锚定、图片/文本读取）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// newPreviewTask 建一个已结算、带产物清单的任务。
func newPreviewTask(t *testing.T, m *manager, origin agenthub.TaskOrigin, target agenthub.TaskTarget, arts []agenthub.TaskArtifact) *agenthub.Task {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(ctx, &agenthub.Task{
		Origin: origin, Target: target, Title: "产物预览", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	task, err = ledger.Finish(ctx, agenthub.FinishInput{
		TaskID: task.ID, RunID: "run-preview", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Summary: "done", Artifacts: arts},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func artifactData(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	if data == nil {
		t.Fatal("data is nil")
	}
	return data
}

func TestHubTaskArtifactPreviewTextAndImage(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out", "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	md := "# 定价 v3\n\n| 档位 | 价格 |\n| --- | --- |\n| Pro | $20 |\n"
	if err := os.WriteFile(filepath.Join(ws, "out", "pricing.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	csv := "tier,price\nPro,20\nTeam,50\n"
	if err := os.WriteFile(filepath.Join(ws, "out", "pricing.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(filepath.Join(ws, "out", "assets", "cost.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	task := newPreviewTask(t, m,
		agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_preview_owner"},
		agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: ws, SessionID: "s1"},
		[]agenthub.TaskArtifact{
			{Name: "pricing.md", Path: "out/pricing.md", Kind: "created"},
			{Name: "pricing.csv", Path: "out/pricing.csv", Kind: "created"},
			{Name: "cost.png", Path: "out/assets/cost.png", Kind: "created"},
		})

	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "out/pricing.md",
	})
	if !ok {
		t.Fatalf("md preview: %s", errMsg)
	}
	data = artifactData(t, data)
	if data["content"] != md || data["truncated"] == true || data["binary"] == true || data["path"] != "out/pricing.md" {
		t.Fatalf("md = %+v", data)
	}
	if data["lines"].(float64) != 5 {
		t.Fatalf("lines = %v", data["lines"])
	}

	ok, errMsg, data = hubCmd(t, m, buf, 2, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "out/pricing.csv",
	})
	if !ok {
		t.Fatalf("csv preview: %s", errMsg)
	}
	data = artifactData(t, data)
	if !strings.Contains(data["content"].(string), "Team,50") {
		t.Fatalf("csv = %+v", data)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "out/assets/cost.png",
	})
	if !ok {
		t.Fatalf("image preview: %s", errMsg)
	}
	data = artifactData(t, data)
	if data["binary"] != true || data["mime"] != "image/png" || data["content"] != nil {
		t.Fatalf("image = %+v", data)
	}
	if !strings.HasPrefix(data["data_url"].(string), "data:image/png;base64,") {
		t.Fatalf("data_url = %v", data["data_url"])
	}
	// 不泄漏绝对工作区路径。
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), ws) {
		t.Fatalf("absolute workspace leaked: %s", raw)
	}
}

func TestHubTaskArtifactPreviewTruncates(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	body := strings.Repeat("a", maxPreviewBytes+2048)
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	task := newPreviewTask(t, m, agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_big_owner"},
		agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: ws},
		[]agenthub.TaskArtifact{{Name: "big.txt", Path: "big.txt"}})
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "big.txt",
	})
	if !ok {
		t.Fatalf("truncated preview: %s", errMsg)
	}
	data = artifactData(t, data)
	if data["truncated"] != true || len(data["content"].(string)) != maxPreviewBytes {
		t.Fatalf("truncated = %+v len=%d", data["truncated"], len(data["content"].(string)))
	}
}

func TestHubTaskArtifactPreviewRejectsUnlistedAndEscape(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "unlisted.md"), []byte("unlisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "secret.md"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.md"), []byte("escape"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	arts := []agenthub.TaskArtifact{
		{Name: "secret.md", Path: "secret.md"},
		{Name: "escape.md", Path: "../" + filepath.Base(outside) + "/escape.md"},
		{Name: "abs.md", Path: filepath.Join(outside, "escape.md")},
		{Name: "dir", Path: "out"},
		{Name: "missing.md", Path: "out/missing.md"},
	}
	task := newPreviewTask(t, m, agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_reject_owner"},
		agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: ws}, arts)

	cases := []struct{ name, path string }{
		{"unlisted", "unlisted.md"}, // 文件存在但不在清单：仍然拒绝
		{"dotdot", arts[1].Path},
		{"absolute", arts[2].Path},
		{"directory", "out"},
		{"missing", "out/missing.md"},
	}
	for i, tc := range cases {
		ok, _, _ := hubCmd(t, m, buf, i+1, "hub_task_artifact_preview", map[string]any{
			"task_id": string(task.ID), "path": tc.path,
		})
		if ok {
			t.Fatalf("%s must be rejected (path=%q)", tc.name, tc.path)
		}
	}

	// 清单里只登记 secret.md，精确命中后可以读。
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "secret.md",
	})
	if !ok || artifactData(t, data)["content"] != "secret" {
		t.Fatalf("listed file should read: %s %+v", errMsg, data)
	}

	// task 不存在。
	if ok, _, _ := hubCmd(t, m, buf, 11, "hub_task_artifact_preview", map[string]any{
		"task_id": "task_missing", "path": "secret.md",
	}); ok {
		t.Fatal("missing task must be rejected")
	}
}

func TestHubTaskArtifactPreviewAgentTargetHome(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "HomeOwner")
	home := hubAgentHome(string(p.ID))
	if err := os.MkdirAll(filepath.Join(home, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "out", "r.md"), []byte("# agent home"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := newPreviewTask(t, m,
		agenthub.TaskOrigin{Kind: agenthub.OriginGroup, AgentID: "ag_dispatcher"},
		agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: p.ID, GroupID: "grp_reg"},
		[]agenthub.TaskArtifact{{Name: "r.md", Path: "out/r.md", Kind: "created"}})

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_task_artifact_preview", map[string]any{
		"task_id": string(task.ID), "path": "out/r.md",
	})
	if !ok {
		t.Fatalf("agent-target preview: %s", errMsg)
	}
	if got := artifactData(t, data)["content"]; got != "# agent home" {
		t.Fatalf("content = %v", got)
	}
}

// 回归：agent-target 任务的相对产物路径以前按 bridge CWD 解析，os.Stat 必然失败。
func TestRegisterTaskArtifactsAnchorsAgentHome(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "RegHome")
	home := hubAgentHome(string(p.ID))
	if err := os.MkdirAll(filepath.Join(home, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "out", "r.md"), []byte("# registered"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	task := &agenthub.Task{
		ID:     "task_reg_agent",
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, AgentID: "ag_dispatcher"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: p.ID, GroupID: "grp_reg"},
	}
	if err := registerTaskArtifacts(context.Background(), st, task, []core.Artifact{
		{Name: "r.md", Path: "out/r.md", Kind: "created"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	list, err := st.ListArtifacts(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("artifacts = %+v, %v", list, err)
	}
	got := list[0]
	if got.OwnerAgentID != p.ID {
		t.Fatalf("owner = %s, want %s", got.OwnerAgentID, p.ID)
	}
	if b, err := os.ReadFile(got.SourcePath); err != nil || string(b) != "# registered" {
		t.Fatalf("snapshot = %q, %v", b, err)
	}
}
