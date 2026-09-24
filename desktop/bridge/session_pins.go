package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// session pin 持久化：~/.go-code/sessions/<wsKey>/sessions.json
//
// 结构：
//
//	{ "pinned": ["sid1", "sid2", ...] }   // 按 pin 时间倒序（最新在前），上限 5
//
// 纯前端展示用（左栏置顶排序），不影响会话数据本身。读写走文件，跨重启持久；
// 内存缓存避免每次 list 都读盘。上限 MAX_SESSION_PINS（与前端一致）。

const maxSessionPins = 5

// sessionPinsFile 返回某 workspace 的 pin 文件路径（<appDataDir>/sessions/<wsKey>/sessions.json）。
func sessionPinsFile(wsKey string) string {
	return filepath.Join(appDataDir(), "sessions", wsKey, "sessions.json")
}

type sessionPinsStore struct {
	mu sync.Mutex
	// key: wsKey → 已排序的 pinned session id 列表（最新在前）
	cache map[string][]string
}

var sessionPins = &sessionPinsStore{cache: map[string][]string{}}

type sessionPinsFileData struct {
	Pinned []string `json:"pinned"`
}

// load 读某 workspace 的 pinned 列表（内存缓存优先；文件缺失/损坏 → 空）。
func (s *sessionPinsStore) load(wsKey string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[wsKey]; ok {
		return v
	}
	pins := s.readFileLocked(wsKey)
	s.cache[wsKey] = pins
	return pins
}

// readFileLocked 从磁盘读（持锁调用）。
func (s *sessionPinsStore) readFileLocked(wsKey string) []string {
	data, err := os.ReadFile(sessionPinsFile(wsKey))
	if err != nil {
		return nil
	}
	var f sessionPinsFileData
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	// 去重 + 限长（防御手改/坏数据）
	seen := map[string]bool{}
	out := make([]string, 0, len(f.Pinned))
	for _, id := range f.Pinned {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxSessionPins {
			break
		}
	}
	return out
}

// set 设置某 session 的 pin 状态；返回更新后的 pinned 列表与是否超限。
// pinned=true 且已达上限 → 拒绝（返回 false），不覆盖已有 pin。
func (s *sessionPinsStore) set(wsKey, sessionID string, pinned bool) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.readFileLocked(wsKey)
	cur = s.normalizeLocked(cur)
	if pinned {
		// 已在列表：幂等（移到最前）
		for i, id := range cur {
			if id == sessionID {
				if i == 0 {
					s.cache[wsKey] = cur
					return cur, true
				}
				cur = append([]string{sessionID}, append(cur[:i:i], cur[i+1:]...)...)
				s.writeFileLocked(wsKey, cur)
				s.cache[wsKey] = cur
				return cur, true
			}
		}
		// 新增：检查上限
		if len(cur) >= maxSessionPins {
			s.cache[wsKey] = cur
			return cur, false
		}
		cur = append([]string{sessionID}, cur...)
	} else {
		out := cur[:0]
		for _, id := range cur {
			if id != sessionID {
				out = append(out, id)
			}
		}
		cur = out
	}
	s.writeFileLocked(wsKey, cur)
	s.cache[wsKey] = cur
	return cur, true
}

// normalizeLocked 去重 + 限长 + 保持顺序（持锁调用）。
func (s *sessionPinsStore) normalizeLocked(pins []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(pins))
	for _, id := range pins {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxSessionPins {
			break
		}
	}
	return out
}

// writeFileLocked 写盘（持锁调用；目录自动创建，失败静默——pin 是尽力而为的展示增强）。
func (s *sessionPinsStore) writeFileLocked(wsKey string, pins []string) {
	dir := filepath.Dir(sessionPinsFile(wsKey))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(sessionPinsFileData{Pinned: pins})
	if err != nil {
		return
	}
	_ = os.WriteFile(sessionPinsFile(wsKey), data, 0o600)
}

// isPinned 判断某 session 是否已 pin（供 listSessions 标注）。
func (s *sessionPinsStore) isPinned(wsKey, sessionID string) bool {
	for _, id := range s.load(wsKey) {
		if id == sessionID {
			return true
		}
	}
	return false
}
