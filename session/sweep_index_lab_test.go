package session

// 清扫索引的实验台（2026-11-12）。
//
// 在**副本**上量「加索引之后，启动清扫到底省了多少」。
//
// 跑法：
//
//	cp -c -R ~/.go-code/sessions /tmp/sweepidx-lab/     # APFS 零拷贝克隆
//	HAI_LAB_SWEEP_ROOT=/tmp/sweepidx-lab go test ./session/ -run TestSweepIndexLab -v -timeout 60m
//
// 三轮：
//
//	第 1 轮（冷）  带索引、索引为空      → 今天的成本
//	第 2 轮（热）  带索引、索引已落盘    → 索引生效后的成本
//	第 3 轮（对照）不带索引              → 证明差异来自索引而不是「数据已经干净」

import (
	"os"
	"testing"
	"time"
)

const labSweepIndexRootEnv = "HAI_LAB_SWEEP_ROOT"

// TestSweepIndexLab 三轮清扫的耗时对比。
func TestSweepIndexLab(t *testing.T) {
	root := os.Getenv(labSweepIndexRootEnv)
	if root == "" {
		t.Skip("需要 HAI_LAB_SWEEP_ROOT=<副本目录>")
	}
	idxPath := root + "/../sweep_index_lab.json"

	// ① 冷：索引为空。
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	cold := runSweepLab(t, root, ix)
	t.Logf("① 冷（索引为空）      总耗时 %v", cold.total)
	t.Logf("   jsonl   处理 %d 跳过 %d  用时 %v", cold.jsonl.Files, cold.jsonl.Skipped, cold.jsonl.Elapsed)
	t.Logf("   sidecar 处理 %d 跳过 %d  用时 %v", cold.blocks.Files, cold.blocks.Skipped, cold.blocks.Elapsed)
	t.Logf("   record  处理 %d 跳过 %d  用时 %v", cold.rec.Files, cold.rec.Skipped, cold.rec.Elapsed)
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}
	entries, seen := ix.Stats()
	t.Logf("   索引条目 %d（seen %d）", entries, seen)

	// ② 热：索引已落盘。
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	warm := runSweepLab(t, root, ix2)
	t.Logf("② 热（索引已落盘）    总耗时 %v", warm.total)
	t.Logf("   jsonl   处理 %d 跳过 %d  用时 %v", warm.jsonl.Files, warm.jsonl.Skipped, warm.jsonl.Elapsed)
	t.Logf("   sidecar 处理 %d 跳过 %d  用时 %v", warm.blocks.Files, warm.blocks.Skipped, warm.blocks.Elapsed)
	t.Logf("   record  处理 %d 跳过 %d  用时 %v", warm.rec.Files, warm.rec.Skipped, warm.rec.Elapsed)

	// ③ 对照：同样的数据、不带索引。
	ctrl := runSweepLab(t, root, nil)
	t.Logf("③ 对照（不带索引）    总耗时 %v", ctrl.total)
	t.Logf("   jsonl   处理 %d 跳过 %d  用时 %v", ctrl.jsonl.Files, ctrl.jsonl.Skipped, ctrl.jsonl.Elapsed)
	t.Logf("   sidecar 处理 %d 跳过 %d  用时 %v", ctrl.blocks.Files, ctrl.blocks.Skipped, ctrl.blocks.Elapsed)
	t.Logf("   record  处理 %d 跳过 %d  用时 %v", ctrl.rec.Files, ctrl.rec.Skipped, ctrl.rec.Elapsed)

	if warm.total > 0 {
		t.Logf("→ 索引让启动清扫从 %v 降到 %v（%.1f×）", ctrl.total, warm.total, float64(ctrl.total)/float64(warm.total))
	}
}

// sweepLabResult 是三轮实验共用的结果容器。
type sweepLabResult struct {
	total  time.Duration
	jsonl  SweepResult
	blocks CompressSweepResult
	rec    RecordSweepResult
}

// runSweepLab 跑一遍完整的三步清扫（用生产同款的参数：4MB 阈值 / 100ms 限速）。
func runSweepLab(t *testing.T, root string, ix *SweepIndex) sweepLabResult {
	t.Helper()
	var out sweepLabResult
	start := time.Now()

	j, err := SweepSessionFiles(root, 4<<20, DefaultCompactKeepStates, false, 100*time.Millisecond, ix)
	if err != nil {
		t.Fatal(err)
	}
	out.jsonl = j

	b, err := SweepBlockStores(root, 100*time.Millisecond, ix)
	if err != nil {
		t.Fatal(err)
	}
	out.blocks = b

	r, err := SweepRecordFiles(root, 100*time.Millisecond, ix)
	if err != nil {
		t.Fatal(err)
	}
	out.rec = r

	out.total = time.Since(start)
	return out
}
