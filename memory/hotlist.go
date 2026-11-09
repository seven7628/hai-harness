package memory

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// 热点清单：B 档注入的全部内容（§5.1「启动只放路标」的落点）。
//
// # 它解决什么
//
// 全量索引注入是每会话 5–7k token 的固定开销，而实测 57% 的请求根本不需要记忆、
// 54/93 条记忆从未被读过。热点清单的作用是把这份固定成本降到 ~700 token：
// **只列过去真的被读过的 Top-8 条**，其余靠模型自己用标准文件工具读 MEMORY.md。
//
// # 为什么是「按被读次数排序」，不是别的
//
// 设计过程中先后试过三种"挑哪几条"的办法，前两种被实测否定：
//
//  1. **按文件名前缀聚类，生成主题目录**（原方案）。在本地 72 条记忆上跑：
//     聚出 62 组、其中 55 组只有一个文件 —— 压缩率为零。B 档会比全量还长。
//  2. **按 `modified` 倒序**。需要在每次会话启动时对每一条做一次 stat（72 次 I/O），
//     而且"最近改过"与"最近用过"是两件事：一条被反复纠正、长期生效的规矩会
//     因为没再被写而沉底，恰恰是它最该出场的时候。
//  3. **按被读次数**（本实现）。postRead 钩子已经存在，计数是它的副产品，
//     零额外 I/O；且信号本身就是"这条记忆在过去真的帮上过忙"。
//
// # 为什么没有使用数据时按索引原序兜底
//
// 冷启动（新库、或 usage.json 尚未积累）时所有条目并列，稳定排序退化成索引原序
// ——也就是模型自己写索引时的顺序。它未必最优，但它是**模型表达过的意图**，
// 比"按文件名首字母"或"随便取前 8 条"更有依据，也不需要任何额外信息。
//
// # 三条自我约束
//
//  1. **只排序，不删改。** 热点清单是同一批索引行的子集，措辞不改、摘要不重写
//     （只做尾部截断并标 `…`）。宿主搬运事实，不替模型判断。
//  2. **必须说明"这是子集"。** 见 hotFooter——与 truncationNotice 同一个理由：
//     模型看到一份语法完整、内容变短的清单，会判定"这条我没记过"并重记一遍，
//     而它没有任何可察觉的信号。这是宿主唯一必须补的话。
//  3. **不引入新协议。** 页脚只说"完整清单在 MEMORY.md、需要时自己读"——那是官方
//     本来就有的标准文件工具能力，不是行为规则。
const (
	// HotListSize 热点清单条数。K 的取值经过实测：K=6/8/12 在本地 72 条记忆上
	// 分别是 ~511 / ~730 / ~986 token。取 8 是在"够覆盖常用条目"与"固定开销可接受"
	// 之间的选择，可由 Config.Injection 之外的常量调整（改这里要同步更新评估读数）。
	HotListSize = 8
	// HotLineMaxRunes 单条渲染的目标宽度（按 rune 计，不是字节）。
	//
	// 只截**摘要**：定位符（文件路径）任何时候都不截——截出半条路径等于给了一个
	// 打不开的地址，比不列还糟。所以一行可能因为文件名很长而超过这个宽度。
	//
	// 长定位符不吃摘要的额度：文件名占掉 70 个 rune 时若"预算归零"，这条热点就只剩
	// 一个光秃秃的文件名——八个槽位里每个都写着"你读过这个文件"，却不说它讲什么，
	// 那和只给一份文件名清单没有区别。所以摘要至少保留 HotSummaryMinRunes 个字符，
	// 让每个槽位都带一点内容。代价是长文件名那几行会略超 70，实测整体仍在 ~700 token。
	HotLineMaxRunes = 70
	// HotSummaryMinRunes 摘要的最小保留宽度（rune），见 HotLineMaxRunes。
	HotSummaryMinRunes = 12
)

// HotList 是热点清单的产物与度量。
type HotList struct {
	// Total 索引里的条目总数（去空行、去标题行后）。
	Total int
	// Shown 实际列出的条数。
	Shown int
	// Text 注入正文（不含段落标题与目录说明）。
	Text string
	// Ranked 是否按使用计数排过序。false 表示全部并列（冷启动），此时按索引原序。
	Ranked bool
	// Omitted 被省略的条数（Total - Shown）。>0 时 Text 末尾一定有页脚。
	Omitted int
}

