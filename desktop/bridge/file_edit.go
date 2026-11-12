// file_edit.go —— 内嵌编辑器的读写通道 + 外部变更推送。
//
// 三条命令与一条事件（协议口径见 docs/creator-mode-v0.1/01-design.md §4.5）：
//
//	file_open  {path}                    → {ok, path, content, hash, size, mtime, truncated?, binary?}
//	file_save  {path, content, baseHash} → {ok, hash, mtime}；错误码 conflict/not_found/too_large/denied
//	file_close {path}                    → 只退订（无写副作用）
//	file_changed（事件）                 → {path, hash?, by:"external"}
//
// 三条不变量（改这个文件前先读）：
//
//	① 路径口径与 file_preview **同一条** resolveWorkspacePath：越界、受保护目录
//	  （~/.go-code）的拒绝理由与预览通路逐字一致 —— 同一路径不该因为「是编辑还是预览」
//	  得到两种说法。
//	② 保存的冲突判定必须在 **flock 内**完成：锁 → 重读磁盘算 hash → 比对 baseHash →
//	  原子替换。锁外比对等于「读完再写」的经典丢更新窗口（期间 agent 的一次 edit_file
//	  会被静默覆盖）。锁与 tools/builtin 的 edit_file/write_file 是**同一把**（internal/atomicfile）。
//	③ 外部变更只 watch **已打开文件所在的目录**（不是整个 workspace）：atomicfile 的写是
//	  「临时文件 + rename」，rename 会让按文件挂的 watch 失效；按目录挂 + 精确匹配路径，
//	  既能覆盖 rename 也能覆盖新建/删除。自己的 save 由 2s 静默窗抑制，不回推给前端。
//	④ 打开集合有上限（maxOpenFiles）：超限按 LRU 淘汰「只有一个持有者」的路径；
//	  refs>=2（两个标签都开着）永不淘汰 —— 摘掉跟踪等于让人正在编辑的文件失去变更
//	  推送与写授权，比多占几十个 map 条目贵得多。全部多持有则允许临时超限。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/seven7628/hai-harness/internal/atomicfile"
	"github.com/seven7628/hai-harness/tools/builtin"
)

const (
	// maxEditBytes 编辑器单文件上限：**沿用 file_preview 的口径**（同一个常量，不另立一份）。
	// 超限时 file_open 只回前 maxPreviewBytes 字节 + truncated，且该路径被标为只读 ——
	// 把截断后的 buffer 写回去等于替用户截断文件，这是不可撤销的数据破坏。
	maxEditBytes = maxPreviewBytes

	// fileChangeDebounce 外部变更去抖：一次 save 在 fsnotify 上通常来 2-3 个事件
	//（CREATE + WRITE + RENAME），300ms 静默期内合并成一次推送。
	fileChangeDebounce = 300 * time.Millisecond

	// fileSaveQuietWindow 自己 save 触发的写不回调前端的静默窗：记「最近一次保存的
	// path+hash」，窗内同 hash 的事件直接丢弃（同 hash = 内容没变 = 就是我自己写的那次）。
	fileSaveQuietWindow = 2 * time.Second

	// maxOpenFiles 打开集合的条目上限（LRU；与 03-code-plan §2.7 的 fileOpenCap 同值）。
	// 上限保护的是**跟踪表**（每条目挂着目录 watch 引用 + 一份 hash），不是编辑器标签数：
	// 前端同时开超过 64 个文件时，最久没动且只有一个持有者的那些会被静默退订。
	maxOpenFiles = 64

	// fileHashChunk 流式 hash/读内容的每次读块大小（4.5：大文件不整读进内存）。
	fileHashChunk = 32 * 1024
	// fileBinaryHead 二进制判定只读头部（与 builtin.LookBinary 的判据窗口同尺寸）。
	fileBinaryHead = 8 * 1024
)

