package memory

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 冲突检测（Dream v3）：这条记忆后来**被推翻过吗**。
//
// # 为什么这层最终还是交给模型
//
// "后来有人说过相反的话"不是一个字符串问题：同一句话可以是否定、可以是补充、也可以是
// 在讲另一件事。规则能做的只有"把候选片段捞出来"，判断必须由模型做——它读得懂"不对，
// 应该是 X"和"对，就是 X"的区别。
//
// 所以分工是：
//
//	宿主：从转录里捞出**可能相关**的片段（锚点共现 ≥2），限定在记忆写入之后
//	模型：判断这些片段是 uphold / contradict / unrelated
//	宿主：把结论存进 .state/conflicts.json，供裁决与报告使用
//
// # 为什么限定"之后"
//
// 写入之前的对话不可能否定这条记忆——它还不存在。不设这个界，等于让模型去比较
// 一堆与它无关的历史。
//
// # 为什么要求锚点共现 ≥2
//
// 单个锚点噪声太大：`user_id`、`agent-server-g` 这种词几乎每条会话都有。实测用单锚点
// 会给 69/71 条记忆都配上一堆无关片段，模型读完只会说"无关"。

// ConflictCandidate 是一条记忆的待判候选。
type ConflictCandidate struct {
	File     string   `json:"file"`
	Anchors  []string `json:"anchors"`
	Snippets []string `json:"snippets"`
}

// ConflictVerdict 是模型给出的判定。
type ConflictVerdict struct {
	File      string `json:"file"`
	Verdict   string `json:"verdict"` // upheld / contradicted / unrelated / no_session
	Reason    string `json:"reason"`
	CheckedAt string `json:"checked_at"`
}

// 三种判定 + 一种"没材料"。
const (
	ConflictUpheld       = "upheld"       // 后来的会话与它一致
	ConflictContradicted = "contradicted" // 被推翻 / 被纠正
	ConflictUnrelated    = "unrelated"    // 片段与它无关
	ConflictNoSession    = "no_session"   // 没有可用片段
)

// 一条记忆最多带几个片段给模型；每个片段截断到这里。
const (
	conflictMaxSnippets   = 5
	conflictSnippetChars  = 400
	conflictMinAnchorsHit = 2
)

// 明显的"这是结论性发言"的锚点：反引号里的长标识符与代码路径。
var conflictAnchor = regexp.MustCompile(
	"`([A-Za-z_][A-Za-z0-9_.]{7,}|[A-Za-z0-9_./-]+\\.(?:go|ts|tsx|py|json|yaml|yml))(?::\\d+)?`")

// ConflictCandidates 捞出每条记忆的待判片段。
//
// 只读；不改任何文件。
func (c Config) ConflictCandidates(d Dir, window time.Duration) ([]ConflictCandidate, error) {
	c = c.withDefaults()
	topics, err := c.topicFiles(d)
	if err != nil {
		return nil, err
	}
	anchors := map[string][]string{}
	modified := map[string]time.Time{}
	for _, name := range topics {
		raw, err := c.ReadFile(filepath.Join(d.Path, name))
		if err != nil {
			continue
		}
		body := string(raw)
		set := map[string]bool{}
		var list []string
		for _, m := range conflictAnchor.FindAllStringSubmatch(body, -1) {
			a := m[1]
			if len(a) < 8 || set[a] {
				continue
			}
			set[a] = true
			list = append(list, a)
		}
		if len(list) == 0 {
			continue
		}
		anchors[name] = list
		if mod, ok := frontmatterValue(body, "modified"); ok {
			if t, err := time.Parse(time.RFC3339, mod); err == nil {
				modified[name] = t
			}
		}
	}

	found := map[string][]string{}
	root := c.eventsRoot()
	dirs, err := c.ReadDir(root)
	if err != nil {
		return nil, err
	}
	since := c.Now().Add(-window)
	for _, de := range dirs {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(root, de.Name())
		files, _ := c.ReadDir(dir)
		for _, fe := range files {
			if fe.IsDir() || !isEventLogName(fe.Name()) {
				continue
			}
			c.collectSnippets(filepath.Join(dir, fe.Name()), anchors, modified, since, found)
		}
	}

	var out []ConflictCandidate
	for name, snippets := range found {
		out = append(out, ConflictCandidate{File: name, Anchors: anchors[name], Snippets: snippets})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// collectSnippets 从一个会话文件里捞出与各条记忆锚点共现的用户发言。
func (c Config) collectSnippets(path string, anchors map[string][]string,
	modified map[string]time.Time, since time.Time, found map[string][]string) {
	raw, err := c.readEventFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"user_texts"`) {
			continue
		}
		var ev struct {
			UserTexts []string `json:"user_texts"`
			Timestamp string   `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if !since.IsZero() && !ts.IsZero() && ts.Before(since) {
			continue
		}
		text := strings.Join(ev.UserTexts, "\n")
		if text == "" {
			continue
		}
		for name, as := range anchors {
			if len(found[name]) >= conflictMaxSnippets {
				continue
			}
			// 写入之前的发言不可能否定它。
			if m, ok := modified[name]; ok && !ts.IsZero() && ts.Before(m) {
				continue
			}
			hit := 0
			for _, a := range as {
				if strings.Contains(text, a) {
					hit++
				}
			}
			if hit < conflictMinAnchorsHit {
				continue
			}
			snip := strings.TrimSpace(text)
			if r := []rune(snip); len(r) > conflictSnippetChars {
				snip = string(r[:conflictSnippetChars]) + "…"
			}
			found[name] = append(found[name], snip)
		}
	}
}

// conflictsPath 返回判定文件路径。
func conflictsPath(d Dir) string { return filepath.Join(d.Path, stateDirName, "conflicts.json") }

// ReadConflicts 读出已有判定。
func (c Config) ReadConflicts(d Dir) map[string]ConflictVerdict {
	out := map[string]ConflictVerdict{}
	raw, err := c.withDefaults().ReadFile(conflictsPath(d))
	if err != nil {
		return out
	}
	var list []ConflictVerdict
	if err := json.Unmarshal(raw, &list); err != nil {
		return out
	}
	for _, v := range list {
		out[v.File] = v
	}
	return out
}

// SaveConflicts 合并写入判定（按文件名覆盖，保留其它）。
func (c Config) SaveConflicts(d Dir, verdicts []ConflictVerdict) error {
	if len(verdicts) == 0 {
		return nil
	}
	cur := c.ReadConflicts(d)
	for _, v := range verdicts {
		if v.CheckedAt == "" {
			v.CheckedAt = c.withDefaults().Now().UTC().Format(time.RFC3339)
		}
		cur[v.File] = v
	}
	list := make([]ConflictVerdict, 0, len(cur))
	for _, v := range cur {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].File < list[j].File })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	c = c.withDefaults()
	if err := c.MkdirAll(filepath.Dir(conflictsPath(d)), 0o755); err != nil {
		return err
	}
	return c.atomicWrite(conflictsPath(d), raw)
}

// conflictSummary 给报告用的一句话。
func conflictSummary(v ConflictVerdict) string {
	switch v.Verdict {
	case ConflictContradicted:
		return "被后续会话推翻"
	case ConflictUpheld:
		return "后续会话印证"
	case ConflictNoSession:
		return "无可用片段"
	default:
		return "片段无关"
	}
}
