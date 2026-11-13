package main

// P6-09：重启恢复（崩溃注入）。
//
// 做法：第一个 manager 跑到「一条已投递未完成 + 一条还在排队」的状态，把整个
// APP 数据目录**复制**一份（等价于进程在那一刻被杀：磁盘上就是这些字节），再用
// 第二个 manager 打开副本并恢复。这样不需要真的杀进程，也不会让第一个 manager 的
// 后台 goroutine 干扰断言。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// copyTree 递归复制目录（只用于测试里「拍一份磁盘快照」）。
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("复制 %s → %s 失败: %v", src, dst, err)
	}
}

// restartManagerFrom 用一份磁盘快照起第二个 manager（模拟进程重启）。
func restartManagerFrom(t *testing.T, snapshot string) (*manager, *syncBuf) {
	t.Helper()
	dst := t.TempDir()
	copyTree(t, snapshot, dst)
	appDataDirOverride = dst
	t.Cleanup(func() { appDataDirOverride = "" })
	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}
	t.Cleanup(func() { m.shutdownHubRuntimes() })
	return m, buf
}

// waitConversationFile 等状态文件出现指定片段（保证「拍快照」时磁盘上已经是
// 我们想注入崩溃的那个状态，不靠 sleep 猜）。
func waitConversationFile(t *testing.T, root string, gid agenthub.GroupID, want ...string) string {
	t.Helper()
	path := filepath.Join(root, "hub", "conversations", string(gid)+".json")
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			ok := true
			for _, w := range want {
				if !strings.Contains(last, w) {
					ok = false
					break
				}
			}
			if ok {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("对话状态文件没有到预期状态（缺 %v）：%s", want, last)
	return last
}

// TestRestartRestoresQueueAndInterruptsRunning 是 P6-09 的主验收：
//   - 已投递未完成（B 正在跑）→ 重启后 interrupted + 系统说明，**不重放**；
//   - 未投递（人类给 B 的第二件事）→ 恢复排队并接着投出去；
//   - 已完成（A 那一轮）→ 不重放。
func TestRestartRestoresQueueAndInterruptsRunning(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})

	a := createHubAgent(t, m, buf, 1, "重启A")
	b := createHubAgent(t, m, buf, 2, "重启B")
	g := convGroupWith(t, m, buf, 10, "重启群", map[string]any{"max_turns_per_human": 100}, a, b)
	fa := newConvProvider("@重启B 你来")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(a.ID), string(g.ID)), g, string(a.ID), fa)
	fb := newConvProvider("B 第一轮跑不完")
	releaseB := releaseOnce(fb.gated())
	defer releaseB()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(b.ID), string(g.ID)), g, string(b.ID), fb)

	// 人类 @A → A 秒回并 @B → B 开始跑（卡在门闩上 = 崩溃时「已投递未完成」）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@重启A 叫一下 B",
	}); !ok {
		t.Fatalf("human send #1: %s", errMsg)
	}
	fb.waitStarted(t)
	// 人类再 @B：B 在跑 → 这条排队（崩溃时「未投递」）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@重启B 第二件事",
	}); !ok {
		t.Fatalf("human send #2: %s", errMsg)
	}
	convWait(t, "第二件事排队", func() bool { return convView(m, g.ID).QueueLen == 1 })
	root := appDataDir()
	waitConversationFile(t, root, g.ID, `"delivery_status": "running"`, `"delivery_status": "queued"`, `"target": "`+string(b.ID)+`"`)

	// —— 崩溃注入：磁盘快照 + 第二个 manager ——
	m2, buf2 := restartManagerFrom(t, root)
	// 重启后的 B 用新的 fake（恢复排队的那条要靠它跑）。
	fb2 := newConvProvider("B 重启后跑第二件")
	setGroupFakeLoop(t, m2, mustGroupSession(t, m2, string(b.ID), string(g.ID)), g, string(b.ID), fb2)
	fa2 := newConvProvider("A 不该重放")
	setGroupFakeLoop(t, m2, mustGroupSession(t, m2, string(a.ID), string(g.ID)), g, string(a.ID), fa2)

	m2.restoreHubConversations()

	// ① 恢复：队列里那条还在，状态还是 running。
	convWait(t, "重启后队列恢复", func() bool { return convView(m2, g.ID).QueueLen == 1 })
	// ② 已投递未完成 → interrupted + 系统说明。
	msgs := waitConvSystemMessage(t, m2, g.ID, "Hub 重启时这一轮还没跑完")
	last := convSystemMessages(msgs)[len(convSystemMessages(msgs))-1]
	if !strings.Contains(last.Text, "重启B") || !strings.Contains(last.Text, "不会自动重试") {
		t.Fatalf("重启说明 = %q, want 点名 B 且说明不重试", last.Text)
	}
	var interrupted bool
	for _, d := range m2.hubConversation().Deliveries(g.ID) {
		if d.Target == b.ID && d.Status == agenthub.DeliveryFailed && d.Reason == agenthub.DeliveryReasonInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("重启后 B 的 running 记录没标 interrupted：%+v", m2.hubConversation().Deliveries(g.ID))
	}
	// ③ 不重放已完成的 A：恢复不会给 A 再投一轮。
	time.Sleep(150 * time.Millisecond)
	if n := fa2.callCount(); n != 0 {
		t.Fatalf("重启后把已完成的 A 重放了 %d 次", n)
	}
	// ④ 未投递的那条接着投：B 在重启后真的跑起来，且看到的是「第二件事」。
	convWait(t, "重启后恢复的队列被投出去", func() bool { return fb2.callCount() >= 1 })
	if text := convRequestText(fb2.lastRequest()); !strings.Contains(text, "第二件事") {
		t.Fatalf("重启后投出去的不是排队的那条：%q", text)
	}
	// 重启后的视图里没有幽灵队列：跑完就清空。
	convWait(t, "重启后的队列跑空", func() bool { return convView(m2, g.ID).QueueLen == 0 })
	if evs := hubEvents(t, buf2, "hub_conversation"); len(evs) == 0 {
		t.Fatal("重启恢复没有发 hub_conversation 事件（前端看不到恢复后的状态）")
	}
}

