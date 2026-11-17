// surface_watch.go —— CreateMode 插件的**热加载**（fsnotify）。
//
// 背景（01-design §631 的原裁决是「不做 HMR，改代码重装一次」）：CreateMode 的实际循环是
// 「写 panel.js → 装上 → 在面板里点 → 再改」，一次重装要过审批 + 重启插件 runtime，成本
// 远高于「保存即生效」。2026-11 用户拍板要热加载，于是这里补上 watcher；安全边界不变：
//
//	· 只有 **dev 安装**（surface_install 的 dev:true）会被盯 —— 拷快照的普通安装仍然
//	  「装上的字节 = 校验过的字节」，源码目录后来怎么变都与运行中的插件无关。
//	· 只认 index 里 `dev:true` 且来源是软链的条目；卸载/改成普通安装后立即失去监听。
//	· 只更新 hash 并推 surfaces_changed —— 不走安装器、不重新校验、不重启插件 runtime。
//	  校验发生在安装那一刻（dev 安装仍会跑完整 validateSurfaceSource）；热加载阶段改的是
//	  **同一个已批准的目录**，与「编辑器保存后重新加载页面」同一语义。
//
// 监听哪些目录：store 根（新装/卸载）+ 每个 dev 插件的**真实目录**及其子目录（跳过
// node_modules / .git / 点目录，上限 200 个目录）。为什么用真实目录：<root>/<id> 是软链，
// fsnotify 不跟随软链；对链接本身挂 watch 只能看到链接被替换，看不到面板文件的写入。
//
// 为什么 debounce 250ms：一次编辑器保存通常产生 2-3 个事件（CREATE/WRITE/CHMOD），
// 逐个处理会让同一个面板重挂 2-3 次（每次都是一次 import + apply）。
package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// surfaceWatchDebounce 一次保存的事件合并窗口（见文件头）。
	surfaceWatchDebounce = 250 * time.Millisecond
	// surfaceWatchMaxDirs 单个 dev 插件最多监听多少个子目录（防止把一个大仓库整个挂上）。
	surfaceWatchMaxDirs = 200
)

// ---- manager 接线 ----

// startSurfaceWatch 启动热加载 watcher（幂等；没有 dev 插件时只监听 store 根）。
func (m *manager) startSurfaceWatch() {
	if m == nil {
		return
	}
	if m.surfaceWatch == nil {
		m.surfaceWatch = newSurfaceWatcher(&m.surfaces, m.PushSurfacesChanged)
	}
	m.surfaceWatch.Start()
}

// syncSurfaceWatch 安装/卸载后对齐监听集合（dev 安装成功后必须调，否则新软链不在监听里）。
func (m *manager) syncSurfaceWatch() {
	if m == nil {
		return
	}
	if w := m.surfaceWatch; w != nil {
		w.Sync()
	}
}

func (m *manager) stopSurfaceWatch() {
	if m == nil {
		return
	}
	if w := m.surfaceWatch; w != nil {
		w.Stop()
	}
}

type surfaceWatcher struct {
	store *surfaceStore
	push  func()

	mu      sync.Mutex
	fsn     *fsnotify.Watcher
	dirs    map[string]string // 已监听的**真实**目录 → 插件 id（"" = store 根）
	dirty   map[string]bool   // 待重算 hash 的插件 id
	resync  bool              // 需要重建监听（新装/卸载/新子目录）
	timer   *time.Timer
	stop    chan struct{}
	stopped bool
}

func newSurfaceWatcher(store *surfaceStore, push func()) *surfaceWatcher {
	return &surfaceWatcher{
		store: store,
		push:  push,
		dirs:  map[string]string{},
		dirty: map[string]bool{},
		stop:  make(chan struct{}),
	}
}

// Start 幂等：已经启动过就什么都不做（serve 只调一次；测试可重复调）。
func (w *surfaceWatcher) Start() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.fsn != nil || w.stopped {
		w.mu.Unlock()
		return
	}
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		w.mu.Unlock()
		log.Printf("surfaces: 热加载 watcher 启动失败（dev 插件不会自动重载）：%v", err)
		return
	}
	w.fsn = fsn
	w.mu.Unlock()

	if err := os.MkdirAll(w.store.Dir(), surfaceDirMode); err != nil {
		log.Printf("surfaces: 建 surfaces 目录失败（watcher）：%v", err)
	}
	w.Sync()
	go w.loop(fsn)
}

// Stop 停止监听（进程退出 / 测试收尾）。幂等。
func (w *surfaceWatcher) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	fsn := w.fsn
	w.mu.Unlock()
	close(w.stop)
	if fsn != nil {
		_ = fsn.Close()
	}
}

// Sync 重建监听集合（安装/卸载成功后由 manager 调；内部也会在根目录变化时自己调）。
func (w *surfaceWatcher) Sync() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.syncLocked()
}

