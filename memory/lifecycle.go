package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 记忆的生命周期由**模型决策**，不设人工确认（2026-11 定）。
//
// # 为什么可以去掉人工
//
// 整理记忆的判断本质上是"这条还成立吗"，而人没有比模型更好的信息：模型手里有
// 锚点核验、引用轨迹、usage 三条证据，还能读正文本身。人只能看报告，看到的比它少。
// 让人当闸门，结果是没人愿意天天点，最后报告烂在一边——那比不做更糟。
//
// # 那安全靠什么
//
// 不靠"人看一遍"，靠**动作本身可逆**：
//
//	① 只归档，不删除。文件移到 `~/.go-code/memory/archive/<repo>/`，随时能移回。
//	② 动手前快照。`.state/dream/<ts>/` 存一份原文件 + 原索引行。
//	③ 每次留日志。`.dream.log` 记下"改了什么、依据哪条证据、模型怎么说的"。
//	④ 硬上限。单次最多归档 20%、最近 7 天内动过的一律不动、索引永远不动。
//
// ①②让错误可撤销，③让错误可追溯，④让错误不会一次不可收拾。这四条不参与"判断"，
// 只限制"判断"能造成多大破坏——判断留给模型。

// Decision 是模型对一条记忆给出的裁决。
type Decision struct {
	File   string `json:"file"`
	Action string `json:"action"` // keep / archive
	Reason string `json:"reason"`
}

// 允许的动作。刻意只有两个：合并与删除都需要更强的证据与更复杂的回滚，
// 等这两条路被真实跑过再说。
const (
	ActionKeep    = "keep"
	ActionArchive = "archive"
)

// 安全护栏（不参与判断，只限制破坏面）。
const (
	dreamMaxArchiveRatio = 0.2                // 单次最多归档这个比例
	dreamMinFileAge      = 7 * 24 * time.Hour // 最近动过的文件不碰
	dreamArchiveDirName  = "archive"
)

// ApplyResult 是一次应用的结果。
type ApplyResult struct {
	Archived []string
	Skipped  []string // 被护栏挡下的（附原因）
	Snapshot string
	LogPath  string
}

// evidenceRoot 返回归档根：<记忆根>/archive/<repo-id>/。
//
// 按 v3 §18.5 移出记忆根：留在 repos/ 下的话 `grep -r` 照样命中，等于没降级。
func (c Config) archiveDir(d Dir) string {
	return filepath.Join(filepath.Dir(filepath.Dir(d.Path)), dreamArchiveDirName, d.ID)
}

