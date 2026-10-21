// workspace_registry.go —— 工作区清单的单一真相源（~/.go-code/workspaces.json）。
//
// 背景（TECH_PLAN §2）：桌面端项目树原本只存在渲染进程 localStorage，
// bridge 侧没有清单。手机要「添加/切换工作区」，必须先有一个两端都能读的源，
// 否则会出现「手机加了、电脑看不到」。迁移方式：渲染进程首次用 workspace_sync
// 把 localStorage 里的项目灌进来，之后以本文件为准（localStorage 降级为缓存）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const mobileMaxWorkspaces = 50

var errWorkspaceInvalid = errors.New("工作区路径无效")

type workspaceEntry struct {
	Path       string `json:"path"`
	Title      string `json:"title,omitempty"`
	Pinned     bool   `json:"pinned,omitempty"`
	LastOpened int64  `json:"last_opened,omitempty"`
}

type workspaceRegistry struct {
	mu      sync.Mutex
	path    string
	entries []workspaceEntry
}

func newWorkspaceRegistry(path string) *workspaceRegistry {
	r := &workspaceRegistry{path: path}
	r.load()
	return r
}

func (r *workspaceRegistry) load() {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var doc struct {
		Workspaces []workspaceEntry `json:"workspaces"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return // 坏文件视为空；下一次写入会覆盖（清单可重建，不值得为它卡启动）
	}
	seen := map[string]bool{}
	for _, e := range doc.Workspaces {
		p := normalizeWorkspacePath(e.Path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		e.Path = p
		if e.Title == "" {
			e.Title = filepath.Base(p)
		}
		r.entries = append(r.entries, e)
	}
}

func (r *workspaceRegistry) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(map[string]any{"workspaces": r.entries}, "", "  ")
	if err != nil {
		return err
	}
	return writeMobileAtomic(r.path, b)
}

func (r *workspaceRegistry) list() []workspaceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]workspaceEntry, len(r.entries))
	copy(out, r.entries)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].LastOpened != out[j].LastOpened {
			return out[i].LastOpened > out[j].LastOpened
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func (r *workspaceRegistry) add(path string) (workspaceEntry, error) {
	p := normalizeWorkspacePath(path)
	if p == "" {
		return workspaceEntry{}, errWorkspaceInvalid
	}
	if err := validateWorkspaceDir(p); err != nil {
		return workspaceEntry{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.Path == p {
			return e, nil // 幂等：重复添加不报错、不重复落盘
		}
	}
	if len(r.entries) >= mobileMaxWorkspaces {
		return workspaceEntry{}, fmt.Errorf("工作区数量已达上限 %d", mobileMaxWorkspaces)
	}
	e := workspaceEntry{Path: p, Title: filepath.Base(p), LastOpened: time.Now().Unix()}
	r.entries = append(r.entries, e)
	if err := r.saveLocked(); err != nil {
		return workspaceEntry{}, err
	}
	return e, nil
}

func (r *workspaceRegistry) remove(path string) bool {
	p := normalizeWorkspacePath(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.entries {
		if e.Path == p {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			_ = r.saveLocked()
			return true
		}
	}
	return false
}

// sync 用外部清单（渲染进程 localStorage）补全本文件：只增不减。
//
// 为什么只增不减：手机可能刚加过一个工作区，桌面端 localStorage 还是旧的；
// 一次性同步若做「以外部为准」，会把手机刚加的删掉。
func (r *workspaceRegistry) sync(paths []string) []workspaceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	have := map[string]bool{}
	for _, e := range r.entries {
		have[e.Path] = true
	}
	changed := false
	for _, raw := range paths {
		p := normalizeWorkspacePath(raw)
		if p == "" || have[p] || len(r.entries) >= mobileMaxWorkspaces {
			continue
		}
		have[p] = true
		r.entries = append(r.entries, workspaceEntry{Path: p, Title: filepath.Base(p)})
		changed = true
	}
	if changed {
		_ = r.saveLocked()
	}
	return r.listLocked()
}

func (r *workspaceRegistry) listLocked() []workspaceEntry {
	out := make([]workspaceEntry, len(r.entries))
	copy(out, r.entries)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func normalizeWorkspacePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

func validateWorkspaceDir(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: %v", errWorkspaceInvalid, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: 不是目录", errWorkspaceInvalid)
	}
	return nil
}

// ---------- manager 侧入口（dispatch 与 mobile 共用） ----------

func (m *manager) mobileHub() *mobileHub {
	if m == nil || m.mobile == nil {
		return nil
	}
	return m.mobile.hub
}

func (m *manager) workspaceEntries() []workspaceEntry {
	if m == nil || m.wsReg == nil {
		return []workspaceEntry{}
	}
	return m.wsReg.list()
}

func (m *manager) workspaceAdd(path string) (workspaceEntry, error) {
	if m == nil || m.wsReg == nil {
		return workspaceEntry{}, errWorkspaceInvalid
	}
	return m.wsReg.add(path)
}

func (m *manager) workspaceRemove(path string) bool {
	if m == nil || m.wsReg == nil {
		return false
	}
	return m.wsReg.remove(path)
}

func (m *manager) workspaceSync(paths []string) []workspaceEntry {
	if m == nil || m.wsReg == nil {
		return []workspaceEntry{}
	}
	return m.wsReg.sync(paths)
}
