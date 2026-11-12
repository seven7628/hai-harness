// Package atomicfile 提供跨进程安全的小文件写入原语。
//
// 两个场景（PR-05a0 从 tools/builtin/lock.go 提取，零行为变化）：
//   - 覆盖类写入：flock 串行化「读-改-写」事务，内容写同目录临时文件后 rename ——
//     任何时刻读者都看到完整文件；另一写者持锁时返回明确错误（模型可见、可重试）。
//   - 追加类写入：flock 串行化 + O_APPEND 单次写（跨实例顺序化）。
//
// 锁是 POSIX 建议锁（syscall.Flock，darwin/linux）；Windows 需另行实现。
// 单 goroutine 内本不会并发 —— 这层防护的是多 Session / 多实例共享同一工作区的场景。
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked 表示目标文件正被另一个写者持有（调用方可据此给用户可行动提示）。
var ErrLocked = errors.New("file is locked by another writer")

// LockFile 对已打开的 fd 加非阻塞排他锁；失败 = 另一写者持有（冲突）。
func LockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrLocked
		}
		return fmt.Errorf("lock file: %w", err)
	}
	return nil
}

// LockPath 打开目标文件并加排他锁。文件不存在返回 (nil, nil) —— 尚无写者，无需锁。
// 返回的锁须由调用方持有到写入事务结束并 Close（Close 即释放 flock）。
func LockPath(p string) (*os.File, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := LockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReplaceLocked 在调用方已持锁（或文件尚不存在、lock == nil）时执行原子替换。
// 父目录缺失会被创建 —— 写入类工具的语义承诺。
func ReplaceLocked(lock *os.File, p, content string) error {
	// 父目录缺失时 CreateTemp 会失败并报出内部临时文件名（".atomic-<n>"），
	// 模型无法从中看出真正原因是「父目录不存在」（实测 16 次/周，历史最高）。
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create parent directory %s for %s: %w", dir, p, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".atomic-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // rename 成功后无此文件，清理调用无害
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// Replace 是完整的原子写：加锁 + temp + rename。
func Replace(p, content string) error {
	lock, err := LockPath(p)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}
	return ReplaceLocked(lock, p, content)
}

// Append 追加写入：flock 串行化 + O_APPEND（单次写原子，跨实例顺序化）。
func Append(p, content string) error {
	// 与 ReplaceLocked 同源：追加写也必须能创建父目录（工具描述承诺自动建目录）。
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create parent directory %s for %s: %w", dir, p, err)
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := LockFile(f); err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return err
}
