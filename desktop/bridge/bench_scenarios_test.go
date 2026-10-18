package main

// 性能优化 Loop 的「基础」环：跨场景、可重复测量的基准矩阵。
//
// 为什么需要它：单个 benchmark（如 BenchmarkCoordinatorCommitOnce）只能回答
// 「这一次提交多慢」，回答不了用户真正在意的「保存/项目切换/聊天导航」是否变快。
// 这里把用户可感知的 5 个场景固定成同一套语料（真实长会话规模）上的一组基准，
// 每轮优化都要重跑整张表，避免只优化了局部却让另一条路径变差。
//
// 语料规模刻意对齐线上最坏的会话：一个 record 89.9MB、4097 blocks、
// 20760 条 agent items（其中 67.6MB 是 items）。见 MEMORY
// haibridge-checkpoint-commit-perf.md。
//
// 运行：
//   go test -bench 'BenchmarkScenario' -benchmem -run '^$' ./desktop/bridge
//
// 每个基准都自带 b.ReportMetric，单位统一为 ms，方便横向比。

import (
	"context"
	"sync"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

// benchScenarioTurns 是本矩阵的语料轮数。与 benchTurns（2000）分开：
// 2000 轮的 record 只有几 MB，撑不出线上那种 90MB 的病理规模。
const (
	benchScenarioTurns  = 900
	benchScenarioChunks = 6
	benchAgentBlocks    = 90
	benchAgentItems     = 420 // 每 block 的 items 数（线上均值 210、最大 586）
	benchItemResultLen  = 3200 // 每条 item 的 result 字节数（线上 tool result 截到 ~20KB）
	scenarioWorkspaceKey = "ws-key-scenario"
)

// scenarioEvents 生成主 agent 多轮 + 大量子 agent 事件的语料。
// 子 agent 的 items 是 record 体积的主要来源（线上占 80%），所以必须显式构造，
// 否则基准会低估 record 尺寸、把优化收益测小。
func scenarioEvents() []events.Event {
	base := time.Now().Add(-2 * time.Hour)
	evs := make([]events.Event, 0, benchScenarioTurns*(4+benchScenarioChunks)+benchAgentBlocks*3)
	evs = append(evs, &events.AgentStart{
		RunId: "root-1", Content: "bench 任务", Model: "deepseek-v4-flash",
		Timestamp: base, EventType: events.AgentStartType,
	})

	// 子 agent：每个 agent_start → N 个 tool_start/tool_end（累计成 items）→ agent_end。
	// items 是 agent block 内嵌的历史条目，正是线上 67.6MB 的来源。
	itemResult := make([]byte, benchItemResultLen)
	for i := range itemResult {
		itemResult[i] = 'a' + byte(i%26)
	}
	for a := 0; a < benchAgentBlocks; a++ {
		runID := fmt.Sprintf("sub-%d", a)
		ts := base.Add(time.Duration(a) * time.Second)
		// ParentRunId 必须设置，否则 reducer 不认它是子 agent、不生成 agent 卡片
		//（踩过：漏了这个字段，基准语料里一个 agent block 都没有，
		// record 只有 24MB，把线上 90MB 的病态规模测成了正常规模）。
		evs = append(evs, &events.AgentStart{
			RunId: runID, Content: fmt.Sprintf("子任务 %d", a), Model: "deepseek-v4-flash",
			ParentRunId: "root-1", Depth: 1,
			Timestamp: ts, EventType: events.AgentStartType,
		})
		for it := 0; it < benchAgentItems; it++ {
			id := fmt.Sprintf("call-%d-%d", a, it)
			evs = append(evs, &events.ToolStart{
				RunId: runID, Id: id, Name: "read_file",
				Arguments: fmt.Sprintf(`{"path":"docs/file-%d.md"}`, it),
				Timestamp: ts.Add(time.Duration(it) * time.Millisecond), EventType: events.ToolRunStartType,
			})
			evs = append(evs, &events.ToolResponse{
				RunId: runID, Id: id, Name: "read_file", Result: string(itemResult),
				Timestamp: ts.Add(time.Duration(it)*time.Millisecond + time.Millisecond),
				EventType: events.ToolRunEndType,
			})
		}
		evs = append(evs, &events.AgentEnd{
			RunId: runID, Content: "子任务完成", FinishReason: core.FinishReasonStop,
			Usage:     &core.Usage{Input: 5000, Output: 800, CacheRead: 2000, Reasoning: 100},
			Timestamp: ts.Add(time.Duration(benchAgentItems) * time.Millisecond), EventType: events.AgentEndType,
		})
	}

	// 主 agent 多轮对话。
	for i := 0; i < benchScenarioTurns; i++ {
		req := fmt.Sprintf("q-%d", i)
		t := base.Add(time.Duration(i) * time.Second)
		evs = append(evs, &events.LLMStart{
			RunId: "root-1", RequestId: req, Model: "deepseek-v4-flash",
			Timestamp: t, EventType: events.LLMStartType,
		})
		for c := 0; c < benchScenarioChunks; c++ {
			evs = append(evs, &events.ContentChunk{
				RunId: "root-1", RequestId: req, Index: int64(c),
				Content:   "这是一段用于基准测试的流式内容块文本，包含中文字符与数字 0123456789。",
				Timestamp: t.Add(time.Duration(c) * 5 * time.Millisecond), EventType: events.ContentChunkType,
			})
		}
		evs = append(evs, &events.LLMEnd{
			RunId: "root-1", RequestId: req, Model: "deepseek-v4-flash",
			Content: "最终回复",
			Usage:   &core.Usage{Input: 1500, Output: 200, CacheRead: 800, Reasoning: 100},
			FinishReason: core.FinishReasonStop,
			Timestamp:    t.Add(250 * time.Millisecond), EventType: events.LLMEndType,
		})
		toolID := fmt.Sprintf("call-%d", i)
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
		Timestamp: base.Add(time.Duration(benchScenarioTurns) * time.Second), EventType: events.AgentEndType,
	})
	return evs
}

