package hooks

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Journal 是 hook 执行的落地记录：一行一次执行，含**正常结束**的那些。
//
// 为什么需要它：emitRun 只上报"非正常结束"的跑法（正常跑法原本打算走日志，而日志
// 并不存在）。于是"这个 hook 到底跑过没有"在事前事后都答不出来——实测到的现场是：
// 用户在 ~/.go-code/hooks/ 下写了脚本，settings.json 里没有 hooks 段，于是一次都没
// 执行，而系统里没有任何一处能说明这件事。
//
// 定位是**事后追问**，不是实时反馈：实时反馈走 events（tool_hook_warning），
// 这里只负责"发生过什么"，因此写入永远是 best-effort，失败不打断会话。
type Journal struct {
	path string
	// maxBytes 超过就压缩到尾部一半。hook 是高频路径（PostToolUse 每个工具一次），
	// 无界增长迟早会把磁盘写满，而这只是一份诊断日志。
	maxBytes int64

	mu sync.Mutex
}

// NewJournal 构造日志；path 为空表示不记录（返回 nil）。
func NewJournal(path string) *Journal {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return &Journal{path: path, maxBytes: 1 << 20} // 1 MiB ≈ 4000 行
}

// Append 追加一条。任何 I/O 错误都被吞掉：日志失败不该影响 hook 本身的结果。
func (j *Journal) Append(run HookRun) {
	if j == nil {
		return
	}
	line, err := json.Marshal(run)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if fi, err := os.Stat(j.path); err == nil && fi.Size() > j.maxBytes {
		j.compactLocked()
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// compactLocked 只保留尾部一半的行。调用方持有 mu。
func (j *Journal) compactLocked() {
	f, err := os.Open(j.path)
	if err != nil {
		return
	}
	var keep []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		keep = append(keep, sc.Text())
	}
	f.Close()
	if len(keep) > 1 {
		keep = keep[len(keep)/2:]
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, j.path)
}

// ReadJournal 读回最近的 limit 条（limit<=0 表示全部）。
//
// 给面板与命令行用：它回答的是"最近跑过哪些 hook、有没有超时"，而不是实时状态。
func ReadJournal(path string, limit int) ([]HookRun, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var runs []HookRun
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r HookRun
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // 半行/损坏行跳过：这是诊断日志，不是账本
		}
		runs = append(runs, r)
	}
	if err := sc.Err(); err != nil {
		return runs, err
	}
	if limit > 0 && len(runs) > limit {
		runs = runs[len(runs)-limit:]
	}
	return runs, nil
}
