// session_manual_titles.go —— 用户手动重命名的会话标题（跨端共享）。
//
// 为什么要有它：桌面端重命名只写 localStorage（titleOverrides），手机看不到；
// 手机重命名又不能让桌面丢失。于是把「手动标题」上移到 bridge：
//
//	~/.go-code/sessions/<wsKey>/titles_manual.json   { "titles": { sid: 标题 } }
//
// 优先级：手动标题 > AI 标题 > 首条 user 文本截断 > "New Chat"。
// 与 titles.json（AI 生成）分开存：AI 标题是「没有则写」的幂等增强，
// 手动标题是「随时覆盖/清除」，两者生命周期不同，混在一张表里会互相踩。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func manualTitlesFile(wsKey string) string {
	return filepath.Join(appDataDir(), "sessions", wsKey, "titles_manual.json")
}

type manualTitlesStore struct {
	mu    sync.Mutex
	cache map[string]map[string]string
}

var sessionManualTitles = &manualTitlesStore{cache: map[string]map[string]string{}}

type manualTitlesFileData struct {
	Titles map[string]string `json:"titles"`
}

func (s *manualTitlesStore) get(wsKey, sid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.cache[wsKey]; ok {
		return m[sid]
	}
	m := s.readLocked(wsKey)
	s.cache[wsKey] = m
	return m[sid]
}

// set 写/清手动标题：空字符串 = 清除覆盖（回落到 AI/首条输入）。
// 返回是否真的改了（没改就不落盘，避免每次点开设置都写一次文件）。
func (s *manualTitlesStore) set(wsKey, sid, title string) bool {
	title = strings.TrimSpace(title)
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.cache[wsKey]
	if m == nil {
		m = s.readLocked(wsKey)
		s.cache[wsKey] = m
	}
	if title == "" {
		if _, ok := m[sid]; !ok {
			return false
		}
		delete(m, sid)
		s.writeLocked(wsKey, m)
		return true
	}
	if m[sid] == title {
		return false
	}
	m[sid] = title
	s.writeLocked(wsKey, m)
	return true
}

func (s *manualTitlesStore) readLocked(wsKey string) map[string]string {
	data, err := os.ReadFile(manualTitlesFile(wsKey))
	if err != nil {
		return map[string]string{}
	}
	var f manualTitlesFileData
	if json.Unmarshal(data, &f) != nil || f.Titles == nil {
		return map[string]string{}
	}
	return f.Titles
}

func (s *manualTitlesStore) writeLocked(wsKey string, titles map[string]string) {
	dir := filepath.Dir(manualTitlesFile(wsKey))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(manualTitlesFileData{Titles: titles})
	if err != nil {
		return
	}
	_ = os.WriteFile(manualTitlesFile(wsKey), data, 0o600)
}