// openFileState 一个「已被编辑器打开」的文件的跟踪项。
//
// refs 是引用计数而不是布尔：同一个文件可能被两个标签打开（file{mode:edit} 的身份是
// (path, mode)，但宿主可以同时开 preview 与 edit 两个实参相同的面板），关掉其中一个
// 不该让另一个失去变更推送。
type openFileState struct {
	refs     int
	hash     string // 最近一次确认的内容 sha256（hex）
	readonly string // "" | "binary" | "too_large"（只读原因；非空时拒绝 file_save）
	gone     bool   // 已推送过「消失」，避免文件被删后每轮去抖都重复推
	used     int64  // 最近一次使用（open/save）的 LRU 戳，见 fileEditor.tick
}

// savedMark 最近一次 file_save 的落点（2s 静默窗用）。
type savedMark struct {
	hash string
	at   time.Time
}

// fileEditor 打开集合 + 目录监听器（进程级单例，挂在 manager 上）。
//
// 生命周期：首次 file_open 时懒建（ctx 同时是命令侧 IO 的取消源）；进程退出时
// manager.stopFileWatches() → stop() 取消 ctx 并等 goroutine 退出。stop 之后不可重启
// —— 退出路径上不该再有新命令。
type fileEditor struct {
	m      *manager
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	fsn     *fsnotify.Watcher
	done    chan struct{}
	dirs    map[string]int            // 目录 → 引用计数（同一目录只挂一次 watch）
	files   map[string]*openFileState // 绝对路径 → 打开态
	recent  map[string]savedMark      // 绝对路径 → 最近一次保存（静默窗）
	timers  map[string]*time.Timer    // 绝对路径 → 去抖定时器
	tick    int64                     // LRU 单调戳（open/save 递增）
	lastErr string
}

func newFileEditor(m *manager) *fileEditor {
	ctx, cancel := context.WithCancel(context.Background())
	return &fileEditor{
		m:      m,
		ctx:    ctx,
		cancel: cancel,
		dirs:   map[string]int{},
		files:  map[string]*openFileState{},
		recent: map[string]savedMark{},
		timers: map[string]*time.Timer{},
	}
}

// fileEditor 取（或懒建）进程级 fileEditor。
//
// 不用 m.mu：那是 workspaces 表的锁，而 watcher 的 emit 路径也可能需要它（emitApp →
// mobile 广播），两把锁混在一起迟早死锁。fileEdMu 只保护这一个指针。
func (m *manager) fileEditor() *fileEditor {
	m.fileEdMu.Lock()
	defer m.fileEdMu.Unlock()
	if m.fileEd == nil {
		m.fileEd = newFileEditor(m)
	}
	return m.fileEd
}

// stopFileWatches 停掉文件监听（进程退出 / 测试收尾）。幂等。
// 不持 manager 的其它锁调用：stop 会等 goroutine 退出，期间 goroutine 可能正在 emit。
func (m *manager) stopFileWatches() {
	m.fileEdMu.Lock()
	e := m.fileEd
	m.fileEdMu.Unlock()
	if e != nil {
		e.stop()
	}
}

// —— 命令实现 ——

// handleFileOpen 读原文 + sha256 并登记打开集合。
//
// 返回的 path 是**解析后的绝对路径**（不是入参原样）：file_changed 事件里带的是绝对路径，
// 前端要靠它把「我打开的那个文件」和「外部改了哪个文件」对上。
func (m *manager) handleFileOpen(c command, ws string) {
	raw := str(c.Payload, "path")
	abs, e, ok := m.editPath(c, ws, raw)
	if !ok {
		return
	}
	content, hash, size, mtimeMs, binary, truncated, err := readForEdit(e.ctx, abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.respCode(c, false, "文件不存在: "+abs, "not_found", nil)
			return
		}
		m.respCode(c, false, "无法读取文件: "+err.Error(), "denied", nil)
		return
	}
	readonly := ""
	switch {
	case binary:
		readonly = "binary"
	case truncated:
		readonly = "too_large"
	}
	e.track(abs, hash, readonly)
	m.resp(c, true, "", map[string]any{
		"ok": true, "path": abs, "content": content, "hash": hash,
		"size": size, "mtime": mtimeMs, "truncated": truncated, "binary": binary,
	})
}