// hotEntry 是索引里的一行被解析出来的最小结构。
type hotEntry struct {
	target  string // 定位符：相对记忆目录的路径
	summary string // 定位符之后的说明文字（原样，未截断）
	order   int    // 在索引里出现的行号（稳定排序的次键）
	reads   int    // 被读次数
}

// BuildHotList 从索引正文（boundIndex 的产物）生成热点清单。
//
// usage 为 nil 或空时按索引原序取前 HotListSize 条（见文件头第 3 段）。
// 返回值永不为 nil；索引里没有可识别条目时 Total==0、Text==""。
func BuildHotList(body string, usage map[string]UsageEntry) *HotList {
	entries := parseHotEntries(body)
	hot := &HotList{Total: len(entries)}
	if len(entries) == 0 {
		return hot
	}

	if len(usage) > 0 {
		applyUsage(entries, usage)
		hot.Ranked = true
	}
	// 稳定排序：并列时保持索引原序（sort.SliceStable）。
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].reads > entries[j].reads })

	shown := entries
	if len(shown) > HotListSize {
		shown = shown[:HotListSize]
		hot.Omitted = len(entries) - HotListSize
	}
	hot.Shown = len(shown)

	var b strings.Builder
	for i, e := range shown {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(renderHotLine(e))
	}
	if hot.Omitted > 0 {
		b.WriteString(hotFooter(hot.Total))
	}
	hot.Text = b.String()
	return hot
}

// applyUsage 给每条打上读数。
//
// 两级匹配：先按**相对路径**精确匹配（usage 的 key 是 RelPath 的产物），
// 再退回**basename** 匹配——索引行是人写的，可能写成 `memory/notes/x.md`、
// `./x.md` 或只有文件名。退回 basename 有歧义风险（两个同名文件在子目录里），
// 但那只会让两条共享同一读数，不会让排序出错方向；反过来（匹配不上就当 0 次）
// 会让真实被读过的条目沉底，代价更大。
func applyUsage(entries []hotEntry, usage map[string]UsageEntry) {
	byBase := make(map[string]int, len(usage))
	for k, v := range usage {
		base := k
		if i := strings.LastIndex(k, "/"); i >= 0 {
			base = k[i+1:]
		}
		if v.Reads > byBase[base] {
			byBase[base] = v.Reads
		}
	}
	for i := range entries {
		if u, ok := usage[entries[i].target]; ok {
			entries[i].reads = u.Reads
			continue
		}
		base := entries[i].target
		if j := strings.LastIndex(base, "/"); j >= 0 {
			base = base[j+1:]
		}
		entries[i].reads = byBase[base]
	}
}

// parseHotEntries 把索引正文拆成条目。
//
// 与 list.go 的 parseIndexEntries 是**两套规则**，不合并：那一套只认"无空格的纯文件名行"
// （面板要的是文件名清单），而实际索引行是 `- [x.md](x.md) — 摘要…`，含空格——
// 两套规则各自服务各自的消费者，强行统一会改变面板的既有口径。
//
// 跳过：空行、ATX 标题行（模型可能把索引按主题分组的 `## 组名`）、
// 以及识别不出定位符的行（例如分组说明文字）。识别不出就**不当作条目**，
// 而不是把它当成一条没有路径的记忆塞进清单——那会让 Total 虚高、
// 让页脚说"共 N 条"与实际不符。
func parseHotEntries(body string) []hotEntry {
	var out []hotEntry
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		target, summary, ok := splitLocator(t)
		if !ok {
			continue
		}
		out = append(out, hotEntry{target: target, summary: summary, order: len(out)})
	}
	return out
}

