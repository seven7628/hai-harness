package session

// 按 record 的互斥锁（进程级）。
//
// ## 为什么不是 FileStorage 上的字段
//
// 锁必须按「磁盘上的那条 record」互斥，而不是按「哪个 FileStorage 对象」：
//
//   - 生产里全进程只有一个 FileStorage（bridge 启动时建一次），旧实现用它上面
//     的一把 sync.Mutex 串行化了**所有 workspace 的所有会话** —— 提交与
//     LoadRecord 互相排队，实测邻座一次读就让别人的提交多等 810ms；
//   - 测试与 CLI 会 new 多个 FileStorage 指向同一个 root，锁放字段里就失去互斥。
//
// 所以这里做成进程级的 keyed lock，key 是 record 的绝对路径。
//
// ## 粒度与不变量
//
// 同一个 record 的 commit / load / delete 互斥；不同 record 完全并行。
// `acquireBlockStore` 返回的 *blockStore 实例会在锁外被使用（缓存表只保护查表），
// 它的读写安全**依赖这把锁**：任何持有 blockStore 指针的代码路径都必须先持有
// 对应 record 的锁。改动这块时务必守住这条，否则会出现「读到半个索引」。

import (
	"path/filepath"
	"sync"
)

type recordLock struct {
	mu   sync.Mutex
	refs int // 持有者 + 等待者；归零时从表里摘除
}

var recordLockTable = struct {
	sync.Mutex
	m map[string]*recordLock
}{m: make(map[string]*recordLock, 64)}

// lockRecord 取得该 record 的互斥锁，返回解锁函数。
func lockRecord(path string) (unlock func()) {
	key := filepath.Clean(path)

	recordLockTable.Lock()
	l := recordLockTable.m[key]
	if l == nil {
		l = &recordLock{}
		recordLockTable.m[key] = l
	}
	l.refs++
	recordLockTable.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		recordLockTable.Lock()
		l.refs--
		if l.refs == 0 {
			delete(recordLockTable.m, key)
		}
		recordLockTable.Unlock()
	}
}

// recordLockRefs 仅供测试观测锁表不泄漏。
func recordLockRefs() int {
	recordLockTable.Lock()
	defer recordLockTable.Unlock()
	return len(recordLockTable.m)
}
