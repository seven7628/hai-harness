package agenthub

// P1-03：Profile 版本留档与删除的存储层语义。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	return st
}

func testProfile(id AgentID, version int, role string) *AgentProfile {
	return &AgentProfile{ID: id, Name: "p" + string(id), Provider: "deepseek", Model: "m", Role: role, Version: version}
}

// 快照必须是值拷贝：调用方随后 version++ / 改 persona，不能把历史一起改掉。
func TestAppendProfileVersionSnapshotsValue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := testProfile("ag_v1", 3, "旧职责")
	if err := st.AppendProfileVersion(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Version = 4
	p.Role = "新职责"

	vs, err := st.ListProfileVersions(ctx, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("versions = %d, want 1", len(vs))
	}
	if vs[0].Version != 3 || vs[0].Profile.Role != "旧职责" {
		t.Fatalf("snapshot mutated by caller: %+v", vs[0])
	}
	if vs[0].Profile == p {
		t.Fatal("snapshot aliases the caller's profile")
	}
	if vs[0].SavedAt.IsZero() {
		t.Fatal("SavedAt is zero")
	}
}

// 只保留最近 MaxProfileVersions 版（丢最旧）。
func TestAppendProfileVersionTrims(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 1; i <= MaxProfileVersions+5; i++ {
		if err := st.AppendProfileVersion(ctx, testProfile("ag_trim", i, "v")); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := st.ListProfileVersions(ctx, "ag_trim", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != MaxProfileVersions {
		t.Fatalf("versions = %d, want %d", len(vs), MaxProfileVersions)
	}
	if vs[0].Version != 6 || vs[len(vs)-1].Version != MaxProfileVersions+5 {
		t.Fatalf("trim window wrong: first=%d last=%d", vs[0].Version, vs[len(vs)-1].Version)
	}
	// limit 取最近 N 条。
	tail, err := st.ListProfileVersions(ctx, "ag_trim", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[1].Version != MaxProfileVersions+5 {
		t.Fatalf("limit window wrong: %+v", tail)
	}
}

// 没有历史 = 空表（不是错误）；坏行跳过（归档文件不能把主流程带死）。
func TestListProfileVersionsMissingAndCorrupt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	vs, err := st.ListProfileVersions(ctx, "ag_none", 0)
	if err != nil || len(vs) != 0 {
		t.Fatalf("missing versions = %v, %v", vs, err)
	}
	if err := st.AppendProfileVersion(ctx, testProfile("ag_corrupt", 1, "ok")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root(), "agents", "ag_corrupt", "profile_versions.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	vs, err = st.ListProfileVersions(ctx, "ag_corrupt", 0)
	if err != nil || len(vs) != 1 {
		t.Fatalf("corrupt line not skipped: %v, %v", vs, err)
	}
	if vs[0].SavedAt.IsZero() || time.Since(vs[0].SavedAt) > time.Hour {
		t.Fatalf("SavedAt looks wrong: %v", vs[0].SavedAt)
	}
}

// DeleteAgent 物理删目录且幂等；非法 id 直接拒绝（不能拿 ".." 删到别处）。
func TestDeleteAgentPurgesDirectory(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := testProfile("ag_del", 1, "x")
	if err := st.SaveAgent(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendProfileVersion(ctx, p); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(st.Root(), "agents", "ag_del")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir should exist before delete: %v", err)
	}
	if err := st.DeleteAgent(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists after delete: %v", err)
	}
	if err := st.DeleteAgent(ctx, p.ID); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if err := st.DeleteAgent(ctx, "../etc"); err == nil {
		t.Fatal("invalid id must be rejected")
	}
}

// CloneAgentProfile 必须隔离切片：调用方随后改自己的切片，快照不能跟着变
//（P1-03 复核：浅拷贝让「回滚」的旧版本被 patch 污染）。
func TestCloneAgentProfileIsolatesSlices(t *testing.T) {
	src := &AgentProfile{
		ID: "ag_clone", Name: "Clone", Provider: "deepseek", Model: "m",
		Tools: ToolPolicy{
			Disable: []string{"a", "b"}, Allow: []string{"c"}, BashRoots: []string{"~/x"},
			MCP: MCPPolicy{DisableServers: []string{"s1"}, DisableTools: []string{"t1"}, AllowTools: []string{"t2"}},
		},
		Policy: RunPolicy{WorkspaceRoots: []string{"~/ws"}},
	}
	cp := CloneAgentProfile(src)
	if cp == nil {
		t.Fatal("nil clone")
	}
	// 改调用方的每个切片元素：快照必须还是旧值。
	src.Tools.Disable[0] = "z"
	src.Tools.Allow[0] = "z"
	src.Tools.BashRoots[0] = "z"
	src.Tools.MCP.DisableServers[0] = "z"
	src.Tools.MCP.DisableTools[0] = "z"
	src.Tools.MCP.AllowTools[0] = "z"
	src.Policy.WorkspaceRoots[0] = "z"
	for name, got := range map[string]string{
		"disable":        cp.Tools.Disable[0],
		"allow":          cp.Tools.Allow[0],
		"bash_roots":     cp.Tools.BashRoots[0],
		"mcp_servers":    cp.Tools.MCP.DisableServers[0],
		"mcp_tools":      cp.Tools.MCP.DisableTools[0],
		"mcp_allow":      cp.Tools.MCP.AllowTools[0],
		"workspace_root": cp.Policy.WorkspaceRoots[0],
	} {
		if got == "z" {
			t.Errorf("clone shares slice with caller: %s was mutated", name)
		}
	}
	// nil 语义保持（nil ≠ 空切片，下游会区分「没设置」与「显式清空」）。
	empty := CloneAgentProfile(&AgentProfile{ID: "ag_nil", Name: "N", Provider: "deepseek", Model: "m"})
	if empty.Tools.Disable != nil {
		t.Errorf("nil slice became non-nil: %#v", empty.Tools.Disable)
	}
}
