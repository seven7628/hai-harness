package main

// checkpoint / replay 不受 cwd 状态影响（历史设计 §12.6 的 V5）。
//
// 真正的不变量（§15.5 修正过的表述）**不是**「cwd 变化不改 checkpoint」——
// bash 的回执会进工具结果 ⇒ 进 history ⇒ 进 checkpoint，那是**预期的**。
// 这里要钉的是两件不同的事：
//
//  1. **同一事件流重放两次 ⇒ 逐字节相同的 checkpoint**（重放确定性）；
//  2. `workdir.State`（会话级 cwd）**不参与序列化**：它的当前目录哪怕指向一个
//     事件流里从未出现过的路径，也不该在 checkpoint 字节里留下任何痕迹。
//
// 做法走**生产的重放入口** bs.migrateLegacyToCheckpoint（events.jsonl → reducer →
// commitSync → record.ViewCheckpoint），两个会话喂**同一份事件流**，只在内存里的
// cwd 状态上不同，然后逐字节比对 record 里的 checkpoint。
//
// 为什么要在这一层测：装配层给每个会话 new 一个 workdir.State（main.go:2444），
// 而 checkpoint 由 reducer 生成。若哪天有人「顺手」把 cwd 塞进 checkpoint（很像
// 有用的优化），单看任一测试都不红 —— 只有并排比对两个会话的字节才会红。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"
)

// cwdCheckpointEvents 构造一份确定性事件流（时间戳固定 ⇒ 可逐字节比对）。
//
// 刻意**不含**任何 cd / 回执文本：本用例要证明的是「cwd 状态本身不进 checkpoint」，
// 而工具结果里的回执属于 history（另一回事，见文件头）。
func cwdCheckpointEvents(t *testing.T) []byte {
	t.Helper()
	ts := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	evs := []events.Event{
		&events.AgentStart{
			Content: "cwd checkpoint probe", RunId: "r1", Model: "deepseek-v4-flash",
			Timestamp: ts, EventType: events.AgentStartType,
		},
		&events.ToolStart{
			Index: 1, Id: "c1", Name: "bash", Arguments: `{"command":"ls"}`,
			RunId: "r1", Timestamp: ts.Add(time.Second), EventType: events.ToolRunStartType,
		},
		&events.ToolResponse{
			Index: 2, Id: "c1", Name: "bash", RunId: "r1", Result: "a.txt",
			Timestamp: ts.Add(2 * time.Second), EventType: events.ToolRunEndType,
		},
	}
	var buf bytes.Buffer
	for _, ev := range evs {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %s: %v", ev.Type(), err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// TestCheckpointBytesIndependentOfCwdState 同一事件流重放两次 ⇒ checkpoint 逐字节相同，
// 且会话 cwd 状态（workdir.State）不在其中。
func TestCheckpointBytesIndependentOfCwdState(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	evs := cwdCheckpointEvents(t)

	// 哨兵：一个真实存在、但**事件流里从未出现**的目录。
	// 若 cwd 被序列化，它的路径必然出现在某个会话的 checkpoint 里。
	sentinel := filepath.Join(ws, "sentinel-cwd-only")
	if err := os.MkdirAll(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}

	// 两个会话：同一工作区（同一份事件流各自一份 jsonl），只在内存 cwd 上不同。
	replay := func(id string, cdTo string) *bridgeSession {
		t.Helper()
		bs, err := m.create(ws, id, "")
		if err != nil {
			t.Fatalf("create %s 失败: %v", id, err)
		}
		if bs.coord == nil || bs.reducer == nil {
			t.Fatalf("%s 没装配 reducer/coordinator", id)
		}
		if bs.reducer.BlockCount() != 0 {
			t.Fatalf("%s 的 reducer 不是空的，重放会双计", id)
		}
		if cdTo != "" {
			// 模拟「模型在这个会话里 cd 过」：只动内存状态，不产生任何事件。
			bs.wd.Set(workdir.MainKey, cdTo)
			if got := bs.wd.Current(workdir.MainKey); got != cdTo {
				t.Fatalf("cwd 状态没设上：%q", got)
			}
		}
		if err := os.WriteFile(filepath.Join(bs.eventsDir, bs.id+".jsonl"), evs, 0o600); err != nil {
			t.Fatal(err)
		}
		if !bs.migrateLegacyToCheckpoint() {
			t.Fatalf("%s 的 legacy 重放没生成 checkpoint", id)
		}
		return bs
	}

	bsA := replay("cwd-ckpt-a", sentinel) // 有 cd 的会话
	bsB := replay("cwd-ckpt-b", "")       // 没 cd 的会话

	// record 里落盘的 checkpoint（生产形态的字节）。
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	load := func(id string) []byte {
		t.Helper()
		rec, err := store.LoadRecord(context.Background(), workspaceKey(ws), id)
		if err != nil {
			t.Fatalf("读 %s 的 record 失败: %v", id, err)
		}
		if len(rec.ViewCheckpoint) == 0 {
			t.Fatalf("%s 的 view_checkpoint 是空的", id)
		}
		return rec.ViewCheckpoint
	}
	cpA, cpB := load("cwd-ckpt-a"), load("cwd-ckpt-b")

	// 1) 两次重放逐字节相同（重放确定性；cwd 不会让 checkpoint 漂）。
	if !bytes.Equal(cpA, cpB) {
		t.Fatalf("同一事件流的两次重放 checkpoint 不同（%d vs %d 字节）\nA 首行: %s\nB 首行: %s",
			len(cpA), len(cpB), checkpointFirstLine(cpA), checkpointFirstLine(cpB))
	}
	// 对照（证伪性）：事件流里的内容**确实**在 checkpoint 里 —— 否则下面的
	// 「哨兵不出现」可能只是因为字节里什么都没有，那样断言就是假保护。
	if !bytes.Contains(cpA, []byte("a.txt")) {
		t.Fatalf("checkpoint 里没有事件流内容（工具结果 a.txt），哨兵断言失去意义")
	}
	// 2) cwd 状态不在 checkpoint 里（哨兵路径不出现）。
	if bytes.Contains(cpA, []byte(sentinel)) || bytes.Contains(cpB, []byte(sentinel)) {
		t.Fatalf("checkpoint 里出现了会话 cwd（%q）—— workdir.State 应该是纯内存状态", sentinel)
	}
	// 3) 顺带钉住 reducer 侧的视图字节同样不含 cwd（record 之外的第二条路）。
	viewA, err := bsA.reducer.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	viewB, err := bsB.reducer.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viewA, viewB) {
		t.Fatal("同一事件流的 reducer 视图两次不同")
	}
	if bytes.Contains(viewA, []byte(sentinel)) {
		t.Fatal("reducer 视图里出现了会话 cwd")
	}
}

// checkpointFirstLine 取首行的前若干字节（失败时的定位线索，不刷屏）。
func checkpointFirstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
