// mcp_watch.go —— MCP 配置分层的文件监听（fsnotify）：改配置即热应用，无需重启。
//
// 设计要点（实测结论，见 docs 与提交说明）：
//   - **只挂目录，不挂文件**：macOS(kqueue) 实测目录监听同时覆盖三种写入方式——新建文件
//     （CREATE）、内容原地改写（WRITE，同长度也报）、编辑器原子替换 tmp+rename
//     （REMOVE + CREATE）。而挂文件本体在 rename 覆盖后指向旧 inode，必须重挂，得不偿失。
//   - **精确路径过滤**：只认三层配置文件的绝对路径（用户 settings.json、项目 .mcp.json、
//     项目 .go-code/settings.json）。{ws} 目录里同名的 settings.json 不属于任何层，不触发。
//   - **debounce**：一次保存会产生多条事件（CHMOD/WRITE/CREATE/REMOVE），静默期后只应用一次。
//   - **自愈重挂**：目录可能后建（{ws}/.go-code 首次出现）或被删后重建 → 每条事件后 + 低频
//     tick 都尝试重挂缺失目录；监听目录本身被删/改名时解除标记，等它回来再挂。
//   - **生命周期**：随工作区运行态创建启动；delete_workspace / shutdownAll 停止
//     （cancel + Close + 等 goroutine 退出）。stop 幂等，**调用方不得持 m.mu/ws.mu**。
//   - **应用动作**：Loader 重读分层（坏文件沿用上次有效）→ 合并结果与上次相同时**什么都不做**
//     （不碰连接、不重建 loop）→ 变化时差分 SetConfig（变的重连、未变的保留）+ rebuildWorkspace。
//     运行中的 run 用 loop 快照（session.go:691），换 loop 不打断它；新工具面下一次运行生效。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/seven7628/hai-harness/mcp"
)

const (
	// mcpWatchDebounce 事件静默期：一次保存的多条事件合并成一次重载。
	mcpWatchDebounce = 250 * time.Millisecond
	// mcpWatchRearmInterval 重挂周期（兜底：目录后建/被删重建、watch 失效、事件队列溢出）。
	mcpWatchRearmInterval = 10 * time.Second
)

// mcpWatcher 单个工作区的 MCP 配置监听器。
type mcpWatcher struct {
	ws       string
	targets  []string // 三层配置文件的绝对路径（精确匹配）
	dirs     []string // 待挂目录（三层文件所在目录，去重）
	debounce time.Duration
	apply    func() // 重载 + 热应用（bridge 注入；测试可替换为计数/断言）

	mu      sync.Mutex
	fsn     *fsnotify.Watcher
	armed   map[string]bool
	cancel  context.CancelFunc
	done    chan struct{}
	applies int    // 已应用次数（测试断言 debounce 合并用）
	lastErr string // 最近一次监听错误（排障）
}

// newMCPWatcher 构造监听器（尚未启动）。apply = 实际重载动作。
func newMCPWatcher(ws string, apply func()) *mcpWatcher {
	w := &mcpWatcher{
		ws:       ws,
		debounce: mcpWatchDebounce,
		apply:    apply,
		armed:    map[string]bool{},
	}
	seen := map[string]bool{}
	for _, l := range mcpConfigLayers(ws) {
		if strings.TrimSpace(l.Path) == "" {
			continue
		}
		p := filepath.Clean(l.Path)
		w.targets = append(w.targets, p)
		if d := filepath.Dir(p); !seen[d] {
			seen[d] = true
			w.dirs = append(w.dirs, d)
		}
	}
	return w
}

// start 启动监听（幂等）。失败返回错误，调用方降级为「改配置需 mcp_reload」。
func (w *mcpWatcher) start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return nil
	}
	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsn = fsn
	w.armLocked() // 尽力先挂上（目录缺失的先跳过，等事件/tick）
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.cancel = cancel
	w.done = done
	// fsn/done 以参数传入：字段会被 stop() 置 nil，goroutine 内再读字段有竞态
	//（stop 先拿到锁 → 读到 nil → close(nil) panic，实测崩溃过）。
	go w.loop(ctx, fsn, done)
	return nil
}

// stop 停止监听并等 goroutine 退出（幂等）。调用方不得持 m.mu/ws.mu：
// apply 回调会取 m.mu（m.runtime），持锁等待会死锁。
func (w *mcpWatcher) stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done // 等在跑的 apply 收尾（避免「停止后仍改状态」）
}

// loop 事件循环：事件 → （自愈重挂）→ 精确匹配 → debounce 静默期 → apply。
// fsn/done 由 start 传入（不可从字段读：stop 会把字段置 nil）。
func (w *mcpWatcher) loop(ctx context.Context, fsn *fsnotify.Watcher, done chan struct{}) {
	defer close(done)

	timer := time.NewTimer(w.debounce)
	timer.Stop()
	defer timer.Stop()
	var fire <-chan time.Time // nil = 未挂起（阻塞）

	rearm := time.NewTicker(mcpWatchRearmInterval)
	defer rearm.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = fsn.Close()
			return
		case ev, ok := <-fsn.Events:
			if !ok {
				return
			}
			if !w.onEvent(ev) {
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.debounce)
			fire = timer.C
		case err, ok := <-fsn.Errors:
			if !ok {
				return
			}
			// 队列溢出/失效：全量重挂 + 重载一次（失效期间的事件可能已丢）。
			w.mu.Lock()
			w.lastErr = "watch: " + err.Error()
			for d := range w.armed {
				_ = fsn.Remove(d)
			}
			w.armed = map[string]bool{}
			w.armLocked()
			w.mu.Unlock()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.debounce)
			fire = timer.C
		case <-fire:
			fire = nil
			w.mu.Lock()
			w.applies++
			w.mu.Unlock()
			w.apply()
		case <-rearm.C:
			// 周期兜底：目录后建时（新挂上）该层内容可能已存在 → 立即重载一次。
			w.mu.Lock()
			newly := w.armLocked()
			w.mu.Unlock()
			if newly > 0 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
				fire = timer.C
			}
		}
	}
}

