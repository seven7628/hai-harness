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
	t0 := time.Now()
	rec, err := store.LoadRecord(ctx, wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	loadDur := time.Since(t0)
	view := rec.ViewCheckpoint
	t.Logf("[%s] 前置 LoadRecord=%s view=%.1fMB spans=%d body=%.1fMB ref=%v", phase,
		loadDur.Round(time.Millisecond), float64(len(view))/1e6,
		len(rec.LoadedBlockSpans), float64(len(rec.BodyView))/1e6,
		bytes.Contains(rec.BodyView, []byte("$blocks_ref")))

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
