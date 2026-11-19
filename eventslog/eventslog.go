// Package eventslog 提供事件日志文件的**透明 gzip 封存**原语。
//
// ## 为什么
//
// 事件日志（`~/.go-code/events/<wsKey>/<sid>.jsonl`）是**只增不减**的：
// 实测 528 个文件 / 6.7GB，最早到 2026-08-29，**全仓库没有任何保留期逻辑**
// ⇒ 它是一根只涨不消的柱子。
//
// 而它的内容是 **JSON 文本** ⇒ gzip 压缩比 6–7×（实测）⇒ 6.7GB → 约 1GB，
// **内容一点不丢**（用户明确要求「events 不要删，要支持 zip 压缩」）。
//
// ## 形态：明文活动 + gzip 封存
//
//	活动会话：`<sid>.jsonl`（明文，还在追加）
//	封存会话：`<sid>.jsonl.gz`（压缩后只读）
//
// 读侧透明（`OpenReader`）：有明文读明文，没有就读 .gz。
// 写侧（`Restore`）：会话被**重新激活**时（用户打开旧会话继续聊），
// 先把 .gz 解压还原成明文，再继续追加。
//
// ## 安全
//
//  1. 压缩期间源文件可能被追加 ⇒ 压缩前后各 stat 一次，**变了就放弃**
//     （删掉半成品 .gz，保留明文）⇒ 绝不丢尾巴。
//  2. 先写 .gz（原子）再删明文 ⇒ 中途崩溃时两者都在 ⇒ 读者优先读明文 ⇒ 不丢数据。
//  3. 封存后的 .gz 与明文**互斥**：同一时刻只有一种存在（`Archive` 删明文，
//     `Restore` 删 .gz）。
package eventslog

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seven7628/hai-harness/internal/atomicfile"
)

// GzSuffix 是封存文件的后缀（`<sid>.jsonl` → `<sid>.jsonl.gz`）。
const GzSuffix = ".gz"

// DefaultLevel 是封存用的 gzip 压缩级别。
//
// 权衡（实测于真实事件日志）：默认级别（6）压缩比约 6–7×、约 30–50MB/s；
// BestSpeed(1) 约 4×、约 100–150MB/s。
// ⇒ 用默认级别：多省 30% 磁盘，代价是封存慢一点（封存是后台限速跑的）。
const DefaultLevel = gzip.DefaultCompression

// gzReadCloser 把 gzip.Reader 与底层文件一起关掉。
type gzReadCloser struct {
	zr *gzip.Reader
	f  *os.File
}

func (g *gzReadCloser) Read(p []byte) (int, error) { return g.zr.Read(p) }
func (g *gzReadCloser) Close() error {
	zerr := g.zr.Close()
	ferr := g.f.Close()
	if zerr != nil {
		return zerr
	}
	return ferr
}

// OpenReader 打开事件日志（明文或 gzip 封存），返回逐行可读的 reader。
//
// 优先级：**明文优先**。理由：封存过程中（或崩溃后）两者可能同时存在，
// 而明文那份是**更新的**（追加写在明文上）⇒ 读明文才不丢新事件。
//
// 都��在 → 返回原始的「文件不存在」错误。
func OpenReader(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err == nil {
		return f, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	gzPath := path + GzSuffix
	gf, gerr := os.Open(gzPath)
	if gerr != nil {
		return nil, err // 返回明文的原始错误（调用方按「没有事件日志」处理）
	}
	zr, zerr := gzip.NewReader(gf)
	if zerr != nil {
		_ = gf.Close()
		return nil, fmt.Errorf("eventslog: 打开 %s: %w", gzPath, zerr)
	}
	return &gzReadCloser{zr: zr, f: gf}, nil
}

// Archive 把明文事件日志压成 `<path>.gz`（原子），成功后删掉明文。
//
// 返回 (true, nil) = 已封存；(false, nil) = **放弃**（源文件在压缩期间被改动
// ⇒ 压出来的副本不完整 ⇒ 宁可不封）。
//
// 步骤：
//  1. stat 源文件（记住 size + mtime）
//  2. 流式 gzip 到 `<path>.gz`（临时文件 + 原子 rename）
//  3. 再 stat 一次：变了 ⇒ 删掉 .gz，返回 false（明文原封不动）
//  4. 没变 ⇒ 删明文
func Archive(path string, level int) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	src, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer src.Close()

	gzPath := path + GzSuffix
	pr, pw := io.Pipe()
	go func() {
		zw, zerr := gzip.NewWriterLevel(pw, level)
		if zerr != nil {
			_ = pw.CloseWithError(zerr)
			return
		}
		_, cerr := io.Copy(zw, src)
		if cerr != nil {
			_ = zw.Close()
			_ = pw.CloseWithError(cerr)
			return
		}
		if zerr := zw.Close(); zerr != nil {
			_ = pw.CloseWithError(zerr)
			return
		}
		_ = pw.Close()
	}()

	// 原子写：同目录临时文件 + fsync + rename（内容永远完整）。
	werr := atomicfile.ReplaceReader(gzPath, pr, 0o600)
	if werr != nil {
		_ = pr.Close()
		return false, werr
	}

	// 压缩期间源文件被追加过？⇒ 半成品 .gz 会**丢掉尾巴** ⇒ 放弃封存。
	fi2, serr := os.Stat(path)
	if serr != nil {
		_ = os.Remove(gzPath)
		return false, serr
	}
	if fi2.Size() != fi.Size() || !fi2.ModTime().Equal(fi.ModTime()) {
		_ = os.Remove(gzPath)
		return false, nil
	}

	// 明文已被 .gz 完整取代 ⇒ 删明文（顺序不能反：先删明文后崩溃 = 丢数据）。
	if rerr := os.Remove(path); rerr != nil {
		return false, rerr
	}
	return true, nil
}

// Restore 若明文不存在但 `<path>.gz` 存在 ⇒ 解压还原成明文（原子），删掉 .gz。
//
// 用于「封存的会话被重新激活」：用户打开旧会话继续聊 ⇒ 事件要能继续追加。
// 返回 (true, nil) = 已还原；(false, nil) = 无需还原（明文在 / 两者都没有）。
func Restore(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil // 明文还在（那就是活动会话）
	} else if !os.IsNotExist(err) {
		return false, err
	}
	gzPath := path + GzSuffix
	gf, err := os.Open(gzPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // 两者都没有
		}
		return false, err
	}
	defer gf.Close()
	zr, zerr := gzip.NewReader(gf)
	if zerr != nil {
		return false, fmt.Errorf("eventslog: 打开 %s: %w", gzPath, zerr)
	}
	defer zr.Close()

	if werr := atomicfile.ReplaceReader(path, zr, 0o600); werr != nil {
		return false, werr
	}
	// 明文已完整还原 ⇒ 删 .gz（顺序不能反）。
	if rerr := os.Remove(gzPath); rerr != nil {
		return false, rerr
	}
	return true, nil
}

// ListIDs 返回目录下所有事件日志的 id（明文 `.jsonl` 与封存 `.jsonl.gz` 都算）。
//
// 去重规则：同一个 id 若两者都在 ⇒ 算**一个**（明文优先，见 OpenReader）。
func ListIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	// 封存的（只有 .gz）也要算进来
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl"+GzSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl"+GzSuffix)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// LogPath 返回某会话的事件日志路径（明文形态）。
func LogPath(dir, id string) string { return filepath.Join(dir, id+".jsonl") }