// onEvent 处理单条事件：自愈重挂 + 返回是否需要重载。
// 两种情况都要重载：
//  1. 命中三层配置文件（精确路径）；
//  2. **新挂上了某层目录**——目录后建时，其内部文件可能已经写好（MkdirAll + 写入几乎同时），
//     我们挂上该目录的时刻已错过那个 CREATE 事件，只能主动重读一次（否则该层永远不生效）。
func (w *mcpWatcher) onEvent(ev fsnotify.Event) bool {
	name := filepath.Clean(ev.Name)
	w.mu.Lock()
	// 监听目录本身被删/改名/重建：解除标记（否则永远收不到该目录后续事件）
	for _, d := range w.dirs {
		if name == d && w.armed[d] {
			delete(w.armed, d)
			if w.fsn != nil {
				_ = w.fsn.Remove(d) // 已失效：忽略错误
			}
		}
	}
	newly := w.armLocked()
	w.mu.Unlock()
	if newly > 0 {
		return true
	}
	for _, t := range w.targets {
		if name == t {
			return true
		}
	}
	return false
}

// armLocked 重挂缺失目录（已挂的跳过），返回本次新挂上的目录数。调用方持 w.mu。
func (w *mcpWatcher) armLocked() int {
	if w.fsn == nil {
		return 0
	}
	newly := 0
	for _, d := range w.dirs {
		if w.armed[d] {
			continue
		}
		if _, err := os.Stat(d); err != nil {
			continue // 目录还不存在：等它出现（事件/tick 再试）
		}
		if err := w.fsn.Add(d); err != nil {
			w.lastErr = "watch add: " + err.Error()
			continue
		}
		w.armed[d] = true
		newly++
	}
	return newly
}

// stats 监听统计（测试断言 debounce 合并与排障）。
func (w *mcpWatcher) stats() (applies int, armedDirs int, lastErr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applies, len(w.armed), w.lastErr
}

// —— bridge 侧：重载与应用 ——

// syncMCPLayers 重读给定分层并同步到管理器。
// layers 由调用方显式给出（两条路径的层集合不同）：
//   - 监听/手动重载：mcpConfigLayers（用户文件 + 项目两层，用户层也要重读）
//   - mcp_set（UI 保存）：projectMCPLayers + base=payload —— payload 是**最新**的用户层，
//     比用户文件新（Electron 落盘时序在后），所以这里刻意不读用户文件，否则旧文件内容
//     会盖掉刚保存的 payload。文件落盘后 watcher 会再走一次全量分层（内容相同 → 零副作用）。
//
// 返回 changed = 合并结果与上次应用不同（true 时已按需重建连接；false = 零副作用：
// 不碰连接、不重建 loop——用户层 settings.json 被无关设置改写时走这条）。
func (m *manager) syncMCPLayers(ws *workspaceRuntime, base map[string]mcp.ServerConfig, baseSource string, layers ...mcp.Layer) bool {
	res := ws.mcpCfg.LoadWithBase(base, baseSource, layers...)
	ws.mu.Lock()
	changed := !reflect.DeepEqual(ws.mcpApplied, res.Config)
	if changed {
		ws.mcpApplied = res.Config
	}
	ws.mcpLayers = res.Layers // 层状态总是更新（stale/err 可见）
	ws.mu.Unlock()
	if changed {
		ws.mcp.SetConfigWithSource(res.Config, res.Source) // 差分：变的重连、未变的保留
	}
	return changed
}

// reloadMCP 重读该工作区全部分层；内容有变化时重建会话 loop（watcher 与 mcp_reload 共用）。
// 返回是否发生变化。
func (m *manager) reloadMCP(wsPath string) bool {
	ws := m.runtime(wsPath)
	if !m.syncMCPLayers(ws, nil, "", mcpConfigLayers(wsPath)...) {
		return false
	}
	m.rebuildWorkspace(wsPath)
	return true
}

// mcpLayerStates 该工作区最近一次分层加载状态（mcp_list 展示；未创建运行态 → nil）。
func (m *manager) mcpLayerStates(wsPath string) []mcp.LayerState {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.mcpLayers
}

// startMCPWatch 启动该工作区的配置监听（失败不阻断：降级为手动 mcp_reload）。
// 调用方持 m.mu（runtime 内），此刻 ws 已登记进 m.workspaces。
func (m *manager) startMCPWatch(ws *workspaceRuntime) {
	w := newMCPWatcher(ws.path, func() {
		if m.reloadMCP(ws.path) {
			fmt.Fprintf(os.Stderr, "· MCP 配置变更已应用: %s\n", ws.path)
		}
	})
	if err := w.start(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ MCP 配置监听启动失败（改配置需 mcp_reload）: %v\n", err)
		return
	}
	ws.mcpWatch = w
}

// stopMCPWatch 停止该工作区的配置监听（幂等）。**调用方不得持 m.mu/ws.mu**：
// stop 会等正在跑的 apply 收尾，而 apply 回调要取 m.mu（m.runtime）——持锁等待即死锁。
func (ws *workspaceRuntime) stopMCPWatch() {
	if ws.mcpWatch != nil {
		ws.mcpWatch.stop()
	}
}