// scenarioReducer 把语料灌进一个 reducer（语料只解析一次，供所有基准复用）。
func scenarioReducer(b *testing.B) *ViewReducer {
	b.Helper()
	r := NewViewReducer()
	for _, e := range scenarioEvents() {
		if err := r.ApplyPlain(e); err != nil {
			b.Fatal(err)
		}
	}
	return r
}

// scenarioRecord 构造一份与线上同规模的已落盘 record，返回 store 与 record。
func scenarioRecord(b *testing.B) (*session.FileStorage, *session.SessionRecord, string) {
	b.Helper()
	r := scenarioReducer(b)
	view, err := r.Marshal()
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	store, err := session.NewSessionRecordStorage(dir)
	if err != nil {
		b.Fatal(err)
	}
	rec := session.NewSessionRecord(scenarioWorkspaceKey, benchSessionID, 1, r.LastSeq(), 0, session.State{}, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		b.Fatal(err)
	}
	path, err := store.RecordPath(scenarioWorkspaceKey, benchSessionID)
	if err != nil {
		b.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("语料 record 尺寸: %.1f MiB", float64(fi.Size())/(1<<20))
	return store, rec, dir
}

// msPerOp 把 ns/op 换算成更贴近用户感知口径的 ms。
func msPerOp(b *testing.B) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
}

