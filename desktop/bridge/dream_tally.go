package main

// Dream 巡检台账：一轮巡检里模型**实际碰过**哪些记忆库、各有多少条目、归档了几条，
// 以及按顺序的**工具调用时间线**（详情视图的「执行时间线」）。
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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/events"
)

type dreamTally struct {
	mu    sync.Mutex
	order []string
	byID  map[string]*dreamRepoResult
	// steps 按发生顺序的工具调用（nil 台账安全：非 Dream 会话走不到这里）。
	steps []dreamStep
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

// observe 吃一条工具结果事件。
//
// 与 repos 台账同一个观察点（main.go 的 ToolRunEnd 分支）：时间线不该另开一路
// 事件订阅——两路观察会在"收口时读谁"上产生时序分歧，而收口必须看到**最后一条**
// 工具结果。
//
// 所有工具都记步骤（不只是 memory_dream）：详情视图要回答的是"这一轮都干了什么"，
// 模型读文件、跑 bash 也在其列。
func (t *dreamTally) observe(ev *events.ToolResponse) {
	if t == nil || ev == nil || strings.TrimSpace(ev.Name) == "" {
		return
	}
	// 一次加锁覆盖"记步骤 + 记台账"：两者必须来自同一条事件，分两次加锁会让
	// 收口快照读到"步骤里有 apply、台账里还没加"的半截状态。
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, dreamStepOf(ev))
	if ev.Name != "memory_dream" {
		return
	}
	var a struct {
		Action string `json:"action"`
		Repo   string `json:"repo"`
	}
	if json.Unmarshal([]byte(ev.Arguments), &a) != nil {
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
		if json.Unmarshal([]byte(ev.Result), &payload) != nil {
			return
		}
		for _, r := range payload.Repos {
			t.get(r.ID).Topics = r.Topics
		}
	case "evidence":
		var payload struct {
			Repo   string `json:"repo"`
			Topics int    `json:"topics"`
		}
		if json.Unmarshal([]byte(ev.Result), &payload) != nil {
			return
		}
		id := payload.Repo
		if id == "" {
			id = a.Repo
		}
		t.get(id).Topics = payload.Topics
	case "apply":
		n := dreamArchivedFromResult(ev.Result)
		t.get(a.Repo).Archived += n
	}
}

// dreamStep 的一条事件翻成时间线条目（参数、成败、一句话备注）。
func dreamStepOf(ev *events.ToolResponse) dreamStep {
	st := dreamStep{
		Tool: strings.TrimSpace(ev.Name),
		Arg:  dreamStepArg(ev),
		OK:   dreamStepOK(ev),
		Note: dreamStepNote(ev),
	}
	// 事件没带时间戳时用收口时刻兜底：时间线可以粗，但不能整列空白。
	at := ev.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	st.T = at.Format(time.RFC3339)
	return st
}

// dreamStepArg 取这次调用**最有辨识度**的一个参数（不是整串 JSON：一行放不下，
// 而且 content 字段能把一条记录撑到几百 KB）。
//
// 其余工具留空：时间线要的是"何时调了什么、成没成"，参数细节看 tool 名就够，
// 塞满参数只会让列表没法扫。想扩就在这里加 case。
func dreamStepArg(ev *events.ToolResponse) string {
	var a map[string]any
	if json.Unmarshal([]byte(ev.Arguments), &a) != nil {
		return ""
	}
	argOf := func(k string) string {
		if s, ok := a[k].(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	switch ev.Name {
	case "memory_dream":
		action := argOf("action")
		// apply 是唯一会**改**记忆的动作：只写 "apply" 时，8 个库的 8 次归档
		// 在时间线上长得一模一样，看不出是哪个库。
		if action == "apply" {
			if repo := argOf("repo"); repo != "" {
				return action + " " + repo
			}
		}
		return action
	case "bash":
		return dreamClip(argOf("command"), 60)
	case "write_file", "edit_file":
		return argOf("path")
	}
	return ""
}

// dreamClip 按 rune 截断（不切半个汉字）并压成单行——行内展示用。
func dreamClip(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// dreamStepOK 判断一次工具调用是否成功。
//
// 主依据是事件自带的 IsError（宿主工具的失败都是 ToolResult{IsError:true}，
// 见 tools/engine.go）。文本判据只是**兜底**：有些失败以文本形式返回（MCP 工具、
// 外部命令），IsError 为假。这里刻意的偏向是"宁可 false 不可误判成功"——
// 用户看时间线是为了排查"这一轮为什么没干完"，把失败标成成功比标反更误导。
//
// 但也不能全文扫 "error"：成功结果里常有这个词（grep 命中、读到的日志、看的代码）。
// 所以只看结果**开头**一小段，且要求错误词的形态（"error:" / "failed" / "permission
// denied" 这类），而不是单独一个 error。
func dreamStepOK(ev *events.ToolResponse) bool {
	if ev.IsError {
		return false
	}
	head := strings.TrimSpace(ev.Result)
	if head == "" {
		return true
	}
	if r := []rune(head); len(r) > dreamStepErrorScan {
		head = string(r[:dreamStepErrorScan])
	}
	low := strings.ToLower(head)
	for _, m := range []string{
		"error:", "failed", "failure", "permission denied", "no such file",
		"not found", "timed out", "timeout", "panic:", "traceback",
	} {
		if strings.Contains(low, m) {
			return false
		}
	}
	return true
}

// dreamStepErrorScan 兜底判据只看结果开头这么多字符（rune）：
// 工具的错误文案都在开头，越往后越可能是正常内容里恰好出现的错误词。
const dreamStepErrorScan = 200

// dreamStepNote 一句话备注（读宿主自己产出的返回，不复述模型的话）。
func dreamStepNote(ev *events.ToolResponse) string {
	if ev.Name != "memory_dream" {
		return ""
	}
	var a struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(ev.Arguments), &a) != nil {
		return ""
	}
	switch a.Action {
	case "repos":
		var payload struct {
			Count int `json:"count"`
			Repos []struct {
				ID string `json:"id"`
			} `json:"repos"`
		}
		if json.Unmarshal([]byte(ev.Result), &payload) != nil {
			return ""
		}
		n := payload.Count
		if n == 0 {
			n = len(payload.Repos)
		}
		return fmt.Sprintf("%d 个库", n)
	case "evidence":
		var payload struct {
			Topics int `json:"topics"`
		}
		if json.Unmarshal([]byte(ev.Result), &payload) != nil {
			return ""
		}
		return fmt.Sprintf("%d 条", payload.Topics)
	case "apply":
		return fmt.Sprintf("归档 %d 条", dreamArchivedFromResult(ev.Result))
	}
	return ""
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
	return t.resultsLocked()
}

func (t *dreamTally) resultsLocked() []dreamRepoResult {
	out := make([]dreamRepoResult, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, *t.byID[id])
	}
	return out
}

// snapshot 一次性取出（步骤, 台账）——收口时用，不能边写边读。
//
// 观察发生在会话 goroutine（每条 ToolRunEnd），收口在主会话 emit 的尾部：
// 两次 getter 各自加锁会在中间让出一条缝（快照里步骤有 apply、台账还没算上）；
// 一次加锁 + **拷贝** 两份数据，收口拿到的是同一个瞬间。
func (t *dreamTally) snapshot() ([]dreamStep, []dreamRepoResult) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	steps := make([]dreamStep, len(t.steps))
	copy(steps, t.steps)
	return steps, t.resultsLocked()
}