// ApplyDecisions 执行模型给出的裁决。
//
// 只做两件事：归档（移动文件 + 从索引摘掉那一行）与忽略（keep）。
// 任何一步失败都**不中断**整体：一条失败就跳过它并记下来，剩下继续——
// 半途而废留下的状态比"少归档一条"更难收拾。
func (c Config) ApplyDecisions(d Dir, decisions []Decision) (ApplyResult, error) {
	c = c.withDefaults()
	var out ApplyResult

	topics, err := c.topicFiles(d)
	if err != nil {
		return out, err
	}
	allowed := map[string]bool{}
	for _, t := range topics {
		allowed[t] = true
	}

	// 先过滤，再落盘：护栏必须在动任何文件之前生效。
	var todo []Decision
	for _, dec := range decisions {
		if dec.Action != ActionArchive {
			continue
		}
		if !allowed[dec.File] {
			out.Skipped = append(out.Skipped, dec.File+"（不是这个记忆库里的条目）")
			continue
		}
		p := filepath.Join(d.Path, dec.File)
		// "最近动过"用绝对时间比较：mtime 可能落在未来（时钟漂移/同步），
		// 那种情况同样不该动它。
		if info, err := os.Stat(p); err == nil && info.ModTime().After(c.Now().Add(-dreamMinFileAge)) {
			out.Skipped = append(out.Skipped, dec.File+"（最近 7 天内动过）")
			continue
		}
		todo = append(todo, dec)
	}
	if len(topics) > 0 {
		// 上限至少为 1：小记忆库（2~4 条）按 20% 取整会得到 0，
		// 那等于永远不许归档——护栏是为了限制破坏面，不是为了禁用功能。
		capN := int(float64(len(topics)) * dreamMaxArchiveRatio)
		if capN < 1 {
			capN = 1
		}
		if len(todo) > capN {
			for _, dec := range todo[capN:] {
				out.Skipped = append(out.Skipped, dec.File+"（超出单次归档上限）")
			}
			todo = todo[:capN]
		}
	}
	if len(todo) == 0 {
		return out, nil
	}

	// 快照：动手之前把原文件与索引整份复制出来。
	stamp := c.Now().UTC().Format("20060102-150405")
	snap := filepath.Join(d.Path, stateDirName, "dream", stamp)
	if err := c.MkdirAll(snap, 0o755); err != nil {
		return out, err
	}
	out.Snapshot = snap
	for _, dec := range todo {
		raw, err := c.ReadFile(filepath.Join(d.Path, dec.File))
		if err != nil {
			continue
		}
		if err := c.WriteFile(filepath.Join(snap, dec.File), raw, 0o600); err != nil {
			return out, err
		}
	}
	if idx, err := c.ReadFile(joinIndex(d)); err == nil {
		_ = c.WriteFile(filepath.Join(snap, IndexFileName), idx, 0o600)
	}

	// 执行：移动到归档区（跨目录，先确保目标存在）。
	arch := c.archiveDir(d)
	if err := c.MkdirAll(arch, 0o755); err != nil {
		return out, err
	}
	logPath := filepath.Join(d.Path, stateDirName, "dream.log")
	now := c.Now().UTC().Format(time.RFC3339)
	for _, dec := range todo {
		src := filepath.Join(d.Path, dec.File)
		dst := filepath.Join(arch, dec.File)
		if err := c.Rename(src, dst); err != nil {
			out.Skipped = append(out.Skipped, dec.File+"（移动失败："+err.Error()+"）")
			continue
		}
		out.Archived = append(out.Archived, dec.File)
		line := fmt.Sprintf("%s archive %s — %s\n", now, dec.File, strings.ReplaceAll(dec.Reason, "\n", " "))
		if f, err := c.OpenAppend(logPath); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}

	if len(out.Archived) > 0 {
		if err := c.dropIndexLines(d, out.Archived); err != nil {
			out.Skipped = append(out.Skipped, "索引更新失败："+err.Error())
		}
	}
	out.LogPath = logPath
	return out, nil
}