// TestRestartCorruptStateNotOverwritten：坏文件只告警、不覆盖，重启后该群照常可用
// （只是不再持久化）。
func TestRestartCorruptStateNotOverwritten(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "坏档A")
	b := createHubAgent(t, m, buf, 2, "坏档B")
	g := convGroupWith(t, m, buf, 10, "坏档群", map[string]any{"max_turns_per_human": 100}, a, b)
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@坏档A 先跑一轮",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	root := appDataDir()
	waitConversationFile(t, root, g.ID, `"conversation_id"`)

	m2, buf2 := restartManagerFrom(t, root)
	path := filepath.Join(appDataDir(), "hub", "conversations", string(g.ID)+".json")
	garbage := []byte("{ corrupted on purpose")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	m2.restoreHubConversations()

	// 告警一条（前端可提示），且文件原样保留。
	alerts := hubEvents(t, buf2, "hub_alert")
	found := false
	for _, ev := range alerts {
		if code, _ := ev["code"].(string); code == "hub_conversation_corrupt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("坏文件没有告警：%+v", alerts)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(garbage) {
		t.Fatalf("坏文件被改写了（err=%v）：%q", err, string(got))
	}
	// 该群照常可用：恢复后仍然能接人类消息（只是不落盘）。
	if ok, errMsg, _ := hubCmd(t, m2, buf2, 21, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "坏档之后还能聊",
	}); !ok {
		t.Fatalf("坏档之后群不可用: %s", errMsg)
	}
	if got, _ := os.ReadFile(path); string(got) != string(garbage) {
		t.Fatalf("后续状态变化把坏文件覆盖了：%q", string(got))
	}
}
