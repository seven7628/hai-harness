package main

// P2-05 复审 NB-1：回推幂等的真实重启回归。
//
// 之前只有直接构造 Session 的单测，把生产接线退回 History-only 仍然全绿。
// 这里跑完整链路：push 成功 + MarkNotified 失败 → 崩溃态整盘快照 → 重建 manager
// → recoverHubTasks，断言模型侧恰好一份、账本 delivered。
//
// 崩溃模拟口径：checkpoint-first 的权威恢复源是 SessionRecord.sdk_state。
// 只回滚 .record.json 不够——blocks 走同目录 sidecar，单独回滚会被判 corrupt
// 而退回 legacy，测不到 checkpoint-first 路径；所以整棵 appdata 快照。

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func restartManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	return &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}, buf
}

func restartCopyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy appdata: %v", err)
	}
	return dst
}

func restartCountTaskResult(bs *bridgeSession, taskID string) int {
	prefix := "〔后台任务 " + taskID + " "
	count := func(list []core.Message) int {
		n := 0
		for _, m := range list {
			for _, c := range m.Content {
				if c.Type == core.ContentTypeTaskResult && strings.HasPrefix(c.Content, prefix) {
					n++
				}
			}
		}
		return n
	}
	return count(bs.s.Messages()) + count(bs.s.PendingSnapshot())
}

func TestHubTaskResultSurvivesMarkNotifiedFailureRestart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	prev := appDataDirOverride
	appDataDirOverride = root
	t.Cleanup(func() { appDataDirOverride = prev })

	m1, buf1 := restartManager(t)
	agent := createHubAgent(t, m1, buf1, 1, "RestartDedup")
	agentID := string(agent.ID)
	if _, err := m1.hubDMSession(agentID); err != nil {
		t.Fatal(err)
	}
	st, err := m1.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: agent.ID, AgentName: agent.Name},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "重启幂等", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	settled, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: task.ID, RunID: "run-restart", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Summary: "重启后只应看到一份"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 任务目录只读 → MarkNotified 必然失败（结果已进 inbox）。
	tdir := filepath.Join(appDataDir(), "hub", "tasks", string(task.ID))
	t.Cleanup(func() { _ = os.Chmod(tdir, 0o755) })
	if err := os.Chmod(tdir, 0o555); err != nil {
		t.Fatal(err)
	}
	m1.pushHubTaskResult(ledger, settled, &events.AgentEnd{
		EventType: events.AgentEndType, RunId: "run-restart", Content: "重启后只应看到一份",
	})
	_ = os.Chmod(tdir, 0o755)

	status, notified := restartTaskLedger(t, m1, task.ID)
	if status == agenthub.TaskDelivered || len(notified) != 0 {
		t.Fatalf("setup: MarkNotified 未失败: status=%s notified=%v", status, notified)
	}
	origin, err := m1.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if n := restartCountTaskResult(origin, string(task.ID)); n != 1 {
		t.Fatalf("崩溃前可见 task_result = %d, want 1", n)
	}

	snap := restartCopyTree(t, appDataDir())
	m1.closeHubRuntimes()
	appDataDirOverride = snap

	m2, _ := restartManager(t)
	t.Cleanup(m2.closeHubRuntimes)
	if _, err := m2.hubDMSession(agentID); err != nil {
		t.Fatal(err)
	}
	m2.recoverHubTasks()
	origin2, err := m2.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if n := restartCountTaskResult(origin2, string(task.ID)); n != 1 {
		t.Fatalf("重启后模型可见 task_result = %d, want 1（双推回归）", n)
	}
	status, notified = restartTaskLedger(t, m2, task.ID)
	if status != agenthub.TaskDelivered {
		t.Fatalf("重启后 status = %s, want delivered", status)
	}
	if len(notified) != 1 || notified[0] != "run-restart" {
		t.Fatalf("notified = %v, want [run-restart]", notified)
	}
}

func restartTaskLedger(t *testing.T, m *manager, id agenthub.TaskID) (agenthub.TaskStatus, []string) {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	got, err := agenthub.NewLedger(st).Replay(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status, got.NotifiedRuns
}
