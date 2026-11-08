package session

// 提交路径的规模实验（默认跳过；需要真实会话数据时显式开启）。
//
// 用途：回答「为什么 checkpoint 提交会超 30s」以及「侧车里的垃圾是怎么来的」。
// 它不改动真实数据：先把会话文件用 APFS clone（cp -c，零拷贝、零额外空间）
// 复制到临时目录，再在副本上重复提交。
//
// 跑法：
//
//	HAI_LAB_SESSION=$HOME/.go-code/sessions/<ws>/<sid>.record.json \
//	  go test ./session/ -run TestCheckpointScalingLab -v -timeout 30m
//
// 输出四组数字：
//  1. 索引命中 / 索引丢失两种打开成本（隔离出「全量重建扫描」这一档）；
//  2. 稳态提交的阶段分解（view/record/块数 + 每段耗时）；
//  3. 单个 running block 反复重写产生的追加放大（垃圾来源）；
//  4. N 路并发提交经同一个 FileStorage 时的 lock 等待（30s 报错的放大器）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const labSessionEnv = "HAI_LAB_SESSION"

// labSession 描述一个真实会话文件集合。
type labSession struct {
	record string // 绝对路径 .../<sid>.record.json
	dir    string
	wsKey  string
	sid    string
}

func labLoad(t *testing.T) labSession {
	t.Helper()
	path := os.Getenv(labSessionEnv)
	if path == "" {
		t.Skipf("设置 %s=<sid>.record.json 后运行规模实验", labSessionEnv)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(abs)
	if filepath.Ext(base) != ".json" {
		t.Fatalf("不是 record.json: %s", base)
	}
	sid := base[:len(base)-len(".record.json")]
	return labSession{record: abs, dir: filepath.Dir(abs), wsKey: filepath.Base(filepath.Dir(abs)), sid: sid}
}

// labClone 用 APFS clone 把会话文件复制到 dstRoot/<wsKey>/，可改名成 newSID。
// cp -c 走 clonefile：不占额外空间，写时复制 —— 因此可以放心在副本上提交。
func labClone(t *testing.T, src labSession, dstRoot, newSID string) (labSession, error) {
	t.Helper()
	if newSID == "" {
		newSID = src.sid
	}
	dstDir := filepath.Join(dstRoot, src.wsKey)
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return labSession{}, err
	}
	for _, suffix := range []string{".json", ".json.bak", ".json.blocks", ".json.blocks.idx"} {
		s := filepath.Join(src.dir, src.sid+".record"+suffix)
		if _, err := os.Stat(s); err != nil {
			continue
		}
		d := filepath.Join(dstDir, newSID+".record"+suffix)
		out, cerr := exec.Command("cp", "-c", s, d).CombinedOutput()
		if cerr != nil {
			return labSession{}, fmt.Errorf("cp -c %s: %w (%s)", filepath.Base(s), cerr, out)
		}
	}
	return labSession{record: filepath.Join(dstDir, newSID+".record.json"), dir: dstDir, wsKey: src.wsKey, sid: newSID}, nil
}

// labSpans 从展开后的 view 里切出每个 block 的字节（等价于 reducer 的 spans）。
func labSpans(t *testing.T, view []byte) [][]byte {
	t.Helper()
	raw, ok := splitViewBlocks(view)
	if !ok {
		t.Fatalf("view 没有 blocks 字段")
	}
	spans, ok := splitBlocksArray(raw)
	if !ok {
		t.Fatalf("blocks 不是数组")
	}
	return spans
}

