//go:build !unix

package memory

import (
	"errors"
	"os"
	"time"
)

// 非 unix 平台（Windows 等）没有 flock。这里**降级为不加进程间锁**，
// 并把这件事说清楚而不是假装有并发保证：
//
//   - 写盘本身仍是"临时文件 + 同目录 rename"，所以**不会出现半截文件**；
//   - 丢字段的窗口仍然存在（同一 topic 文件被两个进程同时改），概率远低于 unix；
//   - 这与官方无关：官方只在 macOS/Linux/Windows 上发布桌面应用，Windows 上
//     官方自己也没有给出并发语义，《技术方案》没有要求跨平台并发正确性。
//
// 真要修，实现方式是 LockFileEx；本轮不写没有测试覆盖的平台代码。
const flockSupported = false

var errLockBusy = errors.New("memory: 记忆文件写锁被占用")

func flockFile(f *os.File, deadline time.Time) error { return nil }

func syscallFlockUnlock(f *os.File) error { return nil }