// handleFileSave 锁内比对 baseHash → 原子替换。
//
// baseHash 为空 = **无条件写**，但不是隐式放行：路径必须是 file_open 打开过的
// （= 用户授权过的），或调用方显式带 allowNew（「另存为」/新建；前端首次保存带的是
// 真实 hash，走不到这里）。否则任意路径都能被一条命令覆盖，等于给渲染层一个无授权写接口。
func (m *manager) handleFileSave(c command, ws string) {
	raw := str(c.Payload, "path")
	content := str(c.Payload, "content")
	baseHash := str(c.Payload, "baseHash")
	allowNew := c.Payload["allowNew"] == true

	if len(content) > maxEditBytes {
		m.respCode(c, false, fmt.Sprintf("内容超限（%s > %s），拒绝写入", humanBytes(len(content)), humanBytes(maxEditBytes)), "too_large", nil)
		return
	}
	abs, e, ok := m.editPath(c, ws, raw)
	if !ok {
		return
	}
	if state, opened := e.expect(abs); opened {
		switch state.readonly {
		case "binary":
			m.respCode(c, false, "二进制文件不支持文本写回: "+abs, "denied", nil)
			return
		case "too_large":
			m.respCode(c, false, fmt.Sprintf("文件超过 %s，本次是截断打开，拒绝写回（会用截断内容覆盖原文件）", humanBytes(maxEditBytes)), "too_large", nil)
			return
		}
	} else if !allowNew {
		m.respCode(c, false, "未打开的文件不允许写入（先 file_open，或带 allowNew 显式新建）: "+abs, "denied", nil)
		return
	}

	// 锁在**读 hash 之前**获取、rename 之后释放：锁外比对是典型的丢更新窗口。
	lock, err := atomicfile.LockPath(abs)
	if err != nil {
		if errors.Is(err, atomicfile.ErrLocked) {
			m.respCode(c, false, "文件正被另一个写者占用（agent 正在写入），本次保存未落盘，请稍后重试", "conflict", nil)
			return
		}
		m.respCode(c, false, "无法锁定文件: "+err.Error(), "denied", nil)
		return
	}
	if lock != nil {
		defer lock.Close() // Close 即释放 flock
	}

	curHash, _, curErr := hashFile(e.ctx, abs)
	switch {
	case errors.Is(curErr, os.ErrNotExist):
		if baseHash != "" {
			m.respCode(c, false, "文件已不存在（可能被移动或删除），未写入", "not_found", nil)
			return
		}
	case curErr != nil:
		m.respCode(c, false, "无法读取当前文件: "+curErr.Error(), "denied", nil)
		return
	case baseHash != "" && curHash != baseHash:
		// 冲突响应的 data 里带 currentHash：前端据此展示「磁盘上现在是哪一版」，
		// 冲突条不需要再发一次 file_open 才知道。
		m.respCode(c, false, "文件已被外部修改，本次保存未写入（baseHash 不匹配）", "conflict",
			map[string]any{"path": abs, "currentHash": curHash})
		return
	}

	if err := atomicfile.ReplaceLocked(lock, abs, content); err != nil {
		m.respCode(c, false, "写入失败: "+err.Error(), "write_failed", nil)
		return
	}
	newHash := hashBytes([]byte(content))
	e.noteSave(abs, newHash)
	m.resp(c, true, "", map[string]any{
		"ok": true, "path": abs, "hash": newHash, "size": len(content), "mtime": fileMtimeMs(abs),
	})
}

// handleFileClose 只退订（无写副作用）。幂等：没打开过的路径也回 ok ——
// 前端卸载/关标签的清理路径不该因为「已经关了」而报错。
func (m *manager) handleFileClose(c command, ws string) {
	raw := str(c.Payload, "path")
	abs, e, ok := m.editPath(c, ws, raw)
	if !ok {
		return
	}
	e.drop(abs)
	m.resp(c, true, "", map[string]any{"ok": true, "path": abs})
}