func TestCheckpointScalingLab(t *testing.T) {
	src := labLoad(t)

	t.Run("open-index-hit", func(t *testing.T) {
		root := t.TempDir()
		dst, err := labClone(t, src, root, "")
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewSessionRecordStorage(root)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rec, err := store.LoadRecord(context.Background(), dst.wsKey, dst.sid)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("LoadRecord(索引命中): %s  view=%dMB", time.Since(start).Round(time.Millisecond), len(rec.ViewCheckpoint)/1e6)
	})

	t.Run("open-index-lost-rebuild", func(t *testing.T) {
		root := t.TempDir()
		dst, err := labClone(t, src, root, "")
		if err != nil {
			t.Fatal(err)
		}
		sidecar := dst.record + ".blocks"
		// 索引丢失 = 崩溃/compact 中断/索引超前 后的真实路径。
		if err := os.Remove(sidecar + ".idx"); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(sidecar)
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewSessionRecordStorage(root)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rec, err := store.LoadRecord(context.Background(), dst.wsKey, dst.sid)
		el := time.Since(start)
		if err != nil {
			t.Fatalf("重建失败: %v", err)
		}
		t.Logf("LoadRecord(索引丢失→全量重建): %s  sidecar=%.2fGB view=%dMB",
			el.Round(time.Millisecond), float64(fi.Size())/1e9, len(rec.ViewCheckpoint)/1e6)
	})

	root := t.TempDir()
	dst, err := labClone(t, src, root, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec, err := store.LoadRecord(ctx, dst.wsKey, dst.sid)
	if err != nil {
		t.Fatal(err)
	}
	view := rec.ViewCheckpoint
	spans := labSpans(t, view)
	var liveBytes int
	for _, s := range spans {
		liveBytes += len(s)
	}
	t.Logf("会话规模: view=%dMB  blocks=%d  有效块=%dMB  record=%dMB",
		len(view)/1e6, len(spans), liveBytes/1e6, recSize(dst.record)/1e6)

	t.Run("commit-steady-state", func(t *testing.T) {
		rev := rec.Revision
		var totalDur time.Duration
		const n = 3
		for i := 0; i < n; i++ {
			rev++
			r := NewSessionRecord(dst.wsKey, dst.sid, rev, rec.ReducerSeq+uint64(i), 0, rec.SDKState, view)
			r.BlockSpans = spans
			ct, err := store.CommitRecordTraced(ctx, r)
			if err != nil {
				t.Fatalf("提交失败: %v", err)
			}
			totalDur += ct.Total
			t.Logf("commit#%d total=%-8s view=%.1fMB record=%.1fMB blocks=%d | lock=%s sidecar=%s(compact=%s) encode=%s baseline=%s temp=%s bak=%s rename+dir=%s",
				i, ct.Total.Round(time.Millisecond), float64(ct.ViewBytes)/1e6, float64(ct.RecordBytes)/1e6, ct.BlocksWrote,
				ct.LockWait.Round(time.Millisecond), ct.Sidecar.Round(time.Millisecond), ct.Compact.Round(time.Millisecond),
				ct.Encode.Round(time.Millisecond), ct.Baseline.Round(time.Millisecond), ct.Temp.Round(time.Millisecond),
				ct.Backup.Round(time.Millisecond), (ct.Rename + ct.SyncDir).Round(time.Millisecond))
		}
		t.Logf("稳态均值: %s/commit", (totalDur / n).Round(time.Millisecond))

		// 自洽性：刚写进去的 record 必须能被自己读回来（引用里带 ids 之后，
		// 校验只针对「我引用的块」，不再要求与 sidecar 全集相等）。
		back, lerr := store.LoadRecord(ctx, dst.wsKey, dst.sid)
		if lerr != nil {
			t.Fatalf("提交后读回失败: %v", lerr)
		}
		if got := len(labSpans(t, back.ViewCheckpoint)); got != len(spans) {
			t.Fatalf("读回块数 %d != 提交块数 %d", got, len(spans))
		}
		raw := readFile(t, dst.record)
		head := raw
		if len(head) > 1<<20 {
			head = head[:1<<20]
		}
		hasIDs := strings.Contains(string(head), `"ids":[`)
		t.Logf("读回校验: rev=%d blocks=%d record=%dMB 引用含 ids=%v",
			back.Revision, len(spans), len(raw)/1e6, hasIDs)
	})

	t.Run("commit-growing-block-amplification", func(t *testing.T) {
		// 模拟「子智能体卡片在 running 期每个事件都被整块重写」：
		// 每次给一个 block 追加 8KB 文本，其余块不变。
		// 挑一个接近 200KB 的块：真实会话里 running 卡片就是这个量级，
		// 且不会让实验本身写掉几十 GB。
		biggest := 0
		bestDelta := 1 << 62
		for i, s := range spans {
			d := len(s) - 200*1024
			if d < 0 {
				d = -d
			}
			if d < bestDelta {
				bestDelta, biggest = d, i
			}
		}
		base := append([]byte(nil), spans[biggest]...)
		work := append([][]byte(nil), spans...)
		sidecar := dst.record + ".blocks"
		before := fileSize(t, sidecar)
		rev := rec.Revision + 100
		const events = 40
		payload := make([]byte, 8*1024)
		for i := range payload {
			payload[i] = byte('a' + i%26)
		}
		start := time.Now()
		for i := 0; i < events; i++ {
			base = growBlock(t, base, payload)
			work[biggest] = base
			rev++
			grownView, gerr := replaceJSONField(view, "blocks", joinSpans(t, work))
			if gerr != nil {
				t.Fatalf("重建 view: %v", gerr)
			}
			r := NewSessionRecord(dst.wsKey, dst.sid, rev, rec.ReducerSeq+uint64(1000+i), 0, rec.SDKState, grownView)
			r.BlockSpans = work
			if _, err := store.CommitRecordTraced(ctx, r); err != nil {
				t.Fatalf("提交失败: %v", err)
			}
		}
		el := time.Since(start)
		grown := fileSize(t, sidecar) - before
		t.Logf("running 卡片 %d 个事件: sidecar +%.1fMB（有效增量仅 %.1fMB）, 平均 %.1fms/事件, 放大 %.1fx",
			events, float64(grown)/1e6, float64(events*len(payload))/1e6,
			float64(el.Milliseconds())/events, float64(grown)/float64(events*len(payload)))
	})

	t.Run("commit-concurrent-lock", func(t *testing.T) {
		for _, n := range []int{1, 2, 3} {
			rootN := t.TempDir()
			stores := make([]*FileStorage, 0, n)
			recs := make([]*SessionRecord, 0, n)
			st, err := NewSessionRecordStorage(rootN)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				// 同一 storage 下 N 个独立会话 —— 生产形态：一个 bridge 进程里
				// 多个 workspace 同时提交，共用一把 FileStorage.mu。
				// 文件名不能改（record identity 与路径绑定），所以用新 sid 造 record。
				sid := fmt.Sprintf("%s-c%d", src.sid, i)
				r2 := NewSessionRecord(src.wsKey, sid, rec.Revision, rec.ReducerSeq, 0, rec.SDKState, view)
				// 预热：首提交会把全部块写进新 sidecar（≈185MB），不计入并发段。
				if _, err := st.CommitRecordTraced(ctx, r2); err != nil {
					t.Fatal(err)
				}
				stores = append(stores, st)
				recs = append(recs, r2)
			}
			var mu sync.Mutex
			var waits []time.Duration
			var totals []time.Duration
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					sp := labSpans(t, recs[i].ViewCheckpoint)
					for k := 0; k < 3; k++ {
						r := NewSessionRecord(recs[i].WorkspaceKey, recs[i].SessionID, recs[i].Revision+uint64(k+1), recs[i].ReducerSeq, 0, recs[i].SDKState, recs[i].ViewCheckpoint)
						r.BlockSpans = sp
						ct, err := stores[i].CommitRecordTraced(ctx, r)
						if err != nil {
							t.Errorf("并发提交失败: %v", err)
							return
						}
						mu.Lock()
						waits = append(waits, ct.LockWait)
						totals = append(totals, ct.Total)
						mu.Unlock()
					}
				}(i)
			}
			wg.Wait()
			t.Logf("并发 %d 路: commit p50=%s p95=%s max=%s | lock p50=%s max=%s",
				n, pct(totals, 0.5), pct(totals, 0.95), maxDur(totals), pct(waits, 0.5), maxDur(waits))
		}
	})
}

