package main

// 事件流重放实验台（2026-11-11，CHECKPOINT_V3_DESIGN §6 P3/P4）。
//
// ## 要回答的问题
//
// P3 的提案是「`.blocks` 停写，UI 视图从事件流投影」。这条路的成立条件有两条：
//
//	① 事件流**够全**——重放出来的视图和 checkpoint 里的视图一致；
//	② 重放**够快**——否则打开会话从 2~3.4s 变成 10s+，比现在还差。
//
// 实测台：把 ~/.go-code/events/<ws>/<sid>.jsonl 整份喂给 reducer，
// 量时间、块数，并与同会话的 record（checkpoint 真值）逐块比对。
//
// 跑法：
//
//	HAI_LAB_EVENTS=$HOME/.go-code/events/<ws>/<sid>.jsonl \
//	HAI_LAB_RECORD=$HOME/.go-code/sessions/<ws>/<sid>.record.json \
//	go test ./desktop/bridge/ -run TestReplayLab -v -timeout 30m

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	labEventsEnv = "HAI_LAB_EVENTS"
	labRecordEnv = "HAI_LAB_RECORD"
)

// TestReplayLab 全量重放真实事件日志，量时间/块数/内存，并与 checkpoint 比对。
func TestReplayLab(t *testing.T) {
	eventsPath := os.Getenv(labEventsEnv)
	if eventsPath == "" {
		t.Skip("需要 HAI_LAB_EVENTS=<events.jsonl>")
	}
	f, err := os.Open(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	red := NewViewReducer()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)

	var (
		lines    int
		applied  int
		skipped  int
		start    = time.Now()
		parseDur time.Duration
		applyDur time.Duration
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		t0 := time.Now()
		ev, perr := parseLegacyEvent(line)
		parseDur += time.Since(t0)
		if perr != nil || ev == nil {
			skipped++
			continue
		}
		t1 := time.Now()
		_ = red.Apply(ev, EventMeta{})
		applyDur += time.Since(t1)
		applied++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	elapsed := time.Since(start)

	fi, _ := f.Stat()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	t.Logf("=== 全量重放 %s ===", eventsPath)
	t.Logf("文件 %.1f MB / %d 行（applied=%d skipped=%d）", float64(fi.Size())/1024/1024, lines, applied, skipped)
	t.Logf("总耗时 %v  parse=%v  apply=%v", elapsed, parseDur, applyDur)
	t.Logf("重放后块数 = %d", red.BlockCount())
	t.Logf("Go 堆 = %.1f MB (TotalAlloc %.1f MB)", float64(ms.HeapAlloc)/1024/1024, float64(ms.TotalAlloc)/1024/1024)

	// 与 checkpoint 真值比对块数
	if rp := os.Getenv(labRecordEnv); rp != "" {
		raw, rerr := os.ReadFile(rp)
		if rerr != nil {
			t.Logf("读 record 失败: %v", rerr)
			return
		}
		var env struct {
			ViewCheckpoint struct {
				Blocks []json.RawMessage `json:"blocks"`
			} `json:"view_checkpoint"`
			SDKState struct {
				Messages []json.RawMessage `json:"messages"`
			} `json:"sdk_state"`
		}
		if uerr := json.Unmarshal(raw, &env); uerr != nil {
			t.Logf("解析 record 失败: %v", uerr)
			return
		}
		cpBlocks := len(env.ViewCheckpoint.Blocks)
		t.Logf("checkpoint 块数 = %d （重放 %d）→ %s", cpBlocks, red.BlockCount(), cmpWord(cpBlocks == red.BlockCount()))
	}

	// 视图体积：重放出来的视图有多大？（= 若要落盘，需要多少字节）
	if view, merr := red.Marshal(); merr == nil {
		t.Logf("重放视图 = %.1f MB", float64(len(view))/1024/1024)
	}
}

func cmpWord(ok bool) string {
	if ok {
		return "一致 ✓"
	}
	return "不一致 ✗"
}

// TestReplayLabTail 量「只重放尾部 N 行」的耗时（分页/首屏方案的成本）。
func TestReplayLabTail(t *testing.T) {
	eventsPath := os.Getenv(labEventsEnv)
	if eventsPath == "" {
		t.Skip("需要 HAI_LAB_EVENTS=<events.jsonl>")
	}
	lines, err := labReadAllLines(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []int{1000, 5000, 20000, 100000} {
		if tail > len(lines) {
			tail = len(lines)
		}
		red := NewViewReducer()
		start := time.Now()
		for _, line := range lines[len(lines)-tail:] {
			ev, perr := parseLegacyEvent(line)
			if perr != nil || ev == nil {
				continue
			}
			_ = red.Apply(ev, EventMeta{})
		}
		t.Logf("尾部 %d 行重放：%v → %d 块", tail, time.Since(start), red.BlockCount())
	}
}

func labReadAllLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	var out []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

var _ = fmt.Sprintf
