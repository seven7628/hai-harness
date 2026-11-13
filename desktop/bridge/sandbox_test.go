package main

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/sandbox"
)

// waitEvent 轮询 stdout 直到出现指定 event_type 的事件（返回其行数据）。
func waitEvent(t *testing.T, buf *syncBuf, typ string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range readLines(buf) {
			if l["event_type"] == typ {
				return l
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting " + typ + " 事件")
	return nil
}

// TestSandboxGateAutoUsesToolApprovalChannel auto 模式（会话默认）：沙箱拦截 →
// 工具层审批通道（tool_approval_requested + approve 命令），不再发 sandbox_blocked。
// 平台无关（不依赖 sandbox-exec）：直接测放行门的分派与 IPC。
func TestSandboxGateAutoUsesToolApprovalChannel(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatalf("no session for %s", sid)
	}
	if modeName(bs.mode) != "auto" {
		t.Fatalf("默认会话模式应为 auto, got %s", modeName(bs.mode))
	}
	g := &sandboxReleaseGate{m: m, ws: ws, id: sid}

	// 批准路径：Release → tool_approval_requested 事件 → approve=true → 放行
	done := make(chan bool, 1)
	go func() {
		done <- g.Release(context.Background(), sandbox.ExecSpec{Command: "cat /secret"}, "deny file-read*: /secret")
	}()
	ev := waitEvent(t, buf, string(events.ToolApprovalType), 2*time.Second)
	if ev["name"] != "bash" {
		t.Fatalf("审批工具名应为 bash: %v", ev["name"])
	}
	if ev["hint"] == "" {
		t.Fatal("审批事件应携带被拒路径 hint")
	}
	id, _ := ev["id"].(string)
	if id == "" {
		t.Fatalf("审批事件缺 id: %v", ev)
	}
	m.dispatch(cmd(1, "approve", map[string]any{"workspace": ws, "session_id": sid, "id": id, "approve": true}))
	if got := <-done; !got {
		t.Fatal("approve=true 应放行")
	}

	// 拒绝路径：approve=false → 不放行
	done = make(chan bool, 1)
	go func() {
		done <- g.Release(context.Background(), sandbox.ExecSpec{Command: "cat /secret"}, "deny file-read*: /secret")
	}()
	ev = waitEvent(t, buf, string(events.ToolApprovalType), 2*time.Second)
	id, _ = ev["id"].(string)
	m.dispatch(cmd(2, "approve", map[string]any{"workspace": ws, "session_id": sid, "id": id, "approve": false}))
	if got := <-done; got {
		t.Fatal("approve=false 应拒绝")
	}

	// 全链路不应出现 sandbox_blocked 事件（已抽象到工具层审批）
	for _, l := range readLines(buf) {
		if l["event_type"] == "sandbox_blocked" {
			t.Fatal("不应再发 sandbox_blocked 事件")
		}
	}
}

// TestSandboxGatePreApprovedModesAutoRelease bash 工具层已预审的模式
// （accept-edits/manual/hitl/normal）与直通模式（full-access/all-pass）：
// 沙箱拦截不再询问，直接放行重跑（无 tool_approval_requested 事件）。
func TestSandboxGatePreApprovedModesAutoRelease(t *testing.T) {
	for _, mode := range []string{"accept-edits", "manual", "hitl", "full-access", "all-pass"} {
		t.Run(mode, func(t *testing.T) {
			m, buf := testManager(t)
			ws := t.TempDir()
			sid := newSessionID(t, m, buf, ws)
			m.dispatch(cmd(0, "switch_mode", map[string]any{"workspace": ws, "session_id": sid, "mode": mode}))
			g := &sandboxReleaseGate{m: m, ws: ws, id: sid}
			if !g.Release(context.Background(), sandbox.ExecSpec{Command: "cat /secret"}, "deny file-read*: /secret") {
				t.Fatalf("%s: 应自动放行（工具层已预审/直通）", mode)
			}
			for _, l := range readLines(buf) {
				if l["event_type"] == string(events.ToolApprovalType) || l["event_type"] == "sandbox_blocked" {
					t.Fatalf("%s: 不应发审批/拦截事件: %v", mode, l)
				}
			}
		})
	}
}