// ---- 小工具 ----

func recSize(path string) int64 { return fileSize(nil, path) }

func fileSize(t *testing.T, path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		if t != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		return 0
	}
	return fi.Size()
}

// growBlock 给一个 block 追加文本：模拟 running 块的 items/usage 增长。
func growBlock(t *testing.T, block, add []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(block, &m); err != nil {
		t.Fatalf("block 不是 JSON 对象: %v", err)
	}
	text, _ := m["text"].(string)
	m["text"] = text + string(add)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("重新编码 block: %v", err)
	}
	return out
}

// joinSpans 把 block 字节数组拼回合法的 blocks JSON 数组。
func joinSpans(t *testing.T, spans [][]byte) []byte {
	t.Helper()
	out := make([]byte, 0, 1<<20)
	out = append(out, '[')
	for i, s := range spans {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, s...)
	}
	out = append(out, ']')
	return out
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i].Round(time.Millisecond)
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m.Round(time.Millisecond)
}

// TestCheckpointRebuildAndCompactAtScale 测量「索引丢失后的全量重建」与
// 「compact 重写」这两条 stop-the-world 路径在真实体量下的耗时。
//
// 为什么单独测：两者都发生在提交/打开的热路径里，且都持有 FileStorage.mu
// （= 进程内所有会话串行）。它们是 30s 超时最可能的单一来源。
//
//	HAI_LAB_SESSION=... HAI_LAB_INFLATE_MB=3200 \
//	  go test ./session/ -run TestCheckpointRebuildAndCompactAtScale -v -timeout 30m
func TestCheckpointRebuildAndCompactAtScale(t *testing.T) {
	src := labLoad(t)
	// 默认 0 = 不膨胀，直接量现有 sidecar（用 APFS clone，零额外空间）。
	// 磁盘紧张时不要用 HAI_LAB_INFLATE_MB；现成的大会话已经够做规模曲线。
	var target int64
	if v := os.Getenv("HAI_LAB_INFLATE_MB"); v != "" {
		var mb int64
		if _, err := fmt.Sscan(v, &mb); err == nil && mb > 0 {
			target = mb
		}
	}
	root := t.TempDir()
	dst, err := labClone(t, src, root, "")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := dst.record + ".blocks"
	if target > 0 {
		if err := inflateSidecar(t, sidecar, target<<20); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(sidecar + ".idx"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	size := fileSize(t, sidecar)
	t.Logf("实验体: sidecar=%.2fGB（无索引，记录重复 → 大量废弃）", float64(size)/1e9)

	start := time.Now()
	bs, err := openBlockStore(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	openDur := time.Since(start)
	t.Logf("openBlockStore(全量重建索引): %s  唯一块=%d", openDur.Round(time.Millisecond), bs.Len())

	if os.Getenv("HAI_LAB_COMPACT") == "1" {
		start = time.Now()
		if err := bs.compact(); err != nil {
			t.Fatalf("compact: %v", err)
		}
		t.Logf("compact(重写): %s  %.2fGB → %.2fGB（%.1fx）",
			time.Since(start).Round(time.Millisecond), float64(size)/1e9,
			float64(fileSize(t, sidecar))/1e9, float64(size)/float64(fileSize(t, sidecar)))
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
}

// inflateSidecar 把 sidecar 原地膨胀到 targetBytes：重复追加已有记录。
// 结果是一份「记录条数与字节数严重失衡」的文件，正好复刻真实会话的形态。
func inflateSidecar(t *testing.T, path string, targetBytes int64) error {
	t.Helper()
	cur := fileSize(t, path)
	if cur >= targetBytes {
		return nil
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	for cur < targetBytes {
		if _, err := src.Seek(8, io.SeekStart); err != nil { // 跳过 magic
			return err
		}
		n, cerr := io.Copy(dst, src)
		if cerr != nil {
			return cerr
		}
		if n == 0 {
			return fmt.Errorf("inflate 没有进展")
		}
		cur += n
	}
	return nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCheckpointHeadOfLineBlocking 量化「跨会话队头阻塞」。
//
// 生产形态：一个 bridge 进程里所有 workspace 的会话共用**同一个**
// FileStorage.mu（CommetRecord 全程持锁），而 LoadRecord 也持同一把锁。
// 于是「切会话/恢复」这种上秒级的读操作，会把**别的会话**的提交堵在锁外，
// 堵的时间全额算在它们自己的 30s 预算里 —— 这才是「小会话也报 30s 超时」
// 的原因：不是它自己慢，是排在别人后面。
func TestCheckpointHeadOfLineBlocking(t *testing.T) {
	src := labLoad(t)
	root := t.TempDir()
	blocker, err := labClone(t, src, root, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec, err := store.LoadRecord(ctx, blocker.wsKey, blocker.sid)
	if err != nil {
		t.Fatal(err)
	}
	spans := labSpans(t, rec.ViewCheckpoint)

	// 小会话：100 个块，提交成本可忽略 → 测到的 LockWait 全是排队时间。
	smallBlocks := make([]string, 0, 100)
	for i := 1; i <= 100; i++ {
		smallBlocks = append(smallBlocks, testBlock(i, "small"))
	}
	smallView := testView(smallBlocks...)
	smallSID := src.sid + "-hol"
	if err := store.CommitRecord(ctx, NewSessionRecord(src.wsKey, smallSID, 1, 1, 0, State{}, smallView)); err != nil {
		t.Fatal(err)
	}

	// 阻塞者：一次 LoadRecord（真实体量）+ 一次大提交。
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.LoadRecord(ctx, blocker.wsKey, blocker.sid)
	}()
	time.Sleep(5 * time.Millisecond) // 让它先拿到锁
	var lockWaits []time.Duration
	var totals []time.Duration
	for i := 0; i < 3; i++ {
		ct, cerr := store.CommitRecordTraced(ctx, NewSessionRecord(src.wsKey, smallSID, uint64(i+2), uint64(i+2), 0, State{}, smallView))
		if cerr != nil {
			t.Fatalf("小会话提交失败: %v", cerr)
		}
		lockWaits = append(lockWaits, ct.LockWait)
		totals = append(totals, ct.Total)
	}
	<-done
	t.Logf("邻座 LoadRecord 期间，小会话提交: 总耗时 %s，其中等锁 %s（阻塞者全程 %s）",
		maxDur(totals), maxDur(lockWaits), time.Since(start).Round(time.Millisecond))
	_ = spans
}
