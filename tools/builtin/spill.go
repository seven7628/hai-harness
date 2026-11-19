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
// 删掉等于给死链 —— 但**必须按年龄删**（承诺窗口 = 模型读完之前，见
// spillFileTTL）：改造前是「永不删」，目录只增不减（实测本机 2158 个
// hai-harness-*-spill-* 目录，其中 1867 个是**空壳**）。
// ⇒ 两条修法（2026-11-18 资源有界化第 2 步）：
//   1. **目录不预建**（下面 dirPath）：第一次真要落盘时才建 ⇒ 空壳从根上不产生；
//   2. **按年龄回收**（下面 SweepToolSpillDirs）：文件过 TTL 删、目录空了删。

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// spillFileTTL 工具级落盘文件的**承诺窗口**（小时）。
//
// 为什么是这个值：**交给模型的每个路径都是一个带期限的承诺** ——
// bash 的结果文本写着「full output saved to <path> — Read it with read_file」。
// 模型会在下一轮（秒级~分钟级）读它，但**不该**指望几天后还能读 ⇒
// 24h 是「足够读完 + 不至于无限堆积」的平衡（与引擎级 spill 的 TTL 同口径）。
//
// 写进提示词（见 bash 的落盘提示）：承诺窗口对模型**可见** ⇒ 模型不会
// 拿着一个三天的旧路径来找文件。
const spillFileTTL = 24 * time.Hour

// spillStore 落盘目录 + 文件名序号（**目录惰性创建**：第一次真要落盘时才建）。
//
// 并发安全：mu 保护 dir/seq（多 goroutine 同时调用 reserve/write）。
type spillStore struct {
	mu     sync.Mutex
	dir    string
	seq    int
	prefix string // 目录名前缀（见 isToolSpillDir 的约定）
}

// newSpillStore 建一个落盘仓库（prefix = 目录名前缀，如 "hai-harness-bash-spill-"）。
func newSpillStore(prefix string) *spillStore {
	return &spillStore{prefix: prefix}
}

// dirPath 返回本实例的落盘目录路径（**不创建目录**）；同实例复用同一个路径。
//
// 为什么改成「不预建」（2026-11-18）：改造前 dirPath 用 os.MkdirTemp **立刻建目录**，
// 而 bash 的每次调用都会 reserve 一个落盘路径（给执行层「超阈值才写」）⇒
// **每次调用都建一个 0700 目录**，哪怕这次输出只有 10 字节 ⇒
// 实测本机 2158 个目录、其中 1867 个是空壳（文件早被系统回收，壳留下）。
//
// 现在：目录名先定下来（pid + 随机后缀 ⇒ 跨进程唯一），**真正写文件的人**负责
// 建目录（spillStore.write 与 sandbox 的 outputBuffer 都会 MkdirAll）⇒
// 没发生溢出的调用**一个目录都不留**。
func (s *spillStore) dirPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		s.dir = filepath.Join(os.TempDir(), s.prefix+uniqueDirSuffix())
	}
	return s.dir
}

// uniqueDirSuffix 落盘目录的唯一后缀（pid + 随机 hex）：跨进程唯一（多个 bridge
// 实例同时在跑时不能撞名），且**不创建**目录（见 dirPath）。
func uniqueDirSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用：退到 pid + 纳秒时间戳（唯一性够用：同进程内不会重复）。
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%x", os.Getpid(), b[:])
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
	return filepath.Join(s.dirPath(), s.nextName(toolRunID(ctx)))
}

// reserveNamed 预留一个**带指定基名**的落盘路径（后台日志用：名字要能让人一眼
// 看出是后台日志，而不是命令输出 spill）。
//
// 与 reserve 的差别只在文件名（内容与权限约定完全一致：同目录 0700 / 文件 0600）。
func (s *spillStore) reserveNamed(ctx context.Context, base string) string {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return filepath.Join(s.dirPath(), fmt.Sprintf("%s-%d.log", sanitizeSpillID(base), seq))
}

