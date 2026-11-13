package memory

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// 做梦 v1：**只报不改**（P4）。
//
// # 为什么第一版什么都不敢改
//
// 记忆是用户与模型共同积累的事实，删错一条的代价（"它明明记着，现在不记得了"）远大于
// 多留一条的代价（多几行索引）。而离线整理最危险的地方在于**没有人当场纠正**——
// 会话里的错误模型下一轮就能发现，离线批处理的错误要等很久以后才暴露。
//
// 所以 v1 只产出一份报告：哪些没登记、哪些索引里已经没文件、哪些字段缺失、
// 哪些从未被读过、哪些内容疑似重复。**一个字节都不写。** 等这些读数被真实用过一阵，
// 再逐项决定哪一类可以自动化。
//
// # 为什么"从未被读"不算删除依据
//
// 一条"只在发版那天有用"的记忆可能 90 天没人读过，但那天必须存在。usage 是**排序信号**
// （决定谁进热点清单），不是生命周期信号——这条界线在 usage.go 里也写过一遍，
// 这里再写一遍是因为做梦正是最容易越界的地方。
//
// 报告里因此把这组单独列出并注明：它回答的是"我该不该给它写一条更好的摘要"，
// 不是"我该不该删掉它"。

// DreamReport 是一次只读审计的全部产出。
type DreamReport struct {
	Repo        string
	Path        string
	Topics      int
	Index       DreamIndexStats
	NotIndexed  []string
	Stale       []string
	MissingType []string
	MissingMeta []string
	NeverRead   []string
	Duplicates  [][2]string
	// Transcripts 是会话转录里的引用轨迹（Dream v2）。
	Transcripts TranscriptReport
	// Conflicts 是冲突判定（Dream v3）：哪些记忆被后续会话印证 / 推翻。
	Conflicts map[string]ConflictVerdict
	// Anchors 是代码锚点核对结果（Dream v1.5）。Unbound=true 表示这个记忆库还没绑定
	// 本地仓库路径，整段跳过——**跳过不等于没问题**，报告里会写明。
	Anchors AnchorReport
	// topics 是全部条目名（内部用：转录统计要一份完整清单）。不导出——报告是给
	// 人看的，条目清单只是算中间量。
	topics []string
}

// DreamIndexStats 是索引的体量与限额读数（官方 200 行 / 25KB）。
type DreamIndexStats struct {
	Lines    int
	Bytes    int
	OverLine bool
	OverByte bool
}

// HasIssues 判断这份报告有没有值得看的内容。
func (r DreamReport) HasIssues() bool {
	return len(r.Anchors.Dead) > 0 || len(r.NotIndexed) > 0 || len(r.Stale) > 0 || len(r.MissingType) > 0 ||
		len(r.MissingMeta) > 0 || len(r.Duplicates) > 0 ||
		r.Index.OverLine || r.Index.OverByte
}

// Dream 生成一份报告。**只读**：本函数不调用任何写出口（atomicWrite / WriteFile / Rename）。
func (c Config) Dream(d Dir) (DreamReport, error) {
	c = c.withDefaults()
	out := DreamReport{Repo: d.ID, Path: d.Path}

	// 索引体量与登记表。
	rawIndex, idxErr := c.ReadFile(joinIndex(d))
	listed := map[string]bool{}
	if idxErr == nil {
		s := string(rawIndex)
		out.Index.Lines = strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
		if s == "" {
			out.Index.Lines = 0
		}
		out.Index.Bytes = len(s)
		out.Index.OverLine = out.Index.Lines > MaxIndexLines
		out.Index.OverByte = out.Index.Bytes > MaxIndexBytes
		for _, n := range parseIndexEntries(s) {
			listed[n] = true
		}
	}

	usage := c.ReadUsage(d)
	titles := map[string][]string{}

	entries, err := c.ReadDir(d.Path)
	if err != nil && !isNotExist(err) {
		return out, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".md") || strings.EqualFold(name, IndexFileName) {
			continue
		}
		out.Topics++
		if !listed[name] {
			out.NotIndexed = append(out.NotIndexed, name)
		}
		raw, err := c.ReadFile(filepath.Join(d.Path, name))
		if err != nil {
			continue
		}
		body := string(raw)
		if hasFrontmatter(raw) {
			if typ, ok := frontmatterValue(body, "type"); !ok || typ == "" {
				out.MissingType = append(out.MissingType, name)
			} else if !isMemoryType(typ) {
				out.MissingType = append(out.MissingType, name+"（type="+typ+"）")
			}
			if _, ok := frontmatterValue(body, "modified"); !ok {
				out.MissingMeta = append(out.MissingMeta, name)
			}
		}
		if usage[name].Reads == 0 {
			out.NeverRead = append(out.NeverRead, name)
		}
		out.addTopic(name)
		if key := titleKey(body); key != "" {
			titles[key] = append(titles[key], name)
		}
	}

	// 索引里登记了、磁盘上没有。
	for n := range listed {
		if _, err := c.ReadFile(filepath.Join(d.Path, n)); err != nil {
			out.Stale = append(out.Stale, n)
		}
	}

	// 标题相同的组 → 重复候选。
	for _, names := range titles {
		if len(names) > 1 {
			sort.Strings(names)
			out.Duplicates = append(out.Duplicates, [2]string{names[0], names[1]})
		}
	}

	sort.Strings(out.NotIndexed)
	sort.Strings(out.Stale)
	sort.Strings(out.MissingType)
	sort.Strings(out.MissingMeta)
	sort.Strings(out.NeverRead)
	sort.Slice(out.Duplicates, func(i, j int) bool { return out.Duplicates[i][0] < out.Duplicates[j][0] })

	// 会话转录：失败只记在报告里，不让整份盘点失败。
	if tr, err := c.ScanTranscripts(d, 180*24*time.Hour); err == nil {
		out.Transcripts = tr
	} else {
		out.Transcripts = TranscriptReport{Unbound: true}
	}

	out.Conflicts = c.ReadConflicts(d)

	// 代码锚点核对：失败只记在报告里，不让整份盘点失败。
	if ar, err := c.CheckAnchors(d); err == nil {
		out.Anchors = ar
	} else {
		out.Anchors = AnchorReport{Root: "", Unbound: true}
	}
	return out, nil
}

