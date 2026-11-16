package main

// Dream 跨轮状态（~/.go-code/dream_state.json）：把上一轮的结论带到下一轮。
//
// 为什么需要它。任务书（dreamPrompt）是**固定文本**，于是每晚都是一次冷启动：
// 昨天已经判过"这条是 10-17 的进行中快照，S2/S3/S4 已闭合"，今天同一个理由会被
// 重新推一遍；更糟的是模型不知道自己昨天说过什么，于是同一条记忆在 archive 与
// keep 之间来回摆，而两边的理由都写得头头是道——积累变成了漂移。
//
// 运行记录（dream_runs.jsonl）里其实有这些结论，但它**不是**巡检输入：
//   - 只留最近 200 行（dreamRunsKeep），一个忙碌的季度就能把它轮转掉；
//   - 按"轮"组织，要看"这个库上一条归档是什么时候、为什么"得跨行扫全文；
//   - 正文是模型自述（还有摘要化），而下一轮需要的是**宿主确认过的事实**。
// 所以另立一份状态：小、固定形态、每库只放最近 20 条归档事实，覆盖写、不带历史。
//
// 两份文件的信任级不同，这条界线必须写在代码里：
//   - 状态里的 Archived 只来自宿主事实——memory_dream 的 apply **参数**（file→reason）
//     与**返回**（真正落盘了哪些文件）的交集（见 dream_tally.go 的 addFacts）；
//   - LastNotes 是模型收尾正文，仍然原样留着（人要看"上一轮它自己怎么说"），
//     但它不参与"事实"判定，也不进任务书——那段只放宿主确认过的东西。
//
// 并发：写路径（收口）用 dreamStateMu 串行；读路径（fireDream 拼任务书）不加锁、
// 也不共享内存，直接读文件再解析——写是"临时文件 + rename"，读到的要么是旧的一整份、
// 要么是新的一整份，不存在半份。一天两次的小文件，不值得为它维护进程内缓存。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// dreamStateFile 状态文件名（落在 dreamCfgDir，生产 = ~/.go-code）。
const dreamStateFile = "dream_state.json"

// dreamStateBadSuffix 坏文件的留档后缀（.bad）。
//
// 读到非法 JSON 时先改名再重建：直接覆盖等于把用户上一轮的结论永久吞掉，
// 而"巡检照常跑"不该以丢数据为代价。留档文件不会再被读，留给人工排查。
const dreamStateBadSuffix = ".bad"

// dreamStateVersion 状态格式版本。
//
// 1 = 初版（本文件）。缺 version 的老文件/手写文件按 1 处理；将来的不兼容变更
// 在这里 bump，读路径按版本决定怎么解释。
const dreamStateVersion = 1

// 状态体积上限。状态是**累积**的，任务书必须是**有界**的——这四个常量就是那道闸：
//   - 每库最多带 6 条归档事实（再多也轮不到模型看：它这轮只处理这个库的十几条）；
//   - 最多带 12 个库（实际巡检通常 7 个左右，12 是余量）；
//   - 每条 reason 截 120 rune（模型给的依据本来就一句话）；
//   - 整段（含标题）不超过 4096 字节（超了先砍库数、再砍条目，见 dreamPriorSection）。
const (
	dreamStateNotesRunes   = 800 // LastNotes 保留长度
	dreamStateFactsKeep    = 20  // 每库保留的归档事实条数
	dreamPriorFactsPerRepo = 6   // 任务书里每库最多列几条归档事实
	dreamPriorReposMax     = 12  // 任务书里最多列几个库
	dreamPriorReasonRunes  = 120
	dreamPriorSectionBytes = 4096
	dreamPriorStaleDays    = 90
)

// dreamPriorStaleWindow "很久没巡检"的判定窗口。
var dreamPriorStaleWindow = time.Duration(dreamPriorStaleDays) * 24 * time.Hour

// dreamUnnamedRepo apply/evidence 没给 repo 时的库标识（与台账同一口径）。
const dreamUnnamedRepo = "(未命名库)"

// dreamState 跨轮状态的全部内容。
type dreamState struct {
	Version   int                       `json:"version"`
	UpdatedAt time.Time                 `json:"updated_at"`
	LastNotes string                    `json:"last_notes,omitempty"` // 上轮收尾正文前 800 字
	Repos     map[string]dreamRepoState `json:"repos"`
}

