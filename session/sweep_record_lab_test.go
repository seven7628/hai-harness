package session

// record 瘦身清扫的实验台（2026-11-11，CHECKPOINT_V3_DESIGN §6 P3-1）。
//
// 在**副本**上量「P3-1 到底省了多少盘」——生产数据一律不动。
//
// 跑法：
//
//	cp -c -R ~/.go-code/sessions/<wsKey> /tmp/rsweep/     # APFS 零拷贝克隆
//	HAI_LAB_SWEEP_ROOT=/tmp/rsweep go test ./session/ -run TestRecordSweepLab -v -timeout 30m

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const labSweepRootEnv = "HAI_LAB_SWEEP_ROOT"

// TestRecordSweepLab 在副本上跑一遍 record 瘦身清扫，量前后体积。
func TestRecordSweepLab(t *testing.T) {
	root := os.Getenv(labSweepRootEnv)
	if root == "" {
		t.Skip("需要 HAI_LAB_SWEEP_ROOT=<副本目录>")
	}
	before := recordBytes(t, root)
	t.Logf("清扫前：%d 个文件，%.1f MB", before.files, float64(before.bytes)/(1<<20))

	start := time.Now()
	res, err := SweepRecordFiles(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := recordBytes(t, root)

	t.Logf("清扫后：%d 个文件，%.1f MB（扫过 %d，瘦身 %d）",
		after.files, float64(after.bytes)/(1<<20), res.Files, res.Stripped)
	t.Logf("净省 %.1f MB，耗时 %v",
		float64(before.bytes-after.bytes)/(1<<20), time.Since(start))
}

// recordBytes 统计目录下的 record 文件总字节。
func recordBytes(t *testing.T, root string) struct {
	files int
	bytes int64
} {
	t.Helper()
	var out struct {
		files int
		bytes int64
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".record.json") && !strings.HasSuffix(name, ".record.json.bak") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out.files++
		out.bytes += fi.Size()
	}
	return out
}

// TestRecordSweepVerify 验证清扫后的 record 与**原件**内容逐块一致。
//
// 这是清扫的安全底线：搬进 sidecar 之后，块内容、顺序、id 都不能变。
// 做法：拿副本里的（已清扫）与 ~/.go-code 里的（原件）逐个比对。
//
// 跑法：
//
//	HAI_LAB_VERIFY_CLONE=/tmp/rsweep3/<wsKey> \
//	HAI_LAB_VERIFY_ORIG=$HOME/.go-code/sessions/<wsKey> \
//	go test ./session/ -run TestRecordSweepVerify -v -timeout 30m
const (
	labVerifyCloneEnv = "HAI_LAB_VERIFY_CLONE"
	labVerifyOrigEnv  = "HAI_LAB_VERIFY_ORIG"
)

// TestRecordSweepVerify 逐个比对「清扫后的副本」与「原件」。
func TestRecordSweepVerify(t *testing.T) {
	cloneRoot := os.Getenv(labVerifyCloneEnv)
	origRoot := os.Getenv(labVerifyOrigEnv)
	if cloneRoot == "" || origRoot == "" {
		t.Skip("需要 HAI_LAB_VERIFY_CLONE + HAI_LAB_VERIFY_ORIG")
	}
	entries, err := os.ReadDir(cloneRoot)
	if err != nil {
		t.Fatal(err)
	}
	var checked, failed int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".record.json") {
			continue
		}
		clonePath := filepath.Join(cloneRoot, e.Name())
		origPath := filepath.Join(origRoot, e.Name())
		if _, err := os.Stat(origPath); err != nil {
			continue
		}
		cloneBlocks, cerr := loadRecordBlocks(clonePath)
		if cerr != nil {
			t.Errorf("%s: 清扫后读不出来: %v", e.Name(), cerr)
			failed++
			continue
		}
		origBlocks, oerr := loadRecordBlocks(origPath)
		if oerr != nil {
			t.Errorf("%s: 原件读不出来: %v", e.Name(), oerr)
			failed++
			continue
		}
		if len(cloneBlocks) != len(origBlocks) {
			t.Errorf("%s: 块数不一致 %d vs %d", e.Name(), len(cloneBlocks), len(origBlocks))
			failed++
			continue
		}
		for i := range cloneBlocks {
			if string(cloneBlocks[i]) != string(origBlocks[i]) {
				t.Errorf("%s: 第 %d 块内容不一致（%d vs %d 字节）",
					e.Name(), i, len(cloneBlocks[i]), len(origBlocks[i]))
				failed++
				break
			}
		}
		checked++
	}
	t.Logf("比对 %d 个 record，不一致 %d 个", checked, failed)
	if failed > 0 {
		t.Fatalf("有 %d 个 record 清扫后内容不一致", failed)
	}
}

// loadRecordBlocks 读一个 record 的全部块字节（按 view 里的顺序）。
func loadRecordBlocks(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec SessionRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	// 引用形态要解析成完整数组（清扫后的 record 就是这个形态）。
	view, _, err := resolveBlocksRefSpans(rec.ViewCheckpoint, path)
	if err != nil {
		return nil, err
	}
	elems, ok := findBlocksField(view)
	if !ok {
		return nil, nil
	}
	return elems, nil
}

// TestCombinedSweepLab 一次跑完两个清扫（会话文件 + record），量合计收益。
func TestCombinedSweepLab(t *testing.T) {
	root := os.Getenv("HAI_LAB_SWEEP_ROOT")
	if root == "" {
		t.Skip("need root")
	}
	t0 := time.Now()
	r1, err := SweepSessionFiles(root, 0, DefaultCompactKeepStates, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("会话文件清扫: %d 个文件（%d 个压缩）%.1fMB → %.1fMB，省 %.1fMB，用时 %s",
		r1.Files, r1.Compacted, float64(r1.Before)/1e6, float64(r1.After)/1e6, float64(r1.Saved())/1e6, time.Since(t0).Round(time.Millisecond))
	t1 := time.Now()
	r2, err := SweepRecordFiles(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("record 清扫: %d 个（%d 个搬进 sidecar）%.1fMB → %.1fMB，省 %.1fMB，用时 %s",
		r2.Stripped, r2.Migrated, float64(r2.Before)/1e6, float64(r2.After)/1e6, float64(r2.Saved())/1e6, time.Since(t1).Round(time.Millisecond))
}
