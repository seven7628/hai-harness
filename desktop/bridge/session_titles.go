package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AI 生成的会话标题持久化：~/.go-code/sessions/<wsKey>/titles.json
//
// 结构：
//
//	{ "titles": { "sid": "ai 生成的标题", ... } }
//
// 用途：会话首次用户输入后异步调 LLM 生成简短标题（≤15 字符、跟随用户输入语言），
// 跨重启持久；listSessions 标题优先级 = AI 标题 > 首条 user 文本截断 > "New Chat"。
// 与 session_pins 同模式（独立文件 + 内存缓存），不影响会话数据本身。
//
// 用户手动重命名（前端 titleOverrides，localStorage）优先级高于本存储——
// 前端渲染取 titleOverrides ?? 服务端 title，AI 标题只是服务端默认标题的增强。

const (
	// defaultSessionTitle 无任何用户输入的新会话默认标题。
	defaultSessionTitle = "New Chat"
	// maxGeneratedTitleRunes AI 生成标题的上限（需求：不超过 15 个字符）。
	maxGeneratedTitleRunes = 15
)

// titlesFile 返回某 workspace 的 AI 标题文件路径。
func titlesFile(wsKey string) string {
	return filepath.Join(appDataDir(), "sessions", wsKey, "titles.json")
}

type sessionTitlesStore struct {
	mu sync.Mutex
	// key: wsKey → sid → ai 标题
	cache map[string]map[string]string
}

var sessionTitles = &sessionTitlesStore{cache: map[string]map[string]string{}}

type sessionTitlesFileData struct {
	Titles map[string]string `json:"titles"`
}

// get 取某 session 的 AI 标题（空 = 未生成）。
func (s *sessionTitlesStore) get(wsKey, sid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.cache[wsKey]; ok {
		return m[sid]
	}
	m := s.readFileLocked(wsKey)
	s.cache[wsKey] = m
	return m[sid]
}

// set 写某 session 的 AI 标题；返回是否实际写入（已有值 / 空标题 → false 不覆盖）。
func (s *sessionTitlesStore) set(wsKey, sid, title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.cache[wsKey]
	if m == nil {
		m = s.readFileLocked(wsKey)
		s.cache[wsKey] = m
	}
	if _, exists := m[sid]; exists {
		return false // 已有标题不覆盖（幂等；用户改标题走前端 titleOverrides）
	}
	m[sid] = title
	s.writeFileLocked(wsKey, m)
	return true
}

// readFileLocked 从磁盘读（持锁调用）。
func (s *sessionTitlesStore) readFileLocked(wsKey string) map[string]string {
	data, err := os.ReadFile(titlesFile(wsKey))
	if err != nil {
		return map[string]string{}
	}
	var f sessionTitlesFileData
	if err := json.Unmarshal(data, &f); err != nil || f.Titles == nil {
		return map[string]string{}
	}
	return f.Titles
}

// writeFileLocked 写盘（持锁调用；目录自动创建，失败静默——标题是尽力而为的展示增强）。
func (s *sessionTitlesStore) writeFileLocked(wsKey string, titles map[string]string) {
	dir := filepath.Dir(titlesFile(wsKey))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(sessionTitlesFileData{Titles: titles})
	if err != nil {
		return
	}
	_ = os.WriteFile(titlesFile(wsKey), data, 0o600)
}
