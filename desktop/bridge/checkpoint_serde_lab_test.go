package main

// 提交路径的序列化/反序列化成本分解（默认跳过）。
//
// 为什么分阶段跑：这台机器在 17GB 物理内存下已经用了 ~19GB swap，
// 一次性把 view(210MB) + Checkpoint + 快照 + 缓冲全留在堆上会把测量变成
// 「换页速度」。因此每个阶段用独立进程（HAI_LAB_PHASE 选择），阶段之间
// 显式丢引用 + debug.FreeOSMemory。
//
// 跑法：
//
//	for p in load parse marshal commit; do
//	  HAI_LAB_PHASE=$p HAI_LAB_SESSION=$HOME/.go-code/sessions/<ws>/<sid>.record.json \
//	    go test ./desktop/bridge/ -run TestCheckpointSerdeLab -v -count=1
//	done

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/session"
)

func TestCheckpointSerdeLab(t *testing.T) {
	phase := os.Getenv("HAI_LAB_PHASE")
	recordPath := os.Getenv("HAI_LAB_SESSION")
	if phase == "" || recordPath == "" {
		t.Skipf("设置 HAI_LAB_PHASE=<load|parse|marshal|commit> 与 HAI_LAB_SESSION=<sid>.record.json")
	}
	root, wsKey, sid := serdeClone(t, recordPath)
	defer session.WaitBackgroundCompacts(60 * time.Second)
	store, err := session.NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 统一的前置：读回 record（各阶段的公共成本，单独报告）。
	// view 阶段量的是生产路径本身，跳过这份前置（否则会把慢路径算进结果）。
	var rec *session.SessionRecord
	var view []byte
	var t0 time.Time
	loadDur := time.Duration(0)
	if phase != "view" {
		t0 = time.Now()
		rec, err = store.LoadRecord(ctx, wsKey, sid)
		if err != nil {
			t.Fatal(err)
		}
		loadDur = time.Since(t0)
		view = rec.ViewCheckpoint
	}
	if rec != nil {
		t.Logf("[%s] 前置 LoadRecord=%s view=%.1fMB spans=%d body=%.1fMB ref=%v", phase,
			loadDur.Round(time.Millisecond), float64(len(view))/1e6,
			len(rec.LoadedBlockSpans), float64(len(rec.BodyView))/1e6,
			bytes.Contains(rec.BodyView, []byte("$blocks_ref")))
	}

	switch phase {
	case "load":
		// 只报 LoadRecord（反序列化 + 从 sidecar 拼回 185MB）
		return

	case "parse":
		var cp *Checkpoint
		t0 = time.Now()
		cp, err = parseCheckpoint(view)
		if err != nil {
			t.Fatal(err)
		}
		parseDur := time.Since(t0)
		t.Logf("parseCheckpoint(反序列化 %.1fMB) = %s  blocks=%d",
			float64(len(view))/1e6, parseDur.Round(time.Millisecond), len(cp.Blocks))
		cp = nil
		debug.FreeOSMemory()
		return

	case "turns":
		// 单独量 turns 数组（24MB=94% body）：解码、编码各多贵。
		var doc struct {
			SchemaVersion int             `json:"schema_version"`
			Turns         json.RawMessage `json:"turns"`
		}
		t0 = time.Now()
		if err := json.Unmarshal(rec.BodyView, &doc); err != nil {
			t.Fatal(err)
		}
		scan := time.Since(t0)
		t.Logf("跳过式扫描 body(turns 不进对象图) = %s turns=%.1fMB",
			scan.Round(time.Millisecond), float64(len(doc.Turns))/1e6)

		var turns []Turn
		t0 = time.Now()
		if err := json.Unmarshal(doc.Turns, &turns); err != nil {
			t.Fatal(err)
		}
		decode := time.Since(t0)
		t.Logf("turns 解码 %d 条 = %s", len(turns), decode.Round(time.Millisecond))

		t0 = time.Now()
		out, err := json.Marshal(turns)
		if err != nil {
			t.Fatal(err)
		}
		encode := time.Since(t0)
		t.Logf("turns 编码 = %s bytes=%.1fMB", encode.Round(time.Millisecond), float64(len(out))/1e6)
		return

	case "ab":
		// 同进程 A/B：拼装出来的 spans（堆上连续）vs mmap 窗口（按页），
		// 以及「解码时校验摘要」的增量成本。三者在同一份数据上交替跑，
		// 抵消机器抖动（本机 swap 曾让同一项差 200 倍）。
		rec2, lerr := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr != nil {
			t.Fatal(lerr)
		}
		defer rec2.ReleaseView()
		spansA := rec.LoadedBlockSpans
		spansB := rec2.LoadedBlockSpans
		hashesB := rec2.LoadedBlockHashes
		t.Logf("A(拼装)=%d spans B(mmap)=%d spans", len(spansA), len(spansB))
		for round := 0; round < 3; round++ {
			outA := make([]Block, len(spansA))
			t0 = time.Now()
			if err := decodeBlocksParallel(spansA, nil, outA); err != nil {
				t.Fatal(err)
			}
			dA := time.Since(t0)
			outA = nil

			outB := make([]Block, len(spansB))
			t0 = time.Now()
			if err := decodeBlocksParallel(spansB, nil, outB); err != nil {
				t.Fatal(err)
			}
			dB := time.Since(t0)
			outB = nil

			outC := make([]Block, len(spansB))
			t0 = time.Now()
			if err := decodeBlocksParallel(spansB, hashesB, outC); err != nil {
				t.Fatal(err)
			}
			dC := time.Since(t0)
			outC = nil
			debug.FreeOSMemory()
			t.Logf("round %d: 拼装=%s mmap=%s mmap+校验=%s", round,
				dA.Round(time.Millisecond), dB.Round(time.Millisecond), dC.Round(time.Millisecond))
		}
		return

	case "view":
		// 生产路径（S2）：LoadRecordForView（不拼装 blocks）+ 骨架/并行解码 + 释放映射。
		t0 = time.Now()
		rec2, lerr := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr != nil {
			t.Fatal(lerr)
		}
		loadFast := time.Since(t0)
		t0 = time.Now()
		cp2, perr2 := parseCheckpointFromRecord(rec2)
		if perr2 != nil {
			t.Fatal(perr2)
		}
		decode := time.Since(t0)
		rec2.ReleaseView()
		debug.FreeOSMemory()
		t0 = time.Now()
		r2 := NewViewReducer()
		if err := r2.Restore(cp2); err != nil {
			t.Fatal(err)
		}
		restore := time.Since(t0)
		t.Logf("打开路径：LoadRecordForView=%s 解码=%s blocks=%d Restore=%s 合计=%s",
			loadFast.Round(time.Millisecond), decode.Round(time.Millisecond), len(cp2.Blocks),
			restore.Round(time.Millisecond), (loadFast + decode + restore).Round(time.Millisecond))
		return

	case "restore":
		// 恢复路径对照（S1④）：整份 Unmarshal vs 骨架 + 并行块解码。
		debug.FreeOSMemory()
		t0 = time.Now()
		oldCP, oerr := parseCheckpoint(view)
		if oerr != nil {
			t.Fatal(oerr)
		}
		oldDur := time.Since(t0)
		oldBlocks := len(oldCP.Blocks)
		oldIDs := sampleIDs(oldCP.Blocks)
		oldCP = nil
		debug.FreeOSMemory()

		t0 = time.Now()
		newCP, nerr := parseCheckpointFromRecord(rec)
		if nerr != nil {
			t.Fatal(nerr)
		}
		newDur := time.Since(t0)
		newBlocks := len(newCP.Blocks)
		newIDs := sampleIDs(newCP.Blocks)
		t.Logf("恢复解码：整份 Unmarshal=%s → 骨架+并行=%s（%.1fx）blocks %d/%d 抽样一致=%v spans=%d",
			oldDur.Round(time.Millisecond), newDur.Round(time.Millisecond),
			float64(oldDur)/float64(newDur), oldBlocks, newBlocks,
			oldIDs == newIDs, len(rec.LoadedBlockSpans))
		if newBlocks != oldBlocks || oldIDs != newIDs {
			t.Fatalf("快慢路径结果不一致：blocks %d!=%d idsEqual=%v", newBlocks, oldBlocks, oldIDs == newIDs)
		}
		debug.FreeOSMemory()

		r := NewViewReducer()
		t0 = time.Now()
		if err := r.Restore(newCP); err != nil {
			t.Fatal(err)
		}
		t.Logf("Restore = %s", time.Since(t0).Round(time.Millisecond))
		newCP = nil
		debug.FreeOSMemory()
		return

	case "marshal":
		cp, err := parseCheckpoint(view)
		if err != nil {
			t.Fatal(err)
		}
		view, rec = nil, nil // 只保留 Checkpoint，减少一份 210MB
		debug.FreeOSMemory()

		r := NewViewReducer()
		t0 = time.Now()
		if err := r.Restore(cp); err != nil {
			t.Fatal(err)
		}
		t.Logf("Restore = %s", time.Since(t0).Round(time.Millisecond))
		cp = nil
		debug.FreeOSMemory()

		// 冷：blockBytes 缓存为空 → 逐块 json.Marshal（首次提交/唤醒后的第一笔）
		t0 = time.Now()
		cold, err := r.MarshalWithSpans()
		if err != nil {
			t.Fatal(err)
		}
		coldDur := time.Since(t0)

		// 热：seq++ 让 marshal 缓存失效，但 blockBytes 还在 → 稳态提交
		r.mu.Lock()
		r.seq++
		r.mu.Unlock()
		t0 = time.Now()
		warm, err := r.MarshalWithSpans()
		if err != nil {
			t.Fatal(err)
		}
		warmDur := time.Since(t0)
		t.Logf("MarshalWithSpans 冷=%s 热=%s view=%.1fMB spans=%d",
			coldDur.Round(time.Millisecond), warmDur.Round(time.Millisecond),
			float64(len(warm.View))/1e6, len(warm.Spans))

		// S1：只取「骨架 + 各块字节」的 body 路径（不拼 210MB）
		r.mu.Lock()
		r.seq++
		r.mu.Unlock()
		t0 = time.Now()
		bodyCold, err := r.MarshalBodyWithSpans()
		if err != nil {
			t.Fatal(err)
		}
		bodyColdDur := time.Since(t0)
		r.mu.Lock()
		r.seq++
		r.mu.Unlock()
		t0 = time.Now()
		bodyWarm, err := r.MarshalBodyWithSpans()
		if err != nil {
			t.Fatal(err)
		}
		bodyWarmDur := time.Since(t0)
		t.Logf("MarshalBodyWithSpans 冷=%s 热=%s body=%.1fMB spans=%d（S1：不拼 210MB）",
			bodyColdDur.Round(time.Millisecond), bodyWarmDur.Round(time.Millisecond),
			float64(len(bodyWarm.Body))/1e6, len(bodyWarm.Spans))
		_ = bodyCold
		bodyWarm = nil
		debug.FreeOSMemory()

		// 拆开热路径：骨架 marshal / 逐块 marshal / 纯拼接
		var doc Checkpoint
		t0 = time.Now()
		if err := json.Unmarshal(warm.View, &doc); err != nil {
			t.Fatal(err)
		}
		t.Logf("（对照）json.Unmarshal(view→Checkpoint) = %s", time.Since(t0).Round(time.Millisecond))
		doc0 := doc
		blocks := doc0.Blocks
		doc0.Blocks = nil
		t0 = time.Now()
		head, err := json.Marshal(&doc0)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  骨架 json.Marshal = %s (%.1fMB)", time.Since(t0).Round(time.Millisecond), float64(len(head))/1e6)
		doc0.Blocks = blocks
		doc = Checkpoint{}
		runtimeGC()

		t0 = time.Now()
		raw := make([][]byte, len(blocks))
		var total int
		for i := range blocks {
			b, err := json.Marshal(&blocks[i])
			if err != nil {
				t.Fatal(err)
			}
			raw[i] = b
			total += len(b)
		}
		t.Logf("  逐块 json.Marshal = %s (%.1fMB)", time.Since(t0).Round(time.Millisecond), float64(total)/1e6)

		t0 = time.Now()
		var buf bytes.Buffer
		buf.Grow(total + len(raw))
		for i, b := range raw {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(b)
		}
		t.Logf("  纯拼接（memcpy）= %s (%.1fMB)", time.Since(t0).Round(time.Millisecond), float64(buf.Len())/1e6)
		raw, buf, blocks = nil, bytes.Buffer{}, nil
		_ = cold
		cold, warm, r = nil, nil, nil
		debug.FreeOSMemory()
		return

	case "parallel":
		// 并行解码实验：把 view 的 blocks 数组切成每个元素的字节，分给 N 个 worker
		// 各自 json.Unmarshal。restore 路径现在是一次性 210MB 单线程解码（1~2s）。
		raw, ok := splitBlocksField(view)
		if !ok {
			t.Fatal("view 里没有 blocks 数组")
		}
		t0 = time.Now()
		elems, ok := splitArrayElems(raw)
		if !ok {
			t.Fatal("切分 blocks 数组失败")
		}
		splitDur := time.Since(t0)
		t.Logf("切分 blocks 数组 = %s (元素 %d)", splitDur.Round(time.Millisecond), len(elems))

		decodeAll := func(worker int) time.Duration {
			start := time.Now()
			out := make([]Block, len(elems))
			if worker <= 1 {
				for i := range elems {
					if err := json.Unmarshal(elems[i], &out[i]); err != nil {
						t.Fatalf("解码失败: %v", err)
					}
				}
				return time.Since(start)
			}
			var wg sync.WaitGroup
			chunk := (len(elems) + worker - 1) / worker
			var mu sync.Mutex
			var firstErr error
			for w := 0; w < worker; w++ {
				lo := w * chunk
				hi := lo + chunk
				if hi > len(elems) {
					hi = len(elems)
				}
				if lo >= hi {
					continue
				}
				wg.Add(1)
				go func(lo, hi int) {
					defer wg.Done()
					for i := lo; i < hi; i++ {
						if err := json.Unmarshal(elems[i], &out[i]); err != nil {
							mu.Lock()
							if firstErr == nil {
								firstErr = err
							}
							mu.Unlock()
							return
						}
					}
				}(lo, hi)
			}
			wg.Wait()
			if firstErr != nil {
				t.Fatalf("并行解码失败: %v", firstErr)
			}
			return time.Since(start)
		}
		for _, n := range []int{1, 2, 4, 8} {
			d := decodeAll(n)
			t.Logf("blocks 解码 worker=%d → %s", n, d.Round(time.Millisecond))
			debug.FreeOSMemory()
		}
		return

	case "e2e":
		// 端到端回归（真实大会话）：全量解析 vs 映射解析 vs 窗口解析，三者必须
		// 在「块集合、顺序、turn 总数」上完全一致；窗口解析的 decoded ∪ paged
		// 必须等于全集（一块不丢）。
		full, ferr := parseCheckpoint(rec.ViewCheckpoint)
		if ferr != nil {
			t.Fatal(ferr)
		}
		rec2, lerr := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr != nil {
			t.Fatal(lerr)
		}
		fast, perr := parseCheckpointFromRecord(rec2)
		if perr != nil {
			t.Fatal(perr)
		}
		if len(fast.Blocks) != len(full.Blocks) {
			t.Fatalf("块数不一致: full=%d fast=%d", len(full.Blocks), len(fast.Blocks))
		}
		for _, i := range []int{0, len(full.Blocks) / 2, len(full.Blocks) - 1} {
			if fast.Blocks[i].ID != full.Blocks[i].ID || fast.Blocks[i].Kind != full.Blocks[i].Kind {
				t.Fatalf("块 %d 不一致: full=%d/%s fast=%d/%s", i,
					full.Blocks[i].ID, full.Blocks[i].Kind, fast.Blocks[i].ID, fast.Blocks[i].Kind)
			}
		}
		if len(fast.Turns) != len(full.Turns) || fast.TurnsTotal != full.TurnsTotal {
			t.Fatalf("turns 不一致: full=%d/%d fast=%d/%d",
				len(full.Turns), full.TurnsTotal, len(fast.Turns), fast.TurnsTotal)
		}
		t.Logf("全量 vs 映射：blocks=%d turns=%d/%d 一致", len(fast.Blocks), len(fast.Turns), fast.TurnsTotal)

		// 窗口解析：decoded ∪ paged == 全集
		const win = 500
		cpw, werr := parseCheckpointFromRecordWindow(rec2, win)
		if werr != nil {
			t.Fatal(werr)
		}
		union := make(map[uint64]struct{}, len(cpw.Blocks)+len(cpw.PagedBlocks.IDs))
		for _, b := range cpw.Blocks {
			union[b.ID] = struct{}{}
		}
		for _, id := range cpw.PagedBlocks.IDs {
			union[id] = struct{}{}
		}
		if len(union) != len(full.Blocks) {
			t.Fatalf("窗口解析丢了块：union=%d full=%d", len(union), len(full.Blocks))
		}
		for _, b := range full.Blocks {
			if _, ok := union[b.ID]; !ok {
				t.Fatalf("窗口解析缺块 id=%d", b.ID)
			}
		}
		if cpw.BlocksTotal != len(full.Blocks) {
			t.Fatalf("BlocksTotal=%d want %d", cpw.BlocksTotal, len(full.Blocks))
		}
		t.Logf("窗口解析(limit=%d)：解码=%d 分页=%d 合计=%d 全集=%d ✓",
			win, len(cpw.Blocks), len(cpw.PagedBlocks.IDs), len(union), len(full.Blocks))
		rec2.ReleaseView()
		return

	case "turnssplit":
		// turns 分块：从旧 record 恢复 → 编码（body 只带窗口）→ 追加日志 → 提交
		// → 按新格式重开，量真实打开路径。
		cpOld, perr := parseCheckpointFromRecord(rec)
		if perr != nil {
			t.Fatal(perr)
		}
		rr := NewViewReducer()
		if err := rr.Restore(cpOld); err != nil {
			t.Fatal(err)
		}
		spans, serr := rr.MarshalBodyWithSpans()
		if serr != nil {
			t.Fatal(serr)
		}
		body := spans.Body
		var probe struct {
			Turns      []Turn `json:"turns"`
			TurnsTotal int    `json:"turns_total"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			t.Fatal(err)
		}
		recPath, _ := store.RecordPath(wsKey, sid)
		pending := rr.TakePendingTurns()
		rawTurns, _ := json.Marshal(pending)
		t0 = time.Now()
		if err := session.AppendTurnChunk(recPath, rawTurns, len(pending)); err != nil {
			t.Fatal(err)
		}
		logDur := time.Since(t0)
		t.Logf("body=%.1fMB turns窗口=%d turns_total=%d 日志追加=%s(%d 条)",
			float64(len(body))/1e6, len(probe.Turns), probe.TurnsTotal, logDur.Round(time.Millisecond), len(pending))

		r2 := session.NewSessionRecord(wsKey, sid, rec.Revision+1, rec.ReducerSeq, 0, rec.SDKState, body)
		r2.BlockSpans = spans.Spans
		r2.BlockIDs = spans.IDs
		r2.DirtyBlocks = map[uint64]struct{}{}
		ct2, cerr := store.CommitRecordTraced(ctx, r2)
		if cerr != nil {
			t.Fatal(cerr)
		}
		fi, _ := os.Stat(recPath)
		t.Logf("提交: total=%s encode=%s temp=%s record=%.1fMB",
			ct2.Total.Round(time.Millisecond), ct2.Encode.Round(time.Millisecond),
			ct2.Temp.Round(time.Millisecond), float64(fi.Size())/1e6)

		debug.FreeOSMemory()
		t0 = time.Now()
		rec4, lerr4 := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr4 != nil {
			t.Fatal(lerr4)
		}
		load4 := time.Since(t0)
		t0 = time.Now()
		cp4, perr4 := parseCheckpointFromRecord(rec4)
		if perr4 != nil {
			t.Fatal(perr4)
		}
		dec4 := time.Since(t0)
		rec4.ReleaseView()
		t.Logf("新格式重开：LoadRecordForView=%s 解码=%s blocks=%d turns_total=%d",
			load4.Round(time.Millisecond), dec4.Round(time.Millisecond), len(cp4.Blocks), cp4.TurnsTotal)
		return

	case "commit":
		// 生产形态（S1）：body 占位（blocks:null）+ spans + 脏块快路径
		spans, ids := serdeSpans(t, view)
		body := serdeBodyPlaceholder(t, view)
		debug.FreeOSMemory()
		t0 = time.Now()
		r2 := session.NewSessionRecord(wsKey, sid, rec.Revision+1, rec.ReducerSeq, 0, rec.SDKState, body)
		copyDur := time.Since(t0)
		debug.FreeOSMemory()

		r2.BlockSpans = spans
		r2.BlockIDs = ids
		r2.DirtyBlocks = map[uint64]struct{}{}
		ct, err := store.CommitRecordTraced(ctx, r2)
		if err != nil {
			t.Fatal(err)
		}
		// 读回校验：body 形态落盘后必须能完整还原
		back, lerr := store.LoadRecord(ctx, wsKey, sid)
		if lerr != nil {
			t.Fatalf("body 形态落盘后读回失败: %v", lerr)
		}
		backSpans, _ := serdeSpans(t, back.ViewCheckpoint)
		if len(backSpans) != len(spans) {
			t.Fatalf("读回块数 %d != %d", len(backSpans), len(spans))
		}
		t.Logf("读回校验: rev=%d blocks=%d（一致）", back.Revision, len(backSpans))

		t.Logf("NewSessionRecord(拷贝 %.1fMB)=%s | CommitRecord=%s [lock=%s sidecar=%s(open=%s ref=%s flush=%s) encode=%s baseline=%s temp=%s hashed=%.1fMB blocks=%d]",
			float64(len(body))/1e6, copyDur.Round(time.Millisecond), ct.Total.Round(time.Millisecond),
			ct.LockWait.Round(time.Millisecond), ct.Sidecar.Round(time.Millisecond),
			ct.SidecarOpen.Round(time.Millisecond), ct.SidecarRef.Round(time.Millisecond), ct.SidecarFlush.Round(time.Millisecond),
			ct.Encode.Round(time.Millisecond), ct.Baseline.Round(time.Millisecond), ct.Temp.Round(time.Millisecond),
			float64(ct.HashedBytes)/1e6, ct.BlocksWrote)

		// 提交后按新格式（turns 只带窗口）重开，量一次真实的打开路径。
		debug.FreeOSMemory()
		t0 = time.Now()
		rec3, lerr3 := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr3 != nil {
			t.Fatal(lerr3)
		}
		load3 := time.Since(t0)
		t0 = time.Now()
		cp3, perr3 := parseCheckpointFromRecord(rec3)
		if perr3 != nil {
			t.Fatal(perr3)
		}
		dec3 := time.Since(t0)
		rec3.ReleaseView()
		t.Logf("提交后重开：LoadRecordForView=%s 解码=%s blocks=%d record=%.1fMB",
			load3.Round(time.Millisecond), dec3.Round(time.Millisecond), len(cp3.Blocks),
			func() float64 {
				if fi, e := os.Stat(filepath.Join(root, wsKey, sid+".record.json")); e == nil {
					return float64(fi.Size()) / 1e6
				}
				return 0
			}())
		return
	}
	t.Fatalf("未知阶段 %q", phase)
}

// serdeClone 用 APFS clone 把会话文件复制到临时目录。
func serdeClone(t *testing.T, recordPath string) (root, wsKey, sid string) {
	t.Helper()
	abs, err := filepath.Abs(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(abs)
	sid = base[:len(base)-len(".record.json")]
	srcDir := filepath.Dir(abs)
	wsKey = filepath.Base(srcDir)
	root = t.TempDir()
	dstDir := filepath.Join(root, wsKey)
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".json", ".json.bak", ".json.blocks", ".json.blocks.idx"} {
		s := filepath.Join(srcDir, sid+".record"+suffix)
		if _, err := os.Stat(s); err != nil {
			continue
		}
		out, cerr := exec.Command("cp", "-c", s, filepath.Join(dstDir, sid+".record"+suffix)).CombinedOutput()
		if cerr != nil {
			t.Fatalf("cp -c %s: %v (%s)", filepath.Base(s), cerr, out)
		}
	}
	return root, wsKey, sid
}

// serdeSpans 从 view 里切出 blocks 的字节与 id（等价于 reducer 的产出）。
func serdeSpans(t *testing.T, view []byte) ([][]byte, []uint64) {
	t.Helper()
	var doc struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(view, &doc); err != nil {
		t.Fatalf("解析 blocks: %v", err)
	}
	spans := make([][]byte, len(doc.Blocks))
	ids := make([]uint64, len(doc.Blocks))
	for i, raw := range doc.Blocks {
		spans[i] = raw
		var b struct {
			ID uint64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("解析 block id: %v", err)
		}
		ids[i] = b.ID
	}
	return spans, ids
}

func runtimeGC() { debug.FreeOSMemory() }

// splitBlocksField 取出 view 里 "blocks":[...] 的数组原文。
func splitBlocksField(view []byte) ([]byte, bool) {
	const key = `"blocks":`
	i := bytes.Index(view, []byte(key))
	if i < 0 {
		return nil, false
	}
	start := i + len(key)
	for start < len(view) && (view[start] == ' ' || view[start] == '\n' || view[start] == '\t' || view[start] == '\r') {
		start++
	}
	if start >= len(view) || view[start] != '[' {
		return nil, false
	}
	depth := 0
	inStr := false
	esc := false
	for j := start; j < len(view); j++ {
		c := view[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return view[start : j+1], true
			}
		}
	}
	return nil, false
}

// splitArrayElems 把 JSON 数组切成每个元素的原始字节。
func splitArrayElems(raw []byte) ([][]byte, bool) {
	t := bytes.TrimSpace(raw)
	if len(t) < 2 || t[0] != '[' || t[len(t)-1] != ']' {
		return nil, false
	}
	body := t[1 : len(t)-1]
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, true
	}
	var out [][]byte
	depth := 0
	inStr := false
	esc := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, bytes.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, bytes.TrimSpace(body[start:]))
	return out, true
}

// serdeBodyPlaceholder 把 view 的 blocks 数组换成 null 占位（S1 提交路径的形态）。
func serdeBodyPlaceholder(t *testing.T, view []byte) []byte {
	t.Helper()
	arr, ok := splitBlocksField(view)
	if !ok {
		t.Fatal("view 里没有 blocks 数组")
	}
	start := bytes.Index(view, arr)
	if start < 0 {
		t.Fatal("blocks 数组定位失败")
	}
	out := make([]byte, 0, len(view)-len(arr)+4)
	out = append(out, view[:start]...)
	out = append(out, 'n', 'u', 'l', 'l')
	out = append(out, view[start+len(arr):]...)
	return out
}

// sampleIDs 给一组块做轻量指纹（顺序 + 抽样 id/kind/文本长度）。
//
// 用途：对照「整份 Unmarshal」与「骨架+并行」两条解码路径是否等价。逐字段
// DeepEqual 210MB 只会把测量变成内存带宽测试，抽样足够抓住顺序错位、
// 截断、字段丢失这三类真实错误。
func sampleIDs(blocks []Block) string {
	if len(blocks) == 0 {
		return "empty"
	}
	var b strings.Builder
	for _, i := range []int{0, len(blocks) / 3, len(blocks) / 2, len(blocks) * 2 / 3, len(blocks) - 1} {
		blk := blocks[i]
		fmt.Fprintf(&b, "%d:%d:%s:%d:%d|", i, blk.ID, blk.Kind, len(blk.Text), len(blk.Thinking))
	}
	fmt.Fprintf(&b, "n=%d", len(blocks))
	return b.String()
}