// splitLocator 从一行里取出「定位符」与「其后的说明」。
//
// 支持三种模型实际写过的形态（按常见度）：
//
//   - [name.md](name.md) — 说明…     ← 官方示例与实际索引的主流形态
//   - `notes/x.md` — 说明…
//   - name.md — 说明…
//
// 只取**第一个**定位符：一行里出现多个链接时，第一个是这一条的标题。
func splitLocator(line string) (target, summary string, ok bool) {
	if i := strings.Index(line, "]("); i >= 0 {
		rest := line[i+2:]
		if j := strings.Index(rest, ")"); j >= 0 {
			return normalizeTarget(rest[:j]), cleanSummary(rest[j+1:]), true
		}
		return "", "", false
	}
	if i := strings.Index(line, "`"); i >= 0 {
		rest := line[i+1:]
		if j := strings.Index(rest, "`"); j >= 0 {
			return normalizeTarget(rest[:j]), cleanSummary(rest[j+1:]), true
		}
		return "", "", false
	}
	// 无 markdown 结构：取第一个以 .md 结尾的字段。
	for i, f := range strings.Fields(line) {
		if strings.HasSuffix(strings.ToLower(f), ".md") {
			return normalizeTarget(f), cleanSummary(strings.Join(strings.Fields(line)[i+1:], " ")), true
		}
	}
	return "", "", false
}

// normalizeTarget 把各种写法收敛成"相对记忆目录的路径"：
// 去掉 `memory/` 前缀（模型常按 `<dir>/memory/…` 写）、`./` 前缀与首尾空白。
func normalizeTarget(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "./")
	if i := strings.Index(t, "memory/"); i == 0 {
		t = t[len("memory/"):]
	}
	return t
}

// cleanSummary 去掉定位符后面那串分隔符（`—` / `-` / `：` / `·` / 空白）。
//
// 保留正文里的破折号与标点：只剥**开头连续**的装饰字符，不做任何重写。
func cleanSummary(s string) string {
	return strings.TrimLeft(s, " \t—–-:：·|、，,")
}

// renderHotLine 渲染一条：`- 路径 — 摘要…`，摘要按 HotLineMaxRunes 截断。
//
// 定位符不截（见 HotLineMaxRunes 的注释）；预算不够时**只丢摘要**，
// 因为一条没有摘要的热点仍然可用（点开就知道），一条没有路径的热点是废的。
func renderHotLine(e hotEntry) string {
	head := "- " + e.target
	if e.summary == "" {
		return head
	}
	budget := HotLineMaxRunes - utf8.RuneCountInString(head) - len(" — ")
	if budget < HotSummaryMinRunes {
		budget = HotSummaryMinRunes
	}
	return head + " — " + truncateRunes(e.summary, budget)
}

// truncateRunes 截到 n 个 rune，截断时以 `…` 结尾（`…` 计入 n）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	rs := []rune(s)
	// 结尾若是空白、标点或未闭合的 markdown 装饰，跟着一起截掉：
	// `…` 前面跟着逗号更好读；而 "终态 `…" 这种半边反引号在真实索引里很常见
	// （摘要里满是 `code` 与 **粗体**），留着会让模型把它当成未闭合的代码段。
	end := n - 1
	for end > 0 && strings.ContainsRune(" \t，,、；;：:`*", rs[end-1]) {
		end--
	}
	return string(rs[:end]) + "…"
}

// hotFooter 页脚：说明"这是子集 + 完整清单在哪 + 共多少条"。
//
// 三件事缺一不可：
//
//   - **是子集**：不说的话模型会把它当全量清单，据此判定"没记过"并重记一遍；
//   - **在哪**：`MEMORY.md` 是官方索引文件名，模型已经有标准文件工具；
//   - **共多少条**：让"少了 64 条"成为可感知的事实，而不是无从判断。
//
// 不点名被省略的条目（与 truncationNotice 的自我约束 1 一致）：点名等于把
// 省略变成"这些不重要"的价值判断，而宿主没有这个判断权。
func hotFooter(total int) string {
	var b strings.Builder
	b.WriteString("\n\n（以上按被读次数列出，非完整清单；")
	b.WriteString("完整索引共 ")
	b.WriteString(itoa(total))
	b.WriteString(" 条，见 MEMORY.md，需要时自行读取。）")
	return b.String()
}

// itoa 小工具（避免为两个调用点引入 strconv 的额外 import 面）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
