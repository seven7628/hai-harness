package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Dream：记忆生命周期的最后一环（2026-11 产品化）。
//
// 解决的问题：记忆有写入、有调整，但时间一长没人知道整体还符不符合规范——
// 哪些已经过时、哪些重复、哪些根本没进索引。评估曾经回答过"能力如何"，
// 但那是过去时；产品要的是"每天都有人巡检一遍"。
//
// 形态：**用内置 Agent 运行时**跑，不是脚本。到点由 bridge 起一个派生会话
// （与 cron 同一条 createOpts + Ask 链路），模型拿 memory_dream 工具遍历
// 全部记忆库：取证据 → 裁决 → 归档（宿主过护栏），并可直接改文件、同步索引。
//
// 作用域：应用内**所有**记忆库（~/.go-code/memory/repos/*），不是当前项目。

// dreamSettings 是设置页「Dream」那一段的内容。
type dreamSettings struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// At 每天的执行时刻，本地时间 "HH:MM"。
	At string `json:"at"`
	// LastRunDate 最近一次触发的**本地日期**（YYYY-MM-DD）。每天最多一次靠它判。
	LastRunDate string `json:"last_run_date"`
}

// dreamDirOverride 测试注入：非空时 Dream 的配置与运行记录都落在这个目录（生产恒为空）。
//
// 与 appDataDirOverride 同一套隔离手段：测试里 t.Setenv("HOME") 是进程级副作用，
// 会和同包内并行跑的其它用例抢 HOME；只改 Dream 自己的落点则互不相关。
var dreamDirOverride string

// dreamCfgDir Dream 的落盘目录（生产 = ~/.go-code）。
func dreamCfgDir() string {
	if dreamDirOverride != "" {
		return dreamDirOverride
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".go-code")
}

func dreamSettingsPath() string {
	dir := dreamCfgDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "dream.json")
}

var dreamSettingsMu sync.Mutex

func loadDreamSettings() dreamSettings {
	p := dreamSettingsPath()
	if p == "" {
		return dreamSettings{}
	}
	dreamSettingsMu.Lock()
	defer dreamSettingsMu.Unlock()
	raw, err := os.ReadFile(p)
	if err != nil {
		return dreamSettings{At: "03:00"}
	}
	var s dreamSettings
	if json.Unmarshal(raw, &s) != nil {
		return dreamSettings{At: "03:00"}
	}
	if strings.TrimSpace(s.At) == "" {
		s.At = "03:00"
	}
	return s
}

