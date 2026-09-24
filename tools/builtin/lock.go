package builtin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// 文件写并发保护（跨进程/跨实例）：
//   - 覆盖类写入（write_file 覆盖 / edit_file）走 temp+rename 原子替换 ——
//     读者永远看到完整文件；写前 flock 串行化「读-改-写」事务（edit_file
//     持锁跨读改写全程），冲突写入方拿到明确错误（进结果文本，模型可见可重试）；
//   - 追加类写入（write_file append）走 appendTo：flock 串行化 + O_APPEND 单次写。
//
// 说明：单 goroutine 架构内（单 agent 顺序执行）本不会并发，
// 锁防护的是多 Session / 多实例共享同一工作区的场景。
// syscall.Flock 为 POSIX 建议锁（darwin/linux），Windows 需另行实现。

// lockFile 对已打开的 fd 加非阻塞排他锁；失败 = 另一写者持有（冲突）。
func lockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return errors.New("file is locked by another writer")
		}
		return fmt.Errorf("lock file: %w", err)
	}
	return nil
}

// lockPath 打开目标文件并加排他锁（文件不存在返回 nil 锁 —— 无可写者）。
// 返回的锁须由调用方持有到写入事务结束并 Close（Close 即释放 flock）。
func lockPath(p string) (*os.File, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// replaceLocked 在调用方已持锁的前提下执行原子替换：
// 内容写入同目录临时文件后 rename —— 替换瞬时完成，任何时刻读者都看到完整文件。
// lock 可为 nil（文件尚不存在，无并发写者）。
func replaceLocked(lock *os.File, p, content string) error {
	// 父目录缺失时 CreateTemp 会失败并报出内部临时文件名（".atomic-<n>"），
	// 模型无法从中看出真正原因是「父目录不存在」（实测 16 次/周，历史最高）。
	// 写入类工具语义上就应创建父目录（write_file 的文档亦如此承诺）。
	// 文案不写死工具名：edit_file 也走本函数（2026-09-18 复核指出）。
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

// atomicReplace 完整原子写：锁 + temp + rename（write_file 覆盖路径）。
func atomicReplace(p, content string) error {
	lock, err := lockPath(p)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}
	return replaceLocked(lock, p, content)
}

// appendTo 追加写入：flock 串行化 + O_APPEND（单次写原子，跨实例顺序化）。
func appendTo(p, content string) error {
	// 与 replaceLocked 同源：追加写也必须能创建父目录。否则 write_file(append=true)
	// 到缺失目录会报 `open <真实路径>: no such file or directory`，与工具描述
	// 「Creates missing files and parent directories automatically」不符
	//（2026-09-18 复核发现：覆盖路径已修，追加路径当时遗漏）。
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
	if err := lockFile(f); err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return err
}