// dreamRepoState 一个记忆库的累积状态。
type dreamRepoState struct {
	LastRunAt time.Time           `json:"last_run_at"`
	Topics    int                 `json:"topics,omitempty"`
	Archived  []dreamArchivedFact `json:"archived,omitempty"` // 最近 20 条
	Runs      int                 `json:"runs,omitempty"`     // 这个库被巡检过几轮
}

// dreamArchivedFact 一条**真的落盘了**的归档事实。
//
// File/At 来自宿主（返回里的归档列表 + 工具事件时间），Reason 来自 apply 参数里
// 模型给的依据——下一轮要推翻它时，得先看见它当初为什么这么判。
type dreamArchivedFact struct {
	File   string    `json:"file"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// dreamStateMu 串行化状态的读改写（收口路径）。
//
// 读（fireDream 拼任务书）刻意不走这把锁：那条路只读文件，锁在这里只会让"正在
// 收口"挡住"马上要起跑"的下一轮。
var dreamStateMu sync.Mutex

func dreamStatePath() string {
	dir := dreamCfgDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, dreamStateFile)
}

// emptyDreamState 空状态（Repos 已初始化：写入路径会直接往 map 里塞）。
func emptyDreamState() dreamState {
	return dreamState{Version: dreamStateVersion, Repos: map[string]dreamRepoState{}}
}

// loadDreamState 读状态。读失败 / 文件损坏 / JSON 非法一律当空状态（并打一行 stderr）。
//
// 坏文件先 rename 成 .bad 再返回空状态：调用方（收口）随后会重建它，而旧的字节
// 还在磁盘上——"不能因此挡住巡检"与"不能吞掉旧文件"两条都要满足，只有留档能做到。
func loadDreamState() dreamState {
	p := dreamStatePath()
	if p == "" {
		return emptyDreamState()
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		// 没有文件是**正常**的第一夜，不该每晚在终端里刷一行错误。
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "dream: 读跨轮状态失败，按空状态巡检: %v\n", err)
		}
		return emptyDreamState()
	}
	var st dreamState
	if err := json.Unmarshal(raw, &st); err != nil {
		backup := p + dreamStateBadSuffix
		if rerr := os.Rename(p, backup); rerr != nil {
			fmt.Fprintf(os.Stderr, "dream: 跨轮状态损坏且无法留档（%v），按空状态巡检: %v\n", rerr, err)
		} else {
			fmt.Fprintf(os.Stderr, "dream: 跨轮状态损坏，已留档 %s，按空状态巡检: %v\n", backup, err)
		}
		return emptyDreamState()
	}
	return st.normalize()
}

// normalize 补齐缺字段（老文件 / 手改过的文件），不解释未知版本。
//
// 缺 version / repos 的文件必须能读：nil map 上的**读**是安全的，但写入路径
// （mergeDreamState）会往里塞，所以统一在这里补成空 map，而不是到处判 nil。
func (st dreamState) normalize() dreamState {
	if st.Version <= 0 {
		st.Version = dreamStateVersion
	}
	if st.Version > dreamStateVersion {
		// 未来版本：字段语义可能已经变了，宁可当空状态，也不要拿错字段去拼任务书。
		// 刻意**不**改名留档——那是新版本写的文件，老进程没资格处置它。
		fmt.Fprintf(os.Stderr, "dream: 跨轮状态版本 %d 比本程序新，按空状态巡检\n", st.Version)
		return emptyDreamState()
	}
	if st.Repos == nil {
		st.Repos = map[string]dreamRepoState{}
	}
	return st
}

// saveDreamState 原子写（临时文件 + rename），0600。
//
// 0600 的理由与运行记录一致：状态里有记忆文件名与模型给的归档理由，按配置文件对待。
// rename 保证读路径永远读到一整份（见文件头"并发"）。
func saveDreamState(st dreamState) error {
	p := dreamStatePath()
	if p == "" {
		return os.ErrNotExist
	}
	st.Version = dreamStateVersion
	if st.Repos == nil {
		st.Repos = map[string]dreamRepoState{}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	// 上一轮残留的临时文件可能带着别的权限位（WriteFile 只对新建文件生效）：
	// 先删掉，让 0600 真正落地。
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// updateDreamState 收口时把本轮事实并进状态（best-effort：失败只打一行 stderr）。
//
// 调用点在 finishDreamRun 里、写完运行记录**之后**：运行记录是用户面的账，先落；
// 状态只影响下一轮的任务书，写失败最多让下一轮少一点上下文，不该让这一轮看起来失败。
func updateDreamState(repos []dreamRepoResult, facts map[string][]dreamArchivedFact, final string) {
	dreamStateMu.Lock()
	defer dreamStateMu.Unlock()
	st := mergeDreamState(loadDreamState(), repos, facts, final, time.Now())
	if err := saveDreamState(st); err != nil {
		fmt.Fprintf(os.Stderr, "dream: 写跨轮状态失败（下一轮按空状态巡检）: %v\n", err)
	}
}

// mergeDreamState 把本轮的宿主事实并进上一轮的状态（纯函数，可测）。
//
// 只覆盖**本轮真的巡检过**的库（repos 非空即"这一轮碰过它"）：没碰过的库保持
// 原样——一夜只跑了一半的巡检，不该把另一半库的累积历史洗掉。
func mergeDreamState(prev dreamState, repos []dreamRepoResult, facts map[string][]dreamArchivedFact, final string, now time.Time) dreamState {
	st := prev.normalize()
	st.Version = dreamStateVersion
	st.UpdatedAt = now
	// 空正文不是结论（这一轮出错 / 没来得及说话）：保留上一条，别把上一轮的收尾抹掉。
	if notes := []rune(strings.TrimSpace(final)); len(notes) > 0 {
		if len(notes) > dreamStateNotesRunes {
			notes = notes[:dreamStateNotesRunes]
		}
		st.LastNotes = string(notes)
	}
	for _, r := range repos {
		id := dreamRepoID(r.ID)
		rs := st.Repos[id]
		rs.LastRunAt = now
		rs.Topics = r.Topics
		rs.Runs++
		rs.Archived = mergeDreamFacts(rs.Archived, facts[id], now)
		st.Repos[id] = rs
	}
	return st
}

// mergeDreamFacts 本轮新归档追加到**头部**（最新在前），同 file 只留最新一条，裁到 20。
//
// fresh 按发生顺序给过来（apply 的先后），所以倒着走才是"最新在前"。去重按 file：
// 同一条记忆被归档两次（归档 → 恢复 → 再归档）只该留最新那条理由，否则任务书里
// 会出现两条互相矛盾的依据。
func mergeDreamFacts(prev, fresh []dreamArchivedFact, now time.Time) []dreamArchivedFact {
	out := make([]dreamArchivedFact, 0, len(fresh)+len(prev))
	seen := make(map[string]bool, len(fresh)+len(prev))
	for i := len(fresh) - 1; i >= 0; i-- {
		f := fresh[i]
		f.File = strings.TrimSpace(f.File)
		if f.File == "" || seen[f.File] {
			continue
		}
		if f.At.IsZero() {
			f.At = now
		}
		seen[f.File] = true
		out = append(out, f)
	}
	for _, f := range prev {
		f.File = strings.TrimSpace(f.File)
		if f.File == "" || seen[f.File] {
			continue
		}
		seen[f.File] = true
		out = append(out, f)
	}
	if len(out) > dreamStateFactsKeep {
		out = out[:dreamStateFactsKeep]
	}
	return out
}

// dreamPromptPrior 任务书末尾那一段「上一轮判断」（空状态返回 ""）。
//
// 第一轮整段不出现：没有上一轮时说"上一轮判断"会给模型一个不存在的锚点。
func dreamPromptPrior(st dreamState, now time.Time) string {
	if len(st.Repos) == 0 {
		return ""
	}
	ids := make([]string, 0, len(st.Repos))
	for id := range st.Repos {
		ids = append(ids, id)
	}
	// 最近巡检过的在前：任务书的字节预算要留给"昨天刚判过的库"，半年前的库排后面。
	// 同一时刻按名字定序——任务书必须可复现，否则同一个状态每晚渲染出不同的文本，
	// 出问题就没法比对。
	sort.Slice(ids, func(i, j int) bool {
		a, b := st.Repos[ids[i]].LastRunAt, st.Repos[ids[j]].LastRunAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return ids[i] < ids[j]
	})
	if len(ids) > dreamPriorReposMax {
		ids = ids[:dreamPriorReposMax]
	}
	head := dreamPriorHead(st)
	// 超预算先砍库数、再砍条目（规格）：一个库的上下文比某一条理由更值钱，
	// 而只剩一个库时砍理由总能让整段落进 4096。
	for n := len(ids); n > 0; n-- {
		if s := renderDreamPrior(head, st, ids[:n], dreamPriorFactsPerRepo, now); len(s) <= dreamPriorSectionBytes {
			return s
		}
	}
	for f := dreamPriorFactsPerRepo - 1; f >= 0; f-- {
		if s := renderDreamPrior(head, st, ids[:1], f, now); len(s) <= dreamPriorSectionBytes {
			return s
		}
	}
	// 走不到：只剩标题 + 两行简介，4096 字节装得下任何正常输入。真到了这里
	// （比如某个库名有几千字节），宁可不给也不给半截。
	return ""
}

// dreamPriorHead 标题 + 那句"可以推翻，但要说清为什么"。
//
// 日期取 UpdatedAt（上一轮收尾时刻）；老状态没有它时退到各库最近一次巡检时间，
// 都没有就说"上一轮"——绝不编一个日期出来。
func dreamPriorHead(st dreamState) string {
	day := st.UpdatedAt
	if day.IsZero() {
		for _, rs := range st.Repos {
			if rs.LastRunAt.After(day) {
				day = rs.LastRunAt
			}
		}
	}
	var b strings.Builder
	b.WriteString("## 上一轮判断（不是事实，可以推翻）\n\n")
	if day.IsZero() {
		b.WriteString("这些是上一轮的结论。")
	} else {
		fmt.Fprintf(&b, "这些是 %s 那一轮的结论。", day.Format("2006-01-02"))
	}
	b.WriteString("今天如果证据不同，就推翻它——但要在\n")
	b.WriteString("memory_dream 的 reason 里写明为什么，别默默改主意。\n")
	return b.String()
}

// renderDreamPrior 按给定的库数与每库条目数渲染整段（调用方负责字节预算）。
func renderDreamPrior(head string, st dreamState, ids []string, perRepo int, now time.Time) string {
	var b strings.Builder
	b.WriteString(head)
	for i, id := range ids {
		rs := st.Repos[id]
		if i == 0 {
			b.WriteString("\n") // 与简介之间空一行：一眼分得开"规则"和"上一轮说了什么"
		}
		fmt.Fprintf(&b, "- %s", id)
		// 超 90 天没巡检的库只留一行：它的归档事实早已不能作为"今天"的依据，
		// 列出来只会让模型拿半年前的标准判今天的证据。但也不能装作没这个库——
		// 下一轮该重新取一次证据。
		if !rs.LastRunAt.IsZero() && now.Sub(rs.LastRunAt) > dreamPriorStaleWindow {
			fmt.Fprintf(&b, "（上次 %s，很久没巡检）\n", rs.LastRunAt.Format("2006-01-02"))
			continue
		}
		meta := make([]string, 0, 3)
		if !rs.LastRunAt.IsZero() {
			meta = append(meta, rs.LastRunAt.Format("01-02 15:04"))
		}
		if rs.Runs > 0 {
			meta = append(meta, fmt.Sprintf("已巡检 %d 轮", rs.Runs))
		}
		meta = append(meta, fmt.Sprintf("%d 条", rs.Topics))
		fmt.Fprintf(&b, "（%s）\n", strings.Join(meta, "，"))
		n := len(rs.Archived)
		if n > perRepo {
			n = perRepo
		}
		for _, f := range rs.Archived[:n] {
			b.WriteString("  已归档：")
			b.WriteString(f.File)
			if reason := dreamOneLine(f.Reason, dreamPriorReasonRunes); reason != "" {
				b.WriteString(" —— ")
				b.WriteString(reason)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// dreamOneLine 压成单行并按 rune 截断（**含**省略号也不超过 max 个字）。
//
// 与 dreamClip 的差别只在计数口径：这里的 max 是硬上限（prompt 有字节预算，
// 多一个省略号就是多 3 字节），而 dreamClip 是"最多 max 个正文 + 省略号"。
func dreamOneLine(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return string(r[:1])
	}
	return string(r[:max-1]) + "…"
}