// Markdown 渲染报告。
//
// 每节都带一句"这意味着什么"，因为报告的读者是人和模型，不是 SQL 客户端——
// 只有数字没有含义的报表，看完还是不知道该干什么。
func (r DreamReport) Markdown(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 记忆盘点 · %s\n\n", r.Repo)
	fmt.Fprintf(&b, "目录：`%s`　条目 **%d**　索引 **%d 行 / %d 字节**（限额 %d 行 / %d 字节）\n\n",
		r.Path, r.Topics, r.Index.Lines, r.Index.Bytes, MaxIndexLines, MaxIndexBytes)
	b.WriteString("> **只报不改**：本报告不写回任何文件。\n\n")

	section := func(title, why string, items []string, limit int) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s（%d）\n\n%s\n\n", title, len(items), why)
		for i, it := range items {
			if i >= limit {
				fmt.Fprintf(&b, "- …另有 %d 条\n", len(items)-limit)
				break
			}
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}

	if r.Index.OverLine || r.Index.OverByte {
		b.WriteString("## 索引已超限\n\n")
		b.WriteString("超出官方 200 行 / 25KB 的部分在下次加载时被丢弃——**模型看不见它们，" +
			"但文件还在**。这是唯一一处「内容仍在、却已经不在注入里」的状态。\n\n")
	}

	// 转录：先说"扫没扫"，再说"扫出什么"。这里只报"多久没被碰过"，**不下失效结论**。
	if r.Transcripts.Unbound {
		b.WriteString("## 会话转录：未扫描\n\n这个记忆库没有绑定工作区（`.state/repo.json` 的 `workspace`），" +
			"扫不了对应的 events 目录。**这不等于没人用过。**\n\n")
	} else {
		silent := r.Transcripts.LongSilent(r.topicList(), now, 90*24*time.Hour)
		fmt.Fprintf(&b, "## 会话转录（近 %s）\n\n扫了 %d 个事件文件；被读过的记忆 **%d** 条，"+
			"其中 **超过 90 天没被碰过或从未出现过** 的 **%d** 条。\n\n",
			"180 天", r.Transcripts.Files, len(r.Transcripts.Traces), len(silent))
		b.WriteString("> 沉寂只说明「没人用过」，不说明「结论错了」——最久的那条可能等在某个" +
			"只有特定日子才发生的场景里。它排在前面是为了让人先看它，不是为了让系统删它。\n\n")
		limit := 10
		for i, n := range silent {
			if i >= limit {
				fmt.Fprintf(&b, "- …另有 %d 条\n", len(silent)-limit)
				break
			}
			ago := r.Transcripts.SilencedFor(n, now)
			if ago < 0 {
				fmt.Fprintf(&b, "- %s ⟵ 转录里从未出现\n", n)
				continue
			}
			fmt.Fprintf(&b, "- %s ⟵ 最后引用 %d 天前\n", n, int(ago.Hours()/24))
		}
		b.WriteString("\n")
	}

	// 冲突：最重要的一条结论单独列出来，因为它是唯一会**改变记忆内容真假**的证据。
	if len(r.Conflicts) > 0 {
		var contra, upheld, other int
		for _, v := range r.Conflicts {
			switch v.Verdict {
			case ConflictContradicted:
				contra++
			case ConflictUpheld:
				upheld++
			default:
				other++
			}
		}
		fmt.Fprintf(&b, "## 冲突核验（已判 %d 条）\n\n", len(r.Conflicts))
		fmt.Fprintf(&b, "被后续会话印证 **%d** 条，被推翻 **%d** 条，片段无关 %d 条。\n\n", upheld, contra, other)
		if contra > 0 {
			b.WriteString("被推翻的条目：\n\n")
			files := make([]string, 0, contra)
			for f, v := range r.Conflicts {
				if v.Verdict == ConflictContradicted {
					files = append(files, f)
				}
			}
			sort.Strings(files)
			for i, f := range files {
				if i >= 8 {
					fmt.Fprintf(&b, "- …另有 %d 条\n", len(files)-8)
					break
				}
				fmt.Fprintf(&b, "- `%s` — %s\n", f, r.Conflicts[f].Reason)
			}
			b.WriteString("\n")
		} else {
			b.WriteString("**没有发现被推翻的条目。** 注意这只说明「候选片段里没有反证」，" +
				"不等于「一定没被推翻」——\n反证往往出现在不含原锚点的句子里（「那个改回去」），" +
				"这类只能靠人读。\n\n")
		}
	}

	// 锚点：先说"核没核"，再说"核出什么"——没核与核过没问题，是两件事。
	if r.Anchors.Unbound {
		b.WriteString("## 代码锚点：未核对\n\n" +
			"这个记忆库还没有绑定本地仓库路径（`.state/repo.json`），所以没法核对" +
			"记忆里引用的代码文件是否还在。**这不等于没问题。**\n" +
			"绑定一次即可：`go run ./memory/cmd/memory_dream --bind <工作区路径>`。\n\n")
	} else if len(r.Anchors.Dead) > 0 {
		fmt.Fprintf(&b, "## 代码锚点失效（%d 条引用，涉 %d 条记忆）\n\n",
			len(r.Anchors.Dead), len(r.Anchors.ByFile))
		fmt.Fprintf(&b, "核对仓库 `%s`（%d 个文件）；共抽到 %d 条引用。\n\n",
			r.Anchors.Root, r.Anchors.RepoFiles, r.Anchors.Refs)
		b.WriteString("这些引用指向的文件在仓库里已经找不到了。可能是文件改名/删除，" +
			"也可能这条记忆本身已经过时——**两种都要人看一眼**，所以只报不改。\n\n")
		limit := 12
		for i, a := range r.Anchors.Dead {
			if i >= limit {
				fmt.Fprintf(&b, "- …另有 %d 条\n", len(r.Anchors.Dead)-limit)
				break
			}
			fmt.Fprintf(&b, "- `%s` → `%s`\n", a.File, a.Ref)
		}
		b.WriteString("\n")
	} else if r.Anchors.RepoFiles > 0 {
		fmt.Fprintf(&b, "## 代码锚点：全部可核对（%d 条引用 / 仓库 %d 个文件）\n\n"+
			"记忆里引用的代码文件都还在。\n\n", r.Anchors.Refs, r.Anchors.RepoFiles)
	}

	section("未登记", "这些文件不在 MEMORY.md 里，启动注入不会包含它们（内容仍可被 read_file 读到）。",
		r.NotIndexed, 10)
	section("索引里的陈旧条目", "MEMORY.md 登记了但磁盘上没有这个文件。官方对陈旧条目不报错也不自动清理。",
		r.Stale, 10)
	section("缺少 type", "有 frontmatter 但没有 type（或取值不在 user/feedback/project/reference 内）。",
		r.MissingType, 10)
	section("缺少 modified", "有 frontmatter 但没有 modified；下次写入时会被补上。", r.MissingMeta, 10)
	section("从未被读过", "usage 读数为 0。**这不是删除依据**——只在发版那天有用的记忆也会长期是 0；"+
		"它回答的是「要不要给它写一条更好的摘要」，不是「要不要删」。", r.NeverRead, 10)

	if len(r.Duplicates) > 0 {
		fmt.Fprintf(&b, "## 疑似重复（%d 对）\n\n标题相同的两条记忆，多半是同一件事写了两次。\n\n", len(r.Duplicates))
		for i, p := range r.Duplicates {
			if i >= 10 {
				fmt.Fprintf(&b, "- …另有 %d 对\n", len(r.Duplicates)-10)
				break
			}
			fmt.Fprintf(&b, "- %s ↔ %s\n", p[0], p[1])
		}
		b.WriteString("\n")
	}

	if !r.HasIssues() {
		b.WriteString("没有发现需要处理的问题。\n")
	}
	return b.String()
}

// topicList 返回这个记忆库里的全部条目名（供转录统计用）。
//
// 为什么不把 allTopics 放进 Report：报告是给人看的，条目清单是算中间量。
func (r DreamReport) topicList() []string { return r.topics }

func (r *DreamReport) addTopic(name string) { r.topics = append(r.topics, name) }

// titleKey 取正文第一个标题，用于重复检测（归一化后比较）。
func titleKey(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			return normalizeTitle(t)
		}
	}
	return ""
}

// normalizeTitle 去掉大小写、空白与标点，只留下"讲的是不是同一件事"的骨架。
func normalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
