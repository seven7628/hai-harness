package main

// P3a-07：产物登记/按 id 预览/可见性与 2MB 上限。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func TestHubArtifactPreviewById(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ArtA")
	b := createHubAgent(t, m, buf, 2, "ArtB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "产物群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("group: %s", errMsg)
	}
	g := groupFromData(t, data)
	dir := t.TempDir()
	src := filepath.Join(dir, "report.md")
	if err := os.WriteFile(src, []byte("hello artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	art := &agenthub.Artifact{
		OwnerAgentID: a.ID, GroupID: g.ID, Name: "report.md", Path: "report.md",
		SourcePath: src, Visibility: agenthub.VisibilityGroup,
	}
	if err := agenthub.RegisterArtifact(context.Background(), st, art); err != nil {
		t.Fatal(err)
	}
	bsB, err := m.hubDMSession(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bsB.hubEngine.GetTool(context.Background(), "artifact_read")
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(context.Background(), "artifact_read", `{"artifact_id":"`+string(art.ID)+`"}`)
	if err != nil {
		t.Fatalf("artifact_read: %v", err)
	}
	var got struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Version   int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Content != "hello artifact" || got.Truncated || got.Version != 1 {
		t.Fatalf("preview = %s, %v", out, err)
	}
	// 未登记/任意路径 id 一律拒绝。
	if _, err := tool.Call(context.Background(), "artifact_read", `{"artifact_id":"/etc/passwd"}`); err == nil {
		t.Fatal("path id must be rejected")
	}
	if _, err := tool.Call(context.Background(), "artifact_read", `{"artifact_id":"art_missing"}`); err == nil {
		t.Fatal("missing artifact must be rejected")
	}
	// 人类命令按 id 读取。
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_artifact_get", map[string]any{"artifact_id": string(art.ID)})
	if !ok {
		t.Fatalf("hub_artifact_get: %s", errMsg)
	}
	raw, _ := json.Marshal(data)
	if !strings.Contains(string(raw), "hello artifact") {
		t.Fatalf("human preview = %s", raw)
	}
}

func TestHubArtifactOwnerOnlyAndCap(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "OwnA")
	b := createHubAgent(t, m, buf, 2, "OwnB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "私有产物群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("group: %s", errMsg)
	}
	g := groupFromData(t, data)
	dir := t.TempDir()
	src := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(src, []byte("owner only"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	art := &agenthub.Artifact{
		OwnerAgentID: a.ID, GroupID: g.ID, Name: "secret.md", Path: "secret.md",
		SourcePath: src, Visibility: agenthub.VisibilityOwner,
	}
	if err := agenthub.RegisterArtifact(context.Background(), st, art); err != nil {
		t.Fatal(err)
	}
	bsB, _ := m.hubDMSession(string(b.ID))
	tool, _ := bsB.hubEngine.GetTool(context.Background(), "artifact_read")
	if _, err := tool.Call(context.Background(), "artifact_read", `{"artifact_id":"`+string(art.ID)+`"}`); err == nil {
		t.Fatal("owner-only artifact leaked to other agent")
	}
	// 人类可读 owner-only。
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_artifact_get", map[string]any{"artifact_id": string(art.ID)})
	if !ok || !strings.Contains(string(mustJSON(data)), "owner only") {
		t.Fatalf("human owner-only preview: %s", errMsg)
	}
	// 2MB 上限：请求 10MB 也只回 2MB，并标记 truncated。
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", artifactPreviewMax+1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	bigArt := &agenthub.Artifact{
		OwnerAgentID: a.ID, GroupID: g.ID, Name: "big.txt", Path: "big.txt",
		SourcePath: big, Visibility: agenthub.VisibilityGroup,
	}
	if err := agenthub.RegisterArtifact(context.Background(), st, bigArt); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_artifact_get", map[string]any{
		"artifact_id": string(bigArt.ID), "limit": 10 << 20,
	})
	if !ok {
		t.Fatalf("big preview: %s", errMsg)
	}
	var got struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	raw := mustJSON(data)
	_ = json.Unmarshal(raw, &got)
	if len(got.Content) != artifactPreviewMax || !got.Truncated {
		t.Fatalf("cap = %d truncated=%v", len(got.Content), got.Truncated)
	}
}

func TestHubTaskFinishRegistersArtifacts(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RegA")
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "out", "r.md"), []byte("# r"), 0o600); err != nil {
		t.Fatal(err)
	}
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: a.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetAgent, AgentID: a.ID, Workspace: workspace, SessionID: "s1"},
		Title:  "登记产物", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bs, &events.AgentEnd{
		RunId: "run-art", Content: "done", FinishReason: core.FinishReasonStop,
		Artifacts: []core.Artifact{{Name: "r.md", Path: "out/r.md", Kind: "created", Size: 3}},
	})
	list, err := st.ListArtifacts(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("artifacts = %+v, %v", list, err)
	}
	got := list[0]
	if got.OwnerAgentID != a.ID || got.Version != 1 || !strings.HasSuffix(got.SourcePath, filepath.Join("out", "r.md")) {
		t.Fatalf("artifact = %+v", got)
	}
}