// editPath 三条编辑命令共用的路径解析：workspace 锚定 + 越界/受保护目录拒绝。
// 拒绝理由与 file_preview 逐字一致（同一个 resolveWorkspacePath），失败时已回错误响应。
func (m *manager) editPath(c command, ws, raw string) (string, *fileEditor, bool) {
	if raw == "" {
		m.respCode(c, false, "path 为空", "denied", nil)
		return "", nil, false
	}
	if ws == "" && !filepath.IsAbs(raw) {
		m.respCode(c, false, "workspace 为空", "denied", nil)
		return "", nil, false
	}
	abs, err := resolveWorkspacePath(ws, raw)
	if err != nil {
		m.respCode(c, false, err.Error(), "denied", nil)
		return "", nil, false
	}
	return abs, m.fileEditor(), true
}

// —— 打开集合 ——

// track 登记一次打开（引用计数 +1）。重复 open 同一个文件只加引用 + 刷 LRU，
// **不再重复挂目录 watch**：目录计数按「每条目一次」记，重复记会让 Remove 永远配不平
// （开 3 次只减 1 次 → 目录 watch 泄漏到进程退出）。
func (e *fileEditor) track(abs, hash, readonly string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.files[abs]; st != nil {
		st.refs++
		st.hash = hash
		st.readonly = readonly
		st.gone = false
		st.used = e.tickLocked()
		return
	}
	e.files[abs] = &openFileState{refs: 1, hash: hash, readonly: readonly, used: e.tickLocked()}
	e.watchDirLocked(filepath.Dir(abs))
	e.evictLocked(abs)
}

// drop 退订一次打开（引用计数 -1），归零即移除并解除目录 watch。
func (e *fileEditor) drop(abs string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.files[abs]
	if st == nil {
		return
	}
	st.refs--
	if st.refs > 0 {
		return // 还有别的标签开着：保留跟踪，继续推外部变更
	}
	e.removeLocked(abs)
}

// tickLocked 取下一个 LRU 戳。调用方持 e.mu。
func (e *fileEditor) tickLocked() int64 {
	e.tick++
	return e.tick
}

// evictLocked 把打开集合压回 maxOpenFiles 以内（keep = 刚插入的路径，永不淘汰）。
// 调用方持 e.mu。
//
// 可淘汰集合 = 只有一个持有者的条目（refs == 1；refs == 0 的条目不会留在表里 ——
// file_close 归零即 removeLocked）。refs >= 2 的路径**永远跳过**：摘掉一个两标签
// 都开着的文件，会让仍在编辑的那个静默失去变更推送与写授权。若剩下的全是多持有者，
// 直接放行超限 —— 宁可多占几十个条目，也不误退订正在编辑的路径。
func (e *fileEditor) evictLocked(keep string) {
	for len(e.files) > maxOpenFiles {
		victim, oldest := "", int64(0)
		for p, st := range e.files {
			if p == keep || st.refs >= 2 {
				continue
			}
			if victim == "" || st.used < oldest {
				victim, oldest = p, st.used
			}
		}
		if victim == "" {
			return // 全都在编辑：临时超限
		}
		e.removeLocked(victim)
	}
}

// removeLocked 摘掉一个条目并释放它占的目录引用与去抖定时器。调用方持 e.mu。
func (e *fileEditor) removeLocked(abs string) {
	delete(e.files, abs)
	delete(e.recent, abs)
	if t := e.timers[abs]; t != nil {
		t.Stop()
		delete(e.timers, abs)
	}
	e.unwatchDirLocked(filepath.Dir(abs))
}

// expect 取打开态（保存前的授权 + 只读判定用）。
func (e *fileEditor) expect(abs string) (*openFileState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.files[abs]
	if !ok {
		return nil, false
	}
	cp := *st
	return &cp, true
}

// noteSave 记录一次成功保存：更新跟踪 hash + 打静默窗标记。
// 静默窗的意义是「这次 fsnotify 事件是我自己写的」，不是「这段时间忽略一切」——
// 窗内来了**不同** hash 的外部写照样推（同 hash 才丢）。
func (e *fileEditor) noteSave(abs, hash string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recent[abs] = savedMark{hash: hash, at: time.Now()}
	if st := e.files[abs]; st != nil {
		st.hash = hash
		st.gone = false
		st.used = e.tickLocked() // 保存也是「在用」
	}
}