func saveDreamSettings(s dreamSettings) error {
	p := dreamSettingsPath()
	if p == "" {
		return os.ErrNotExist
	}
	dreamSettingsMu.Lock()
	defer dreamSettingsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// dreamDue 判断此刻该不该触发：开关开 + 今天还没跑 + 已到点。
func dreamDue(s dreamSettings, now time.Time) bool {
	if !s.Enabled {
		return false
	}
	today := now.Format("2006-01-02")
	if s.LastRunDate == today {
		return false
	}
	h, m, ok := parseClock(s.At)
	if !ok {
		h, m = 3, 0
	}
	return now.Hour() > h || (now.Hour() == h && now.Minute() >= m)
}

func parseClock(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, m := atoiSafe(parts[0]), atoiSafe(parts[1])
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// dreamRepoResult 一次运行里某个记忆库的结果。
type dreamRepoResult struct {
	ID       string `json:"id"`
	Topics   int    `json:"topics"`
	Archived int    `json:"archived"`
	Note     string `json:"note,omitempty"`
}

// dreamRun 一次 Dream 运行记录（设置页的"运行记录"读它）。
type dreamRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	SessionID  string    `json:"session_id,omitempty"`
	// Summary 本轮巡检的收尾结论（模型最后一段正文的首行）。人工要看的是
	// "处理了几个库、归档了几条"，而不是整段对话——正文留在派生会话的事件日志里。
	Summary string            `json:"summary,omitempty"`
	Repos   []dreamRepoResult `json:"repos,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func dreamRunsPath() string {
	dir := dreamCfgDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "dream_runs.jsonl")
}

// dreamRunsMu 串行化运行记录文件的读改写。
//
// 追加（触发时）与收口（AgentEnd 时把同一行从"已开始"补成"已完成"）来自两个
// goroutine，而收口是**重写整个文件**（原子 rename）：不加锁会丢掉同时在追加的行。
var dreamRunsMu sync.Mutex

// dreamRunsKeep 运行记录保留条数上限（200 条约半年）。
//
// 文件只追加不裁剪，一年后就是几 MB，而 dream_get 每次都把它全读一遍。200 条足够
// 看出"最近还正常吗"（设置页本来只展示最近 20 条）。裁剪放在**写入成功之后**做：
// 写失败时历史一个字都不能动。
const dreamRunsKeep = 200

// appendDreamRun 追加一条运行记录（best-effort：失败不打断运行）。
func appendDreamRun(rec dreamRun) {
	p := dreamRunsPath()
	if p == "" {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	dreamRunsMu.Lock()
	defer dreamRunsMu.Unlock()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, werr := f.Write(append(raw, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// 这一行没落住：连裁剪也不做。历史是下一轮写入成功时才裁的。
		fmt.Fprintf(os.Stderr, "dream: 追加运行记录失败，跳过裁剪: %v %v\n", werr, cerr)
		return
	}
	trimDreamRunsLocked(p, dreamRunsKeep)
}

// trimDreamRunsLocked 只保留最近 keep 行（调用方必须持有 dreamRunsMu）。
//
// 临时文件 + rename 原子替换，权限仍是 0600；任何一步失败都只打一行 stderr 就放弃
// ——"这一轮记录没写进去"比"把上一年的历史截断成半截"严重得多。保留的是**文件尾**
// （追加顺序即时间序），所以裁剪后仍是时间序、最近的那条一定还在。
func trimDreamRunsLocked(p string, keep int) {
	raw, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: 读运行记录失败，跳过裁剪: %v\n", err)
		return
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) <= keep {
		return
	}
	buf := make([]byte, 0, len(raw)/2)
	for _, ln := range lines[len(lines)-keep:] {
		buf = append(buf, ln...)
		buf = append(buf, '\n')
	}
	tmp := p + ".trim"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "dream: 写运行记录临时文件失败，跳过裁剪: %v\n", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "dream: 原子替换运行记录失败，跳过裁剪: %v\n", err)
	}
}

// completeDreamRun 把某次运行的记录从"已开始"补成"已完成"。
//
// 为什么改写而不是再追加一条：设置页一行 = 一次运行。追加第二条（状态行）会把
// 同一个 session 拆成两行，用户看到的是"跑了两遍"。文件很小（一次运行一行），
// 整份重写的代价可以忽略。
func completeDreamRun(sessionID string, finished time.Time, summary, errMsg string, repos []dreamRepoResult) {
	if sessionID == "" {
		return
	}
	dreamRunsMu.Lock()
	defer dreamRunsMu.Unlock()
	p := dreamRunsPath()
	if p == "" {
		return
	}
	records := readDreamRuns(0) // 已持锁：readDreamRuns 本身不加锁（见 dreamRunsMu 说明）
	hit := false
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].SessionID == sessionID && records[i].FinishedAt.IsZero() {
			records[i].FinishedAt = finished
			records[i].Summary = summary
			records[i].Error = errMsg
			// repos 来自宿主台账（模型真的调过哪些库），不是模型的自述——
			// 摘要可能漏库，台账不会（见 dream_tally.go）。
			if len(repos) > 0 {
				records[i].Repos = repos
			}
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	var buf []byte
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		buf = append(buf, raw...)
		buf = append(buf, '\n')
	}
	tmp := p + ".tmp"
	// 0600：运行记录里有会话正文摘要，按配置文件对待。
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// readDreamRuns 读最近 limit 条（limit<=0 全部）；损坏行跳过。
func readDreamRuns(limit int) []dreamRun {
	p := dreamRunsPath()
	if p == "" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []dreamRun
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r dreamRun
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// dreamPrompt 交给派生会话的任务书。
//
// 写清楚三件事：范围（全部记忆库）、工具（memory_dream）、允许的动作边界
// （归档由宿主执行；改文件可以，但索引必须同步）。
func dreamPrompt() string {
	return `你的任务是巡检本地全部记忆库，让它们保持可用。

范围：应用内的**每一个**记忆库（一个项目一份），不是当前工作目录那一个。
第一步先调 memory_dream(action=repos) 拿清单：返回里每个 id 就是一个库名，count 是库数。
不要把 ls / bash / glob 当第一步去猜目录——工作目录是个空目录，那几步只会浪费轮次。

对清单里的每一个库：
1. 用库名调 memory_dream(action=evidence, repo=<库 id>) 取证据：代码锚点是否还存在、最后一次被引用是什么时候、
   有没有被后续会话推翻、有没有重复或未进索引的条目。
2. 依据证据判断每一条：仍然成立 → 留着；明显过时/被继任/一次性快照 → 提交 archive；
   证据不足 → 留着。把判断用 memory_dream(action=apply, repo=<库 id>) 提交，宿主会再做护栏检查
   （保护清单、7 天冷却、单次上限、快照）。没有要归档的就不要调 apply 空转。
3. 归档之外可以顺手修正：把与正文不符的索引摘要改对、把没登记的条目补进 MEMORY.md、
   合并明显重复的条目。改文件用标准文件工具（绝对路径 ~/.go-code/memory/repos/<库 id>/…），
   改完必须让 MEMORY.md 与文件一致（索引行必须带文件名，否则不算条目）。

不要做的事：不要删除记忆文件（归档是宿主的动作，可恢复）；不要动 .state/ 下的文件；
不要为了让结果好看而归档仍然有用的条目；不要用 bash 改记忆文件（用 read_file / edit_file / write_file）。

做完后用一句话汇总：处理了几个库、归档了几条、修正了几处。`
}