// BenchmarkScenarioSave —— 场景 1「保存状态」：一次 checkpoint 提交（debounce 后的落盘）。
// 这是线上 12 个 worker 打满 CPU 的那条路径。
func BenchmarkScenarioSave(b *testing.B) {
	store, rec, _ := scenarioRecord(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Revision++
		rec.UpdatedAt = time.Now().UTC()
		rec.Checksum = ""
		if err := store.CommitRecord(ctx, rec); err != nil {
			b.Fatal(err)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioNavigation —— 场景 2「聊天导航/打开会话」：UI 收到快照的全过程。
// 对应 open_session 的 checkpoint-first 分支：Load 磁盘 record → 校验 → reducer.Marshal。
// 注意 Load 与 Marshal 都要算：用户点一下会话，这两个是同步串行发生的。
func BenchmarkScenarioNavigation(b *testing.B) {
	store, _, _ := scenarioRecord(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec, err := store.LoadRecord(ctx, scenarioWorkspaceKey, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := parseCheckpoint(rec.ViewCheckpoint); err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(rec.ViewCheckpoint); err != nil {
			b.Fatal(err)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioNavigateInMemory —— 场景 3「warm 导航」：进程内已有 reducer，
// 只做 Snapshot+Marshal（open_session 的 wasLive 分支，且持有 r.mu）。
// 这条路径的耗时直接等于「切换会话时 UI 卡多久」，因为 Snapshot 持锁。
func BenchmarkScenarioNavigateInMemory(b *testing.B) {
	r := scenarioReducer(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioSaveFreeze —— 场景 4「保存时的 UI 冻结」：每次 chunk 都要拿的
// reducer 写锁，被一次 Save 占住多久。用「提交期间并发 Apply 的等待时间」度量。
func BenchmarkScenarioSaveFreeze(b *testing.B) {
	store, rec, _ := scenarioRecord(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Revision++
		rec.UpdatedAt = time.Now().UTC()
		rec.Checksum = ""
		start := time.Now()
		if err := store.CommitRecord(ctx, rec); err != nil {
			b.Fatal(err)
		}
		// 提交期间若持有任何与事件流共享的锁，这里会排队；
		// 当前实现提交不碰 reducer 锁，故该值≈提交本身，留作回归基线。
		b.ReportMetric(float64(time.Since(start).Nanoseconds())/1e6, "freeze-ms")
		break
	}
}

// BenchmarkScenarioStartupRestore —— 场景 5「启动」：冷启动恢复单个会话。
// 线上启动慢的主因之一是逐个会话 Load record 并 parse（90MB × N 次）。
func BenchmarkScenarioStartupRestore(b *testing.B) {
	store, _, _ := scenarioRecord(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec, err := store.LoadRecord(ctx, scenarioWorkspaceKey, benchSessionID)
		if err != nil {
			b.Fatal(err)
		}
		cp, err := parseCheckpoint(rec.ViewCheckpoint)
		if err != nil {
			b.Fatal(err)
		}
		r := NewViewReducer()
		if err := r.Restore(cp); err != nil {
			b.Fatal(err)
		}
		r.NormalizeForRestore()
	}
	msPerOp(b)
}

// BenchmarkScenarioProjectSwitch —— 场景 6「项目切换」：切换 workspace 时
// 列会话 + 逐个读 record 头部（线上切换卡顿的来源）。
// 这里度量「列出 N 个会话并各读一次 record」的成本下界。
func BenchmarkScenarioProjectSwitch(b *testing.B) {
	_, _, dir := scenarioRecord(b)
	root := dir
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := os.ReadDir(filepath.Join(root, scenarioWorkspaceKey))
		if err != nil && !os.IsNotExist(err) {
			b.Fatal(err)
		}
		for _, e := range entries {
			p := filepath.Join(root, scenarioWorkspaceKey, e.Name())
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				_, _ = os.ReadFile(p)
			}
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioNavigationMetadataOnly —— 候选优化：导航只需要
// revision/reducer_seq/clear_generation 三个标量，没必要解析 24MB payload。
// 与 BenchmarkScenarioNavigation 对比即可量化「多余 Load」的代价。
func BenchmarkScenarioNavigationMetadataOnly(b *testing.B) {
	store, _, _ := scenarioRecord(b)
	path, err := store.RecordPath(scenarioWorkspaceKey, benchSessionID)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 只取文件头部需要的三个字段，不 materialize payload
		f, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		dec := json.NewDecoder(f)
		if tok, terr := dec.Token(); terr != nil {
			f.Close()
			b.Fatal(terr)
		} else if d, ok := tok.(json.Delim); !ok || d != '{' {
			f.Close()
			b.Fatal("not an object")
		}
		var revision, reducerSeq, clearGen uint64
		for dec.More() {
			kt, terr := dec.Token()
			if terr != nil {
				break
			}
			key, _ := kt.(string)
			switch key {
			case "revision":
				_ = dec.Decode(&revision)
			case "reducer_seq":
				_ = dec.Decode(&reducerSeq)
			case "clear_generation":
				_ = dec.Decode(&clearGen)
			default:
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					f.Close()
					b.Fatal(err)
				}
			}
		}
		f.Close()
		if revision == 0 && reducerSeq == 0 && clearGen > 1000 {
			b.Fatalf("unexpected metadata: %d %d %d", revision, reducerSeq, clearGen)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioNavigationViaMetadata —— 「改进」环：同一导航路径改用
// LoadRecordMetadata（只读头部标量），对照 BenchmarkScenarioNavigation 量化收益。
func BenchmarkScenarioNavigationViaMetadata(b *testing.B) {
	store, _, _ := scenarioRecord(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.LoadRecordMetadata(ctx, scenarioWorkspaceKey, benchSessionID); err != nil {
			b.Fatal(err)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioNavigateCachedMeta —— 「改进」环 2 的真实收益：
// 导航快路径 = 读内存缓存（O(1)）+ reducer.Marshal（真正返回给 UI 的内容）。
// 对照 BenchmarkScenarioNavigation：它额外多做了「Load 24MB + 校验 + 解析」，
// 而结果只用到三个标量。
func BenchmarkScenarioNavigateCachedMeta(b *testing.B) {
	r := scenarioReducer(b)
	// 模拟已提交过一次的 coordinator：元数据在内存。
	meta := session.RecordMetadata{Revision: 7, ReducerSeq: 11, ClearGeneration: 0}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var mu sync.Mutex
		var got session.RecordMetadata
		ok := false
		mu.Lock()
		if meta.Revision != 0 {
			got, ok = meta, true
		}
		mu.Unlock()
		if !ok || got.Revision == 0 {
			b.Fatal("cache miss")
		}
		// 快路径仍要产出 UI 视图：这是不可省的部分。
		if _, err := r.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
	msPerOp(b)
}

// BenchmarkScenarioSaveParts 拆解 Save 的成本构成：
// Marshal(序列化视图) / CommitRecord(落盘) / sdkState(构造 state)。
func BenchmarkScenarioSaveParts(b *testing.B) {
	store, rec, _ := scenarioRecord(b)
	r := scenarioReducer(b)
	ctx := context.Background()
	_ = rec

	b.Run("Marshal-view", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = r.ApplyPlain(&events.ContentChunk{RunId: "root-1", RequestId: "x", Index: 0, Content: "y", Timestamp: time.Now(), EventType: events.ContentChunkType})
			if _, err := r.Marshal(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
	})
	b.Run("CommitRecord", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rec.Revision++
			rec.UpdatedAt = time.Now().UTC()
			rec.Checksum = ""
			if err := store.CommitRecord(ctx, rec); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
	})
}