// —— 目录监听 ——

// watchDirLocked 增加一个目录引用（首次挂 watch）。调用方持 e.mu。
func (e *fileEditor) watchDirLocked(dir string) {
	if e.ctx.Err() != nil {
		return // 已停：退出路径上不再挂新 watch
	}
	if e.dirs[dir] > 0 {
		e.dirs[dir]++
		return
	}
	e.dirs[dir] = 1
	if err := e.ensureWatcherLocked(); err != nil {
		// 监听不可用不影响读写，只是没有外部变更推送（用户仍能收到保存冲突的提示）。
		e.lastErr = "watch: " + err.Error()
		fmt.Fprintf(os.Stderr, "[file_edit] 外部变更监听不可用: %v\n", err)
		return
	}
	if err := e.fsn.Add(dir); err != nil {
		e.lastErr = "watch add: " + err.Error()
		fmt.Fprintf(os.Stderr, "[file_edit] 目录监听失败: %v\n", err)
	}
}

// unwatchDirLocked 解除一个目录引用（归零才真的 Remove）。调用方持 e.mu。
func (e *fileEditor) unwatchDirLocked(dir string) {
	n, ok := e.dirs[dir]
	if !ok {
		return
	}
	if n > 1 {
		e.dirs[dir] = n - 1
		return
	}
	delete(e.dirs, dir)
	if e.fsn != nil {
		_ = e.fsn.Remove(dir) // 已失效（目录被删）：忽略错误
	}
}

// ensureWatcherLocked 懒建 fsnotify + 事件循环。调用方持 e.mu。
func (e *fileEditor) ensureWatcherLocked() error {
	if e.fsn != nil {
		return nil
	}
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	e.fsn = fsn
	done := make(chan struct{})
	e.done = done
	// fsn/done 以参数传入循环：字段会被 stop() 置 nil，goroutine 里再读字段有竞态。
	go e.loop(e.ctx, fsn, done)
	return nil
}

// stop 取消 ctx、停掉全部去抖定时器并等 goroutine 退出（幂等，stop 后不可重启）。
func (e *fileEditor) stop() {
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.cancel, e.done, e.fsn = nil, nil, nil
	for p, t := range e.timers {
		t.Stop()
		delete(e.timers, p)
	}
	e.files = map[string]*openFileState{}
	e.dirs = map[string]int{}
	e.mu.Unlock()
	if cancel != nil {
		cancel() // loop 收到 ctx.Done → 关 fsn → 退出
	}
	if done != nil {
		<-done
	}
}

// loop 事件循环：命中已打开文件 → 去抖 → 比对 hash → 推送。
func (e *fileEditor) loop(ctx context.Context, fsn *fsnotify.Watcher, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			_ = fsn.Close()
			return
		case ev, ok := <-fsn.Events:
			if !ok {
				return
			}
			e.onEvent(ev)
		case err, ok := <-fsn.Errors:
			if !ok {
				return
			}
			// 队列溢出/句柄失效：事件可能已丢 → 对打开集合做一次全量复核
			//（宁可多推一次，也不能让用户对着一个已被改写的文件继续编辑）。
			e.mu.Lock()
			e.lastErr = "watch: " + err.Error()
			paths := make([]string, 0, len(e.files))
			for p := range e.files {
				paths = append(paths, p)
			}
			e.mu.Unlock()
			for _, p := range paths {
				e.mu.Lock()
				e.scheduleLocked(p)
				e.mu.Unlock()
			}
		}
	}
}

// onEvent 只关心「已打开的文件」的写/建/删/改名事件（目录里的其它文件一律忽略）。
func (e *fileEditor) onEvent(ev fsnotify.Event) {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return
	}
	name := filepath.Clean(ev.Name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.files[name]; !ok {
		return
	}
	e.scheduleLocked(name)
}

// scheduleLocked 重置该路径的去抖定时器。调用方持 e.mu。
func (e *fileEditor) scheduleLocked(abs string) {
	if e.ctx.Err() != nil {
		return
	}
	if t := e.timers[abs]; t != nil {
		t.Stop()
	}
	e.timers[abs] = time.AfterFunc(fileChangeDebounce, func() {
		e.mu.Lock()
		delete(e.timers, abs)
		e.mu.Unlock()
		e.check(abs)
	})
}