// syncLocked 把「应该监听的目录」对齐到 fsnotify。
//
// 调用方持 w.mu（fsnotify 的 Add/Remove 自身线程安全；这里串行化的是 dirs 这份账）。
func (w *surfaceWatcher) syncLocked() {
	if w.fsn == nil || w.stopped {
		return
	}
	root := w.store.Dir()
	desired := map[string]string{root: ""} // 根先挂上：新装/卸载的目录级事件
	for _, it := range w.store.List(false).Items {
		if !it.Dev {
			continue
		}
		link := filepath.Join(root, it.ID)
		real, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue // 链接断了（源目录被删）：等下一次 Sync/根事件
		}
		desired[real] = it.ID
		// 子目录：面板可能拆成 ./lib/a.js。跳过噪声目录（dev 的源通常是仓库）。
		added := 0
		_ = filepath.WalkDir(real, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // 单目录不可读不影响其它
			}
			if !d.IsDir() || p == real {
				return nil
			}
			if surfaceNoiseDir(d.Name()) {
				return filepath.SkipDir
			}
			if added >= surfaceWatchMaxDirs {
				return filepath.SkipAll
			}
			desired[p] = it.ID
			added++
			return nil
		})
	}
	for p, id := range desired {
		if old, ok := w.dirs[p]; ok && old == id {
			continue
		}
		if err := w.fsn.Add(p); err != nil {
			// 监听失败的形态是「保存没反应」：记一条，排障时能直接看出是哪个目录。
			log.Printf("surfaces: watch %s（插件 %s）失败：%v", p, id, err)
			continue
		}
		w.dirs[p] = id
	}
	for p := range w.dirs {
		if _, keep := desired[p]; keep {
			continue
		}
		_ = w.fsn.Remove(p)
		delete(w.dirs, p)
	}
}

func (w *surfaceWatcher) loop(fsn *fsnotify.Watcher) {
	for {
		select {
		case <-w.stop:
			return
		case ev, ok := <-fsn.Events:
			if !ok {
				return
			}
			w.onEvent(ev.Name)
		case err, ok := <-fsn.Errors:
			if !ok {
				return
			}
			log.Printf("surfaces: watcher 错误（热加载可能漏一次）：%v", err)
		}
	}
}

func (w *surfaceWatcher) onEvent(name string) {
	name = filepath.Clean(name)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	id, ok := w.idOfLocked(name)
	if !ok {
		return
	}
	// store 根目录下的 index.json / audit / 原子写的临时文件（`.atomic-*`）是我们自己写的：
	// 忽略，否则每次写索引都触发一轮 resync。
	if id == "" {
		base := filepath.Base(name)
		if base == surfaceIndexName || base == surfaceBakName || base == surfaceAuditName ||
			strings.HasPrefix(base, ".") || strings.Contains(base, ".corrupt-") {
			return
		}
	}
	if id == "" {
		w.resync = true
	} else {
		w.dirty[id] = true
	}
	w.scheduleLocked()
}

// idOfLocked 事件路径 → 插件 id（最长前缀匹配；"" = store 根）。name 自身可能是一个
// 目录（新建 lib/），所以先看它、再看父目录。
func (w *surfaceWatcher) idOfLocked(name string) (string, bool) {
	if id, ok := w.dirs[name]; ok {
		return id, true
	}
	for p := filepath.Dir(name); ; p = filepath.Dir(p) {
		if id, ok := w.dirs[p]; ok {
			return id, true
		}
		next := filepath.Dir(p)
		if next == p {
			return "", false
		}
	}
}

func (w *surfaceWatcher) scheduleLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(surfaceWatchDebounce, w.flush)
}

// flush 合并窗口结束：重算 dirty 插件的 hash；值变了才更新 index + 推 surfaces_changed。
func (w *surfaceWatcher) flush() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	dirty := w.dirty
	w.dirty = map[string]bool{}
	needResync := w.resync
	w.resync = false
	w.timer = nil
	if needResync {
		w.syncLocked()
	}
	w.mu.Unlock()

	root := w.store.Dir()
	ids := make([]string, 0, len(dirty))
	for id := range dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 固定顺序：两次面板重挂的先后不影响结果，但日志/测试可重复
	changed := false
	for _, id := range ids {
		link := filepath.Join(root, id)
		// 只对「现在仍是 dev 的条目」生效：期间被卸载/改成普通安装的不该再被改写 hash。
		dev := false
		for _, it := range w.store.List(false).Items {
			if it.ID == id && it.Dev {
				dev = true
				break
			}
		}
		if !dev {
			continue
		}
		h, err := surfaceContentHash(link, true)
		if err != nil {
			continue // 源目录正在被原子替换（rename 中间态）：下一次事件会再来
		}
		if w.store.refreshDevHash(id, h) {
			changed = true
		}
	}
	// 新子目录可能刚建出来：补监听（廉价；没有变化时只是几次 map 比较）。
	w.Sync()
	if changed && w.push != nil {
		w.push()
	}
}
