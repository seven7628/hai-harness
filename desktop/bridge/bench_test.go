package main

// 性能基准：checkpoint-first（ViewReducer + SessionRecord）vs legacy（events.jsonl
// 全量扫描 buildSnapshot）恢复路径，以及 reducer 实时应用/提交成本。
//
// 规模参数贴近真实长会话：benchTurns 轮 LLM 交互，每轮 llm_start + N 个
// content_chunk + llm_end + tool_start + tool_end（含 agent_start/end 首尾），
// 事件总数 ≈ benchTurns × (5 + benchChunksPerTurn)。
//
// 运行：go test -bench 'Benchmark' -benchmem -run '^$' ./desktop/bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

const (
	benchTurns         = 2000 // 交互轮数（约 9 万事件，数十 MB 级）
	benchChunksPerTurn = 40   // 每轮 content_chunk 数
	benchWorkspaceKey  = "ws-key-bench"
	benchSessionID     = "s-bench-1"
)

// benchEventStream 生成一条贴近真实的混合事件流（主 agent 多轮 + 工具调用）。
func benchEventStream() []events.Event {
	base := time.Now().Add(-time.Hour)
	evs := make([]events.Event, 0, benchTurns*(5+benchChunksPerTurn)+2)
	evs = append(evs, &events.AgentStart{
		RunId: "root-1", Content: "bench 任务", Model: "deepseek-v4-flash",
		Timestamp: base, EventType: events.AgentStartType,
	})
	for i := 0; i < benchTurns; i++ {
		req := "q-" + itoa(i)
		t := base.Add(time.Duration(i) * time.Second)
		evs = append(evs, &events.LLMStart{RunId: "root-1", RequestId: req, Model: "deepseek-v4-flash", Timestamp: t, EventType: events.LLMStartType})
		for c := 0; c < benchChunksPerTurn; c++ {
			evs = append(evs, &events.ContentChunk{
				RunId: "root-1", RequestId: req, Index: int64(c),
				Content:   "这是一段用于基准测试的流式内容块文本，包含一些中文字符与数字 0123456789。",
				Timestamp: t.Add(time.Duration(c) * 5 * time.Millisecond), EventType: events.ContentChunkType,
			})
		}
		evs = append(evs, &events.LLMEnd{
			RunId: "root-1", RequestId: req, Model: "deepseek-v4-flash",
			Content: "最终回复", Usage: &core.Usage{Input: 1500, Output: 200, CacheRead: 800, Reasoning: 100},
			FinishReason: core.FinishReasonStop, Timestamp: t.Add(250 * time.Millisecond), EventType: events.LLMEndType,
		})
		toolID := "call-" + itoa(i)
		evs = append(evs, &events.ToolStart{
			RunId: "root-1", Id: toolID, Name: "bash", Arguments: `{"command":"ls -la"}`,
			Timestamp: t.Add(300 * time.Millisecond), EventType: events.ToolRunStartType,
		})
		evs = append(evs, &events.ToolResponse{
			RunId: "root-1", Id: toolID, Name: "bash", Result: "total 0\ndrwxr-xr-x",
			Timestamp: t.Add(500 * time.Millisecond), EventType: events.ToolRunEndType,
		})
	}
	evs = append(evs, &events.AgentEnd{
		RunId: "root-1", Content: "完成", FinishReason: core.FinishReasonStop,
		Usage:     &core.Usage{Input: 3000000, Output: 400000, CacheRead: 1600000, Reasoning: 200000},
		Timestamp: base.Add(time.Duration(benchTurns) * time.Second), EventType: events.AgentEndType,
	})
	return evs
}

