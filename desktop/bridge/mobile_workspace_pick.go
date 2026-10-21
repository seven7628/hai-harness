// mobile_workspace_pick.go —— 「让电脑弹目录选择器」。
//
// 手机上打绝对路径很痛苦，而目录选择器只存在于 Electron 主进程。链路：
//
//	手机 workspace_pick_request
//	  → bridge 发 workspace_pick_request 事件（stdout + 广播）
//	    → Electron 弹 dialog，回 workspace_pick_result {request_id, path|canceled}
//	      → bridge 落 workspaces.json，再广播 workspace_picked / workspace_pick_canceled
//
// 请求带 5 分钟 TTL：用户在电脑前没反应时，过期的回填不能悄悄加一个工作区。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

const workspacePickTTL = 5 * time.Minute

type workspacePickTable struct {
	mu      sync.Mutex
	pending map[string]time.Time
}

func newWorkspacePickTable() *workspacePickTable {
	return &workspacePickTable{pending: map[string]time.Time{}}
}

// begin 登记一个待回填请求，返回 request_id。
func (p *workspacePickTable) begin(now time.Time) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "wp_" + hex.EncodeToString(b)

	p.mu.Lock()
	defer p.mu.Unlock()
	// 顺手清过期项（请求量极小，不需要后台 janitor）
	for k, at := range p.pending {
		if now.Sub(at) > workspacePickTTL {
			delete(p.pending, k)
		}
	}
	p.pending[id] = now
	return id
}

// consume 原子地取走一个请求：只有第一次回填有效（防重放/重复点击）。
func (p *workspacePickTable) consume(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.pending[id]
	if !ok {
		return false
	}
	delete(p.pending, id)
	return now.Sub(at) <= workspacePickTTL
}

// emitApp 应用级事件：写 stdout（桌面能看到）并广播给所有手机订阅者。
func (m *manager) emitApp(v map[string]any) {
	if m == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	m.out.writeRaw(b)
	if h := m.mobileHub(); h != nil {
		h.broadcastGlobal(b)
	}
}