// write 把未截断全文落盘并返回路径（失败 = ""）。
func (s *spillStore) write(ctx context.Context, full string) string {
	dir := s.dirPath()
	// 目录**按需创建**（见 dirPath）：没发生溢出的调用不留空壳目录。
	if err := os.MkdirAll(dir, 0o700); err != nil {
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

// ---------- 工具级落盘目录的回收（2026-11-18 资源有界化第 2 步） ----------

// isToolSpillDir 判断临时目录里的某个目录是不是「工具级落盘目录」。
//
// 约定（不按包注册前缀，而是按**命名约定**扫）：名字以 hai-harness- 开头且
// 含 -spill- ⇒ bash / run_python / codemode 三族同款（新增一族自动被覆盖，
// 不用改这里）。
//
// 排除：引擎级的根目录 hai-harness-toolspill（不含 "-spill-"，且由 tools 包
// 自己扫 ⇒ 天然不重叠）。
func isToolSpillDir(name string) bool {
	return strings.HasPrefix(name, "hai-harness-") && strings.Contains(name, "-spill-")
}

// spillMaxTotalBytes 工具级落盘的**总量上限**（300 MiB）。
//
// 为什么 TTL 之外还要总量上限：TTL 只按**年龄**删 ⇒ 一台机器一天跑几百次
// 大输出命令（每次数 MB）⇒ 24h 窗口内就能堆到 GB 级（实测：清扫后仍剩
// 1.0GB / 363 个文件，全是当天写的）。
// 总量上限是「最坏情况」的兜底：超了就按**最旧先删**（最旧的路径最不可能
// 还在被读 —— 与引擎级 spill 的同一口径）。
const spillMaxTotalBytes = 300 << 20

// spillFile 一份落盘文件的元信息（清扫时收集，用于按最旧先删）。
type spillFile struct {
	path string
	size int64
	mod  time.Time
}

// SweepToolSpillDirs 清扫工具级落盘目录（TTL + 总量上限 + 空壳回收）。
//
// 触发点：**宿主在会话开始/结束各调一次**（收尾点挂在会话上 ⇒ 比资源活得久；
// 挂在「产生资源的那次调用」上会漏：命令跑完就没有下一次触发了）。
//
// 异步跑（目录多时是上千次 readdir，不能挂在关键路径上）。
func SweepToolSpillDirs() {
	go sweepToolSpillRoot(os.TempDir(), time.Now(), spillMaxTotalBytes)
}

// sweepToolSpillRoot 清扫某个根目录下的工具级落盘目录（TTL + 总量上限 + 空壳）。
//
// 拆出来是为了可测（SweepToolSpillDirs 固定扫系统临时目录，测试里没法造数据）。
func sweepToolSpillRoot(root string, now time.Time, maxTotalBytes int64) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var (
		files []spillFile
		dirs  []string
	)
	for _, e := range entries {
		if !e.IsDir() || !isToolSpillDir(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		dirs = append(dirs, dir)
		files = append(files, sweepToolSpillDir(dir, now)...)
	}
	// 总量超上限 ⇒ 按最旧先删（最旧的路径最不可能还在被读）。
	total := int64(0)
	for _, f := range files {
		total += f.size
	}
	if total > maxTotalBytes {
		sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
		for _, f := range files {
			if total <= maxTotalBytes {
				break
			}
			if err := os.Remove(f.path); err == nil {
				total -= f.size
			}
		}
	}
	// 空目录不留（os.Remove 对非空目录返回 ENOTEMPTY，忽略即可）。
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

// sweepToolSpillDir 清扫一个工具级落盘目录：删掉过期文件，返回**幸存**的文件
// 元信息（供总量上限那一步按最旧先删）。目录空了顺手删掉。
//
// 失败一律忽略（回收是尽力而为，绝不能影响工具调用）。
func sweepToolSpillDir(dir string, now time.Time) []spillFile {
	// 自守：只碰「工具级落盘目录」（按命名约定）—— 防止将来有人拿它去扫别的目录。
	if !isToolSpillDir(filepath.Base(dir)) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	cutoff := now.Add(-spillFileTTL)
	var alive []spillFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path) // 过期：直接删（路径早已交给模型读过，没人再回头）
			continue
		}
		alive = append(alive, spillFile{path: path, size: info.Size(), mod: info.ModTime()})
	}
	// 空目录不留（改造前每次调用都建目录 ⇒ 87% 是空壳）。
	_ = os.Remove(dir)
	return alive
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