// benchWriteLegacyEvents 把事件流写成 legacy events.jsonl（模拟 logEventLine 累积）。
func benchWriteLegacyEvents(b *testing.B, dir string, evs []events.Event) {
	b.Helper()
	f, err := os.Create(filepath.Join(dir, benchSessionID+".jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
}

// benchBuildRecord 用 reducer 应用事件流并原子提交一个 SessionRecord。
func benchBuildRecord(b *testing.B, dir string, evs []events.Event) (*session.FileStorage, *session.SessionRecord) {
	b.Helper()
	r := NewViewReducer()
	for _, e := range evs {
		if err := r.ApplyPlain(e); err != nil {
			b.Fatal(err)
		}
	}
	view, err := r.Marshal()
	if err != nil {
		b.Fatal(err)
	}
	store, err := session.NewSessionRecordStorage(dir)
	if err != nil {
		b.Fatal(err)
	}
	rec := session.NewSessionRecord(benchWorkspaceKey, benchSessionID, 1, r.LastSeq(), r.ClearGeneration(), session.State{}, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		b.Fatal(err)
	}
	return store, rec
}

// BenchmarkRestoreLegacyBuildSnapshot 旧恢复路径：全量读 events.jsonl + 逐行 parse + 聚合。
func BenchmarkRestoreLegacyBuildSnapshot(b *testing.B) {
	evs := benchEventStream()
	root := b.TempDir()
	// buildSnapshot(wsPath) 内部用 wsEventsDir(wsPath) = appDataDir()/events/<wsKey>
	// 需把 appDataDirOverride 指到 root 才能控制文件位置（与生产一致）。
	prev := appDataDirOverride
	appDataDirOverride = root
	defer func() { appDataDirOverride = prev }()
	dir := wsEventsDir(benchWorkspaceKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	benchWriteLegacyEvents(b, dir, evs)
	m := &manager{}
	fi, _ := os.Stat(filepath.Join(dir, benchSessionID+".jsonl"))
	b.Logf("legacy events.jsonl 大小: %.1f MiB, 事件数 %d", float64(fi.Size())/(1<<20), len(evs))
	b.StartTimer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, ok := m.buildSnapshot(benchWorkspaceKey, benchSessionID)
		if !ok || len(snap["events"].([]json.RawMessage)) == 0 {
			b.Fatal("snapshot 构建失败")
		}
	}
}

// BenchmarkRestoreCheckpointLoadRecord 新恢复路径：读单个 record + checksum 校验 + 解析 checkpoint。
func BenchmarkRestoreCheckpointLoadRecord(b *testing.B) {
	evs := benchEventStream()
	dir := b.TempDir()
	b.StopTimer()
	store, rec := benchBuildRecord(b, dir, evs)
	fi, _ := os.Stat(filepath.Join(dir, benchWorkspaceKey, benchSessionID+".record.json"))
	b.Logf("record.json 大小: %.2f MiB（同事件流）", float64(fi.Size())/(1<<20))
	b.StartTimer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loaded, err := store.LoadRecord(context.Background(), benchWorkspaceKey, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := parseCheckpoint(loaded.ViewCheckpoint); err != nil {
			b.Fatal(err)
		}
	}
	_ = rec
}

// BenchmarkReducerApplyAll 新方案全量重放成本（shadow migration 场景：旧 events 重放出 checkpoint）。
func BenchmarkReducerApplyAll(b *testing.B) {
	evs := benchEventStream()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewViewReducer()
		for _, e := range evs {
			if err := r.ApplyPlain(e); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := r.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCoordinatorCommitOnce 新方案每次提交边界的成本（Marshal + 原子落盘）：
// 实时路径新增的固定开销（旧方案每事件只 append 一行不 fsync）。
func BenchmarkCoordinatorCommitOnce(b *testing.B) {
	evs := benchEventStream()
	dir := b.TempDir()
	b.StopTimer()
	store, err := session.NewSessionRecordStorage(dir)
	if err != nil {
		b.Fatal(err)
	}
	r := NewViewReducer()
	for _, e := range evs {
		_ = r.ApplyPlain(e)
	}
	b.StartTimer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := r.Marshal()
		if err != nil {
			b.Fatal(err)
		}
		rec := session.NewSessionRecord(benchWorkspaceKey, benchSessionID, uint64(i+1), r.LastSeq(), 0, session.State{}, view)
		if err := store.CommitRecord(context.Background(), rec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReducerChunkApply 高频路径：流式 chunk 应用（相邻聚合 + 块索引查找）。
func BenchmarkReducerChunkApply(b *testing.B) {
	evs := benchEventStream()
	r := NewViewReducer()
	for _, e := range evs {
		_ = r.ApplyPlain(e)
	}
	chunk := &events.ContentChunk{
		RunId: "root-1", RequestId: "q-live", Index: 0, Content: "x",
		Timestamp: time.Now(), EventType: events.ContentChunkType,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunk.RequestId = "q-live-" + itoa(i%100)
		_ = r.ApplyPlain(chunk)
	}
}

// BenchmarkLegacyAppendEvents 旧方案实时路径：每个事件一次 JSON marshal + append 写日志
// （logEventLine，无 fsync）。对比 BenchmarkCoordinatorCommitOnce 看新方案提交边界的增量成本。
func BenchmarkLegacyAppendEvents(b *testing.B) {
	evs := benchEventStream()
	dir := b.TempDir()
	b.StopTimer()
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.StartTimer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := evs[i%len(evs)]
		line, err := json.Marshal(e)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckpointMarshalOnly 新方案提交边界中 Marshal 部分（不含 fsync 落盘）：
// 隔离「序列化成本」与「原子提交成本」，定位优化方向。
func BenchmarkCheckpointMarshalOnly(b *testing.B) {
	evs := benchEventStream()
	r := NewViewReducer()
	for _, e := range evs {
		_ = r.ApplyPlain(e)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
}
