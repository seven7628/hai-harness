package sandbox

// 命令输出的**有界缓冲** + 可选落盘。
//
// 为什么（2026-11-17 实测）：改造前命令输出走无界的 bytes.Buffer ——
// 32 MB 的输出要 96 MB 分配（扩容 ~2× + String() 拷贝 1×），一条输出 1 GB 的
// 命令能把 bridge 撑爆。而模型侧真正能看到的只有引擎截断后的 20 KB
// （tools/engine.go:22 defaultMaxResponseSize），中间那一大段**从来没人看**。
//
// 三个角色：
//   - **头窗口**：输出的开头（命令做了什么、参数是什么）
//   - **尾窗口**：输出的结尾（**错误信息通常在这里** —— "error: cannot find package X"）
//   - **落盘文件**：完整输出（可选）。模型拿到路径后可分段读，
//     对齐 Claude Code 的 <persisted-output> / OpenCode / goose 的做法。
//
// 内存占用与输出大小**无关**（只与窗口上限有关）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// outputBuffer 命令输出缓冲：内存里只保留「头 + 尾」窗口，可选把完整内容
// 流式写到落盘文件。
//
// 状态机：
//   - **完整模式**（总量 ≤ 窗口上限）：全部留在 buf 里（String() 返回全文）。
//   - **窗口模式**（越过上限）：buf 释放，只留头 + 尾；中间部分要么落盘、
//     要么丢弃（计入 omitted）。
//
// 并发：stdout / stderr 两个拷贝协程会同时写 ⇒ 全程持锁。
type outputBuffer struct {
	mu sync.Mutex

	limited  bool // 是否设了窗口上限（false = 完整缓冲全部输出）
	headOnly bool // 只要头（脚本路径：output 必须是逐字节前缀，不得插标记）
	headSize int
	tailSize int
	head     []byte // 开头窗口
	tail     []byte // 结尾窗口（满了丢最老的）
	buf      []byte // 完整内容（仅在完整模式下保留）
	dropping bool   // 已越过窗口上限

	total int64 // 输出总字节数

	// 落盘
	filePath      string
	fileAfter     int
	file          *os.File
	spilledToFile bool
	fileErr       error
}

// newOutputBuffer 造缓冲。
//
// limit     = 内存窗口上限（头+尾合计）；0 = 不限（完整缓冲全部输出，= 改造前行为）
// filePath  = 落盘路径（空 = 不落盘）
// fileAfter = 超过多少字节才开始落盘（0 = 立即落盘）
// headOnly  = 只要头（脚本路径：结构化 output 必须是**逐字节前缀**，不得插标记）
func newOutputBuffer(limit int, filePath string, fileAfter int, headOnly bool) *outputBuffer {
	b := &outputBuffer{
		filePath:  filePath,
		fileAfter: fileAfter,
		limited:   limit > 0,
		headOnly:  headOnly,
	}
	if b.limited {
		if headOnly {
			b.headSize = limit
			b.tailSize = 0
		} else {
			b.headSize = limit / 2
			b.tailSize = limit - b.headSize
		}
		if b.headSize < 1024 {
			b.headSize = 1024
		}
	}
	return b
}

// Write 实现 io.Writer（os/exec 的拷贝协程调用）。
func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))

	// 头窗口
	if b.headSize > 0 && len(b.head) < b.headSize {
		n := b.headSize - len(b.head)
		if n > len(p) {
			n = len(p)
		}
		b.head = append(b.head, p[:n]...)
	}
	// 尾窗口
	if b.tailSize > 0 {
		if len(p) >= b.tailSize {
			b.tail = append(b.tail[:0], p[len(p)-b.tailSize:]...)
		} else {
			if len(b.tail)+len(p) > b.tailSize {
				drop := len(b.tail) + len(p) - b.tailSize
				b.tail = b.tail[drop:]
			}
			b.tail = append(b.tail, p...)
		}
	}

	if b.dropping {
		// 已是窗口模式：中间部分进落盘文件（没有文件就是真丢了）
		if b.filePath != "" {
			b.writeToFile(p)
		}
		return len(p), nil
	}

	// 完整模式
	b.buf = append(b.buf, p...)
	if b.limited && b.total > int64(b.headSize+b.tailSize) {
		// 刚越过窗口上界：把完整内容交给落盘文件（或丢弃），然后释放内存
		b.flushToFile()
		b.buf = nil
		b.dropping = true
	}
	return len(p), nil
}

// flushToFile 把当前完整缓冲写进落盘文件（打开文件）。
// 没有落盘路径时不做任何事 —— buf 随后被释放，中间部分真的没了。
func (b *outputBuffer) flushToFile() {
	if b.filePath == "" || b.spilledToFile {
		return
	}
	if err := os.MkdirAll(filepath.Dir(b.filePath), 0o700); err != nil {
		b.fileErr = err
		return
	}
	f, err := os.OpenFile(b.filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		b.fileErr = err
		return
	}
	if len(b.buf) > 0 {
		if _, err := f.Write(b.buf); err != nil {
			b.fileErr = err
		}
	}
	b.file = f
	b.spilledToFile = true
}

// writeToFile 往已打开的落盘文件追加。
func (b *outputBuffer) writeToFile(p []byte) {
	if b.file == nil {
		return
	}
	if _, err := b.file.Write(p); err != nil && b.fileErr == nil {
		b.fileErr = err
	}
}

// finish 收尾：关文件（调用方随后可安全读落盘文件）。
func (b *outputBuffer) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		_ = b.file.Close()
		b.file = nil
	}
}

// omittedLocked 计算被省略的字节数（调用方持锁）。
// 完整模式下为 0（全文都在 buf 里）。
func (b *outputBuffer) omittedLocked() int64 {
	if !b.dropping {
		return 0
	}
	kept := b.keptBytes()
	if b.total > kept {
		return b.total - kept
	}
	return 0
}

// keptBytes 内存里留下的字节数（head-only 时只有头）。
func (b *outputBuffer) keptBytes() int64 {
	if b.headOnly {
		return int64(len(b.head))
	}
	return int64(len(b.head) + len(b.tail))
}

// String 返回**模型可见**的输出文本：
//   - 未超窗口 ⇒ 全文；
//   - 超了 ⇒ 头 + 省略标记 + 尾。
func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dropping {
		return string(b.buf)
	}
	if b.headOnly {
		// 脚本路径：output 必须是**逐字节前缀**（脚本按顺序读）⇒ 不插标记、不带尾。
		return string(b.head)
	}
	var sb strings.Builder
	sb.Write(b.head)
	if omitted := b.omittedLocked(); omitted > 0 {
		fmt.Fprintf(&sb, "\n[... %d bytes omitted ...]\n", omitted)
	}
	sb.Write(b.tail)
	return sb.String()
}

// didSpill 是否真的落盘了（文件已创建、且没有写失败）。
func (b *outputBuffer) didSpill() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spilledToFile && b.fileErr == nil
}

// totalBytes 输出总字节数。
func (b *outputBuffer) totalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// omittedBytes 模型可见文本里被省略的字节数。
func (b *outputBuffer) omittedBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.omittedLocked()
}

// spillErr 落盘失败的原因（nil = 没失败或没落盘）。
func (b *outputBuffer) spillErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fileErr
}