// check 去抖到点：算当前 hash，与跟踪值比 → 变了才推（并跳静默窗内的自家保存）。
func (e *fileEditor) check(abs string) {
	if e.ctx.Err() != nil {
		return
	}
	hash, _, err := hashFile(e.ctx, abs)

	e.mu.Lock()
	st, ok := e.files[abs]
	if !ok {
		e.mu.Unlock()
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		if st.gone {
			e.mu.Unlock()
			return
		}
		st.gone = true
		st.hash = ""
		e.mu.Unlock()
		e.emit(map[string]any{"path": abs, "by": "external", "deleted": true})
		return
	}
	if err != nil {
		e.mu.Unlock()
		return // 读不出来（权限/占用）：不推，等下一个事件
	}
	quiet := false
	if mark, ok := e.recent[abs]; ok && hash == mark.hash && time.Since(mark.at) < fileSaveQuietWindow {
		quiet = true
	}
	changed := hash != st.hash
	st.gone = false
	if changed && !quiet {
		st.hash = hash
	}
	e.mu.Unlock()

	if !changed || quiet {
		return
	}
	e.emit(map[string]any{"path": abs, "by": "external", "hash": hash})
}

// emit 走 bridge 既有事件通道（emitApp：stdout + 手机端广播）。
// 事件不带 workspace/session_id：文件是工作区级资源，前端按绝对路径匹配。
func (e *fileEditor) emit(payload map[string]any) {
	if e.m == nil {
		return
	}
	v := map[string]any{"event_type": "file_changed"}
	for k, val := range payload {
		v[k] = val
	}
	e.m.emitApp(v)
}

// —— 读取与 hash ——

// readForEdit 读文件供编辑器：流式（不整读大文件）+ 全量 sha256 + 二进制/截断判定。
//
// hash 覆盖**整份文件**（哪怕 content 被截断）：保存时的冲突检测比的是磁盘真相，
// 拿截断内容的 hash 去比会让「文件被外部追加了一段」这种情况误判为冲突。
func readForEdit(ctx context.Context, p string) (content string, hash string, size, mtimeMs int64, binary, truncated bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", 0, 0, false, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", "", 0, 0, false, false, err
	}
	if info.IsDir() {
		return "", "", 0, 0, false, false, fmt.Errorf("path 是目录，不是文件: %s", p)
	}

	h := sha256.New()
	limit := int64(maxEditBytes)
	head := make([]byte, 0, fileBinaryHead)
	out := make([]byte, 0, min64(info.Size(), limit))
	buf := make([]byte, fileHashChunk)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return "", "", 0, 0, false, false, cerr
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			size += int64(n)
			_, _ = h.Write(buf[:n])
			if len(head) < fileBinaryHead {
				head = append(head, buf[:minInt(n, fileBinaryHead-len(head))]...)
			}
			if int64(len(out)) < limit {
				out = append(out, buf[:minInt(n, int(limit)-len(out))]...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", "", 0, 0, false, false, rerr
		}
	}
	truncated = size > limit
	binary = builtin.LookBinary(head)
	if binary {
		out = nil // 二进制不往编辑器里塞字符串（前端只看 binary=true 的提示）
	}
	return string(out), hex.EncodeToString(h.Sum(nil)), size, info.ModTime().UnixMilli(), binary, truncated, nil
}

// hashFile 流式算文件 sha256（读不出来时返回的 hash 为空）。size 为实际读取字节数。
func hashFile(ctx context.Context, p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, fileHashChunk)
	var size int64
	for {
		if cerr := ctx.Err(); cerr != nil {
			return "", size, cerr
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			size += int64(n)
			_, _ = h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", size, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// hashBytes 内存内容的 sha256（保存成功后算新 hash 用）。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileMtimeMs 文件 mtime（unix 毫秒，与 bridge 其它时间字段同口径）；取不到返回 0。
func fileMtimeMs(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
