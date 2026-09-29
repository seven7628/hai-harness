//go:build unix

package memory

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// flockFile 对 fd 取独占锁（阻塞等待，最多到 deadline）。
//
// 为什么用 flock 而不是 O_EXCL 建锁文件：O_EXCL 的锁在进程崩溃时会**留下死锁**
// （锁文件永远存在），而记忆目录是机器本地、用户几乎不会去清理的目录——一个死锁
// 会让那条记忆再也写不进去。flock 由内核在 fd 关闭时自动释放，进程死了锁就没了。
const flockSupported = true

// errLockBusy 表示锁被占用、可以重试（区别于真正的 IO 失败）。
var errLockBusy = errors.New("memory: 记忆文件写锁被占用")

func flockFile(f *os.File, deadline time.Time) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return errLockBusy
		}
		time.Sleep(lockPoll)
	}
}

func syscallFlockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