// topicFiles 列出这个记忆库里的条目名（不含索引）。
func (c Config) topicFiles(d Dir) ([]string, error) {
	entries, err := c.ReadDir(d.Path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") ||
			strings.EqualFold(e.Name(), IndexFileName) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// dropIndexLines 从索引里摘掉已归档条目的行。
//
// 只摘"这一行的定位符指向该文件"的行；摘完不动任何其它行——索引是模型的写作成果，
// 宿主只删自己造成的悬空链接。
func (c Config) dropIndexLines(d Dir, files []string) error {
	raw, err := c.ReadFile(joinIndex(d))
	if err != nil {
		return nil // 没有索引就没有可摘的
	}
	gone := map[string]bool{}
	for _, f := range files {
		gone[f] = true
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			if target, _, ok := splitLocator(t); ok && gone[filepath.Base(target)] {
				continue
			}
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	if out == string(raw) {
		return nil
	}
	return c.atomicWrite(joinIndex(d), []byte(out))
}

// ParseDecisions 解析模型输出的裁决 JSON。
//
// 宽容一点：模型可能包一层 `{"decisions": [...]}`，也可能直接给数组。
// 只认 keep/archive，其它动作按 keep 处理（宁可不做）。
func ParseDecisions(raw []byte) ([]Decision, error) {
	var wrapped struct {
		Decisions []Decision `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Decisions) > 0 {
		return normalizeDecisions(wrapped.Decisions), nil
	}
	var arr []Decision
	if err := json.Unmarshal(raw, &arr); err == nil {
		return normalizeDecisions(arr), nil
	}
	// 兜底：从文本里抠出第一个 JSON 数组或对象。
	if i, j := strings.Index(string(raw), "["), strings.LastIndex(string(raw), "]"); i >= 0 && j > i {
		if err := json.Unmarshal(raw[i:j+1], &arr); err == nil {
			return normalizeDecisions(arr), nil
		}
	}
	return nil, fmt.Errorf("无法解析裁决 JSON")
}

func normalizeDecisions(in []Decision) []Decision {
	for i := range in {
		in[i].File = filepath.Base(strings.TrimSpace(strings.TrimSuffix(in[i].File, ".md"))) + ".md"
		switch in[i].Action {
		case ActionArchive:
		case ActionKeep:
		default:
			in[i].Action = ActionKeep
		}
	}
	return in
}

// Evidence 是交给模型裁决的一条记忆的全部证据。
type Evidence struct {
	Repo  string        `json:"repo"`
	Path  string        `json:"path"`
	Count int           `json:"count"`
	Items []EvidenceRow `json:"items"`
}

// EvidenceRow 是单个条目的证据行。
//
// 只放**可核事实**，不放任何"建议"：模型的判断应该建立在原始读数上，
// 而不是建立在宿主替它做的结论上（那等于把决策权又收回去了）。
type EvidenceRow struct {
	File         string `json:"file"`
	Type         string `json:"type,omitempty"`
	Modified     string `json:"modified,omitempty"`
	Indexed      bool   `json:"indexed"`
	UsageReads   int    `json:"usage_reads"`
	ReadsInLog   int    `json:"reads_in_transcript"`
	LastReadDays int    `json:"last_read_days"` // -1 = 转录里从未出现
	AnchorRefs   int    `json:"anchor_refs"`
	AnchorDead   int    `json:"anchor_dead"`
	Summary      string `json:"summary"` // 索引行摘要（模型判断"这条讲什么"用）
}

// Evidence 收集该记忆库的逐条证据。
func (c Config) Evidence(d Dir) (Evidence, error) {
	c = c.withDefaults()
	out := Evidence{Repo: d.ID, Path: d.Path}
	topics, err := c.topicFiles(d)
	if err != nil {
		return out, err
	}
	usage := c.ReadUsage(d)
	var tr TranscriptReport
	if t, err := c.ScanTranscripts(d, 180*24*time.Hour); err == nil {
		tr = t
	}
	var an AnchorReport
	if a, err := c.CheckAnchors(d); err == nil {
		an = a
	}
	summaries := c.indexSummaries(d)
	listed := c.indexedSet(d)

	now := c.Now()
	for _, name := range topics {
		row := EvidenceRow{
			File:         name,
			Indexed:      listed[name],
			UsageReads:   usage[name].Reads,
			ReadsInLog:   tr.Traces[name].Reads,
			LastReadDays: -1,
			AnchorRefs:   an.RefsByFile[name],
			AnchorDead:   an.ByFile[name],
			Summary:      summaries[name],
		}
		if ago := tr.SilencedFor(name, now); ago >= 0 {
			row.LastReadDays = int(ago.Hours() / 24)
		}
		if raw, err := c.ReadFile(filepath.Join(d.Path, name)); err == nil {
			body := string(raw)
			if typ, ok := frontmatterValue(body, "type"); ok {
				row.Type = typ
			}
			if mod, ok := frontmatterValue(body, "modified"); ok {
				row.Modified = mod
			}
		}
		out.Items = append(out.Items, row)
	}
	out.Count = len(out.Items)
	return out, nil
}

// indexSummaries 取索引里每个文件的摘要（没有索引行就没有摘要）。
func (c Config) indexSummaries(d Dir) map[string]string {
	out := map[string]string{}
	raw, err := c.ReadFile(joinIndex(d))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if target, summary, ok := splitLocator(t); ok {
			out[filepath.Base(target)] = summary
		}
	}
	return out
}

// indexedSet 返回哪些文件出现在索引里。
func (c Config) indexedSet(d Dir) map[string]bool {
	out := map[string]bool{}
	for _, n := range parseIndexEntries(string(readOrEmpty(c, joinIndex(d)))) {
		out[n] = true
	}
	return out
}

// readOrEmpty 读不到就返回空（索引不存在是合法状态）。
func readOrEmpty(c Config, p string) []byte {
	raw, err := c.ReadFile(p)
	if err != nil {
		return nil
	}
	return raw
}
