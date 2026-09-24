package session

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// 会话 jsonl 自压缩（O(N²) 快照治理，见 docs/SESSION_JSONL_STATE_O2_FIX.md §4.1）。
//
// 问题：每次 checkpoint 都写一条**全量上下文快照**（state 行），文件体积随轮次
// 呈 O(N²) 增长——实测单会话到 2.9 GB，其中 99.7% 是逐字节重复的 messages。
//
// 治理：超过阈值时**原地重写**，只保留最后 compactKeepStates 条 state 行。
// 依据：sessions/*.jsonl 的读取者只有两处——Load 只尾扫**最后一条** state 行
// （tailLastState），ReadMeta 只读最后一条 meta 行；更早的 state 行无人读取。
// history / compaction / meta 行全部保留（history 是 append-only 审计日志）。
//
// 保留 K=3（而非 1）的原因：tailLastState 的尾扫会跳过损坏行，末行半截时
// 需要回退到上一条；K=1 就退无可退。
//
// 安全性：
//   - 写临时文件 + rename 原子替换（同目录，保证同文件系统）——崩溃时原文件完好
//   - 逐行按原始字节拷贝，不重新序列化：被保留的行**逐字节不变**
//   - 与 append 共用同一把进程内互斥锁，避免“重写”与“追加”竞争导致丢行
//     （跨进程由会话 flock 保证）
const (
	// defaultCompactKeepStates 保留最后 N 条 state 行（<=0 关闭自压缩）。
	defaultCompactKeepStates = 3
	// defaultCompactThreshold 触发自压缩的文件体积阈值（字节）。
	defaultCompactThreshold = 24 << 20
	// compactMinGainRatio 最低收益比：重写后至少省下该比例才执行，
	// 避免“history 本身已很大”时每轮都做一次无收益重写。
	compactMinGainRatio = 0.2
	// maxStateRowBytes 单条 state 行的读取上限（防御异常大行；正常最大约 2 MB）。
	maxStateRowBytes = 64 << 20
)

// maybeCompact 检查并执行自压缩。仅由 SaveState 调用（state 行是膨胀源）。
// 返回是否实际执行了重写。
func (s *FileStore) maybeCompact(p string) (bool, error) {
	if s.compactKeep <= 0 || s.compactThreshold <= 0 {
		return false, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if fi.Size() < s.compactThreshold {
		return false, nil
	}
	return s.compactFile(p, s.compactKeep)
}

// compactFile 原地重写：保留全部非 state 行 + 最后 keep 条 state 行。
// 原子替换，失败时原文件保持不变。
func (s *FileStore) compactFile(p string, keep int) (bool, error) {
	// ① 第一遍：定位最后 keep 条 state 行的起点
	starts, err := stateRowStarts(p)
	if err != nil {
		return false, err
	}
	if len(starts) <= keep {
		return false, nil // 没有可丢的行（或 state 行还不够多）
	}
	keepFrom := starts[len(starts)-keep]

	orig, err := os.Stat(p)
	if err != nil {
		return false, err
	}

	// ② 第二遍：逐行拷贝（跳过起点 < keepFrom 的 state 行）→ 临时文件
	tmp, err := writeStreamedTempFile(filepath.Dir(p), filepath.Base(p), orig.Mode().Perm(), func(w *bufio.Writer) (int64, error) {
		return copyKeepingTailStates(p, keepFrom, w)
	})
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp) // rename 成功后此处为 no-op

	newSize, err := os.Stat(tmp)
	if err != nil {
		return false, err
	}
	// ③ 收益检查：省得太少就不折腾（避免大 history 场景每轮重写）
	if newSize.Size() > int64(float64(orig.Size())*(1-compactMinGainRatio)) {
		return false, nil
	}

	// ④ 原子替换 + 目录 fsync（保证 rename 落盘）
	if err := os.Rename(tmp, p); err != nil {
		return false, fmt.Errorf("replace session file: %w", err)
	}
	if err := syncDir(filepath.Dir(p)); err != nil {
		return true, err
	}
	return true, nil
}

// stateRowStarts 顺序扫描，返回每条 state 行的起始偏移。
// 用 ReadBytes 逐行读（不在内存驻留整文件），行内不解析 JSON。
func stateRowStarts(p string) ([]int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var (
		starts []int64
		off    int64
	)
	for {
		lineStart := off
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			off += int64(len(line))
			if bytes.HasPrefix(line, stateRowPrefix) {
				starts = append(starts, lineStart)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return starts, nil
}

// copyKeepingTailStates 把 p 逐行写入 w，丢弃起始偏移 < keepFrom 的 state 行。
// 保留的行按原始字节写出（不重新序列化 → 逐字节不变）。
func copyKeepingTailStates(p string, keepFrom int64, w *bufio.Writer) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var ( // nolint:all // 与 stateRowStarts 对称的偏移记账
		written int64
		off     int64
	)
	for {
		lineStart := off
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			off += int64(len(line))
			if !(lineStart < keepFrom && bytes.HasPrefix(line, stateRowPrefix)) {
				if _, err := w.Write(line); err != nil {
					return written, err
				}
				written += int64(len(line))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, rerr
		}
	}
	return written, nil
}

// stateRowPrefix state 行的判定前缀（与 fileRecord 的 JSON 字段序一致）。
var stateRowPrefix = []byte(`{"type":"state"`)

// writeStreamedTempFile 在 dir 内写临时文件（同目录 → rename 必在同一文件系统）。
// 与 store_record.go 的 writeTempFile 的区别：**流式写**，不把整个文件读进内存
// （自压缩面对的是 24 MB+ 甚至更大的历史文件，全量驻留内存不安全）。
// write 负责写内容；失败时清理临时文件。
func writeStreamedTempFile(dir, base string, mode os.FileMode, write func(*bufio.Writer) (int64, error)) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+base+".compact-*")
	if err != nil {
		return "", err
	}
	w := bufio.NewWriterSize(tmp, 1<<20)
	if _, werr := write(w); werr != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", werr
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil { // os.CreateTemp 固定 0600，恢复原权限
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// syncDir 对目录 fsync，保证 rename 等目录项变更已落盘。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
