package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestProjectTracesRealLogs 真实事件日志回归（设计文档 §4.2 的基线对照）。
//
// 该测试在 ~/.go-code/events 存在时运行（本机开发环境）；CI/无数据环境自动跳过。
// 基线（2026-09-10 实测，见 docs/TRACE_CHAIN_REDESIGN.md）：
//   - 198 个会话日志 / 2,009 条 trace / 单 trace span 中位 23、最大 3,111
//   - 投影应为**元数据级**：全量约 96 MB → 32 MB 量级
//
// 断言只用**宽松区间**（数据随时间增长），目的是防「投影完全失效」这类硬回归，
// 不做逐字节快照（事件日志持续追加，快照必然漂移）。
func TestProjectTracesRealLogs(t *testing.T) {
	if os.Getenv("HAI_TRACE_REALDATA") == "" {
		t.Skip("真实数据基线；设 HAI_TRACE_REALDATA=1 运行")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	root := filepath.Join(home, ".go-code", "events")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("事件日志不存在，跳过真实数据回归: %v", err)
	}

	var files []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) == ".jsonl" {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		t.Skip("无事件日志文件")
	}
	sort.Strings(files)

	var (
		totalTraces, totalSpans int
		sizes                   []int
		metaBytes               int64
		withTime, withoutTime   int
		parseErr                int
	)
	start := time.Now()
	for _, f := range files {
		details := projectTraces(f, "")
		for _, d := range details {
			totalTraces++
			totalSpans += len(d.Spans)
			sizes = append(sizes, len(d.Spans))
			if b, err := json.Marshal(d.Spans); err == nil {
				metaBytes += int64(len(b))
			}
			for _, s := range d.Spans {
				if s.StartAt > 0 || s.DurMs > 0 {
					withTime++
				} else {
					withoutTime++
				}
			}
		}
	}
	elapsed := time.Since(start)

	if totalTraces < 100 {
		t.Fatalf("trace 条数 %d 明显偏少（基线 2,009）：投影可能失效", totalTraces)
	}
	if totalSpans < totalTraces {
		t.Fatalf("span 总数 %d < trace 条数 %d：配对/收敛有问题", totalSpans, totalTraces)
	}
	// 时间轴覆盖：绝大多数 span 应有时间（P0 补齐 timestamp 后）
	if withTime == 0 {
		t.Fatal("所有 span 都没有时间：timestamp 解析可能失效")
	}
	if withoutTime > 0 && float64(withoutTime)/float64(withTime+withoutTime) > 0.05 {
		t.Fatalf("无时间 span 占比过高：%d/%d（>5%%）——检查 P0 字段是否落盘",
			withoutTime, withTime+withoutTime)
	}
	// 元数据量级：应为「MB 级」而不是「GB 级」（正文未被抄进来）
	if metaBytes > 200<<20 {
		t.Fatalf("投影元数据 %d MB 过大（基线约 32 MB）：可能抄进了正文", metaBytes>>20)
	}
	sort.Ints(sizes)
	t.Logf("真实日志回归：%d 文件 / %d trace / %d span / 中位 %d span / 元数据 %.1f MB / 含时间 %d 缺时间 %d / 解析失败 %d / 耗时 %v",
		len(files), totalTraces, totalSpans, sizes[len(sizes)/2],
		float64(metaBytes)/(1<<20), withTime, withoutTime, parseErr, elapsed)
}
