package builtin

// 工具输出**落盘**的共用件（2026-11-17）。
//
// 背景：引擎层的截断会把结果砍到 20 KB，中间那一大段**永久丢弃**。引擎自己
// 有一层落盘（tools/engine.go 的 spillText），但那是「工具把完整输出原样返回给
// 引擎」之后的补救 —— 工具一旦自己封顶内存，引擎落的就是**被窗口截过的文本**。
//
// 所以：**工具一旦自己封顶，落盘责任必须一起转到工具**（见 bash 的窗口 16 KB
// + 完整输出落盘）。落盘目录的约定（0700 目录 / 0600 文件 / 系统临时目录 /
// 文件名 spill-<runId>-<n>.txt）在这里统一，bash 与 run_python 共用。
//
// 为什么不删文件：路径已经交给模型（分段读）或宿主（full_output_path），
// 删掉等于给死链；留在系统临时目录由操作系统回收。
// ⚠️ 代价：临时目录只增不减（见 docs/TOOL_SEARCH_CLIMB 的「没做的」一节）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/events"
)

// spillStore 落盘目录 + 文件名序号（惰性建目录，同实例复用）。
//
// 并发安全：mu 保护 dir/seq（多 goroutine 同时调用 reserve/write）。
type spillStore struct {
	mu     sync.Mutex
	dir    string
	seq    int
	prefix string // 目录名前缀（os.MkdirTemp 用）
}

// newSpillStore 建一个落盘仓库（prefix = 目录名前缀，如 "hai-harness-bash-spill-"）。
func newSpillStore(prefix string) *spillStore {
	return &spillStore{prefix: prefix}
}

// dirPath 惰性建落盘目录（0700）；同实例复用同一个目录。
func (s *spillStore) dirPath() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		return s.dir, nil
	}
	dir, err := os.MkdirTemp("", s.prefix)
	if err != nil {
		return "", err
	}
	s.dir = dir
	return dir, nil
}

// nextName 生成落盘文件名（spill-<runId>-<n>.txt；无 runId 时省略该段）。
// runId 经 sanitize 处理：它来自事件层（宿主/模型可影响），不能带路径分隔符。
func (s *spillStore) nextName(runID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if runID = sanitizeSpillID(runID); runID == "" {
		return fmt.Sprintf("spill-%d.txt", s.seq)
	}
	return fmt.Sprintf("spill-%s-%d.txt", runID, s.seq)
}

// reserve 预留一个落盘路径（**文件由执行层按需创建** —— 没超阈值就不会有文件，
// 也就没有磁盘开销）。返回空串 = 拿不到路径（只能靠内存窗口截断）。
//
// 与 write 的分工：reserve 是**提前给出路径**，让执行层边跑边写 ⇒
// 命令输出 1 GB 也不会在内存里堆 1 GB；write 是「拿到完整文本后再写一遍」。
func (s *spillStore) reserve(ctx context.Context) string {
	dir, err := s.dirPath()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, s.nextName(toolRunID(ctx)))
}

// write 把未截断全文落盘并返回路径（失败 = ""）。
func (s *spillStore) write(ctx context.Context, full string) string {
	dir, err := s.dirPath()
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, s.nextName(toolRunID(ctx)))
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		return ""
	}
	return path
}

// toolRunID 取本次调用的 runId（事件层；没有就空串）。
func toolRunID(ctx context.Context) string {
	if tc := events.ToolContextFrom(ctx); tc != nil {
		return tc.RunId
	}
	return ""
}

// sanitizeSpillID 只保留文件名安全字符（字母/数字/.-_），其余替换为 "_"，并限长。
func sanitizeSpillID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "._-")
}

// humanBytes 把字节数格式化成人类可读的量（用于结果里的落盘提示）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
