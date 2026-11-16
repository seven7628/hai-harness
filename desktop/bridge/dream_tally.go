package main

// Dream 巡检台账：一轮巡检里模型**实际碰过**哪些记忆库、各有多少条目、归档了几条。
//
// 为什么要它：运行记录（dream_runs.jsonl）里的 repos 字段此前恒为空——用户只能看到
// 一句模型自己写的摘要，看不到"到底处理了哪几个库"。摘要由模型产出，可以写得很漂亮
// 却漏掉一半的库；台账来自工具**真的被调用过**这件事，属于宿主侧的事实。
//
// 数据来源是 memory_dream 的三类返回（见 memory_tool.go）：
//   - action=repos     → 全部库的 id 与条目数（清单本身也是证据：没列到的库没被处理）
//   - action=evidence  → 这个库的条目数
//   - action=apply     → 这个库归档了几条（从宿主自己生成的返回里读，不是模型自述）
//
// 刻意不解析模型正文：它可以说"归档了 12 条"，而宿主只认工具真的执行了什么。

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

type dreamTally struct {
	mu    sync.Mutex
	order []string
	byID  map[string]*dreamRepoResult
}

func newDreamTally() *dreamTally {
	return &dreamTally{byID: map[string]*dreamRepoResult{}}
}

// get 取（或建）某库的记录。调用方必须已持锁。
func (t *dreamTally) get(id string) *dreamRepoResult {
	if id == "" {
		id = "(未命名库)"
	}
	rec, ok := t.byID[id]
	if !ok {
		rec = &dreamRepoResult{ID: id}
		t.byID[id] = rec
		t.order = append(t.order, id)
	}
	return rec
}

// observe 吃一条工具结果事件（非 memory_dream 直接忽略）。
func (t *dreamTally) observe(name, arguments, result string) {
	if t == nil || name != "memory_dream" {
		return
	}
	var a struct {
		Action string `json:"action"`
		Repo   string `json:"repo"`
	}
	if json.Unmarshal([]byte(arguments), &a) != nil {
		return
	}
	switch a.Action {
	case "repos":
		var payload struct {
			Repos []struct {
				ID     string `json:"id"`
				Topics int    `json:"topics"`
			} `json:"repos"`
		}
		if json.Unmarshal([]byte(result), &payload) != nil {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		for _, r := range payload.Repos {
			t.get(r.ID).Topics = r.Topics
		}
	case "evidence":
		var payload struct {
			Repo   string `json:"repo"`
			Topics int    `json:"topics"`
		}
		if json.Unmarshal([]byte(result), &payload) != nil {
			return
		}
		id := payload.Repo
		if id == "" {
			id = a.Repo
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		t.get(id).Topics = payload.Topics
	case "apply":
		n := dreamArchivedFromResult(result)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.get(a.Repo).Archived += n
	}
}

// dreamArchivedFromResult 从宿主的归档返回里读条数。
//
// 返回文本形如「记忆归档：归档 2 条：a.md、b.md；快照 …」（见 memory/dream_tool.go
// 的 ApplyResult.Summary）。读自己产出的格式而不是模型的复述：模型会美化数字。
func dreamArchivedFromResult(s string) int {
	const marker = "归档 "
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, " 条")
	if j <= 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:j]))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// results 按首次出现的顺序导出（喂给运行记录的 repos 字段）。
func (t *dreamTally) results() []dreamRepoResult {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]dreamRepoResult, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, *t.byID[id])
	}
	return out
}
