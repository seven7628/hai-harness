package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P1 注入档位与热点清单的回归测试（2026-11）。
//
// 分工：本文件的测试只测"档位怎么选、热点清单怎么排"；
// 全量形态的逐字节断言留在 index_test.go（那些一律显式 InjectionFull）。
// 这样"默认值是哪个"和"某种形态渲染成什么样"两件事不会互相掩护。

// —————————————————————— 档位选择 ——————————————————————

// 默认档位是 hot：零值 Config 必须落在"省"的那一侧。
//
// 反例警示：默认值一旦漂回 full，本测试是唯一的拦截点——index_test.go 里全部
// 显式声明了 A 档，它们会照样全绿。
func TestDefaultTierIsHot(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n- b.md\n")

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Tier != InjectionHot {
		t.Fatalf("Tier = %q, want %q（零值 Config 的默认档位）", inj.Tier, InjectionHot)
	}
	if inj.Hot == nil {
		t.Fatal("hot 档必须带回 Hot 度量（Total/Shown 是注入预算的读数来源）")
	}
}

// A 档：Tier 报 full、不带 Hot 度量、正文含全部条目。
func TestFullTierDisablesHotList(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n- b.md\n- c.md\n")

	inj, err := fullCfg().BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Tier != InjectionFull {
		t.Fatalf("Tier = %q, want full", inj.Tier)
	}
	if inj.Hot != nil {
		t.Fatal("full 档不该生成热点清单")
	}
	for _, want := range []string{"- a.md", "- b.md", "- c.md"} {
		if !strings.Contains(inj.Text, want) {
			t.Fatalf("full 档正文缺少 %q：\n%s", want, inj.Text)
		}
	}
}

// 环境变量压过 Config：评估脚本要能在不改用户配置的前提下固定档位。
func TestEnvOverridesInjectionTier(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n")

	t.Setenv(EnvMemoryInjection, InjectionFull)
	inj, err := (Config{Injection: InjectionHot}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Tier != InjectionFull {
		t.Fatalf("Tier = %q, want full（环境变量应压过 Config.Injection）", inj.Tier)
	}
}

// 未识别值按"未设"处理：拼错的可选覆盖不该让应用起不来，也不该静默变成别的档。
func TestUnknownTierFallsBackToDefault(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n")

	inj, err := (Config{Injection: "fulll"}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Tier != InjectionHot {
		t.Fatalf("Tier = %q, want %q（拼错应按未设处理）", inj.Tier, InjectionHot)
	}
}

// gate 在 P2 之前降级为 hot，且**如实报出降级后的档位**——
// 否则调用方会以为自己已经在跑闸门。
func TestGateDegradesToHotUntilImplemented(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n")

	t.Setenv(EnvMemoryInjection, InjectionGate)
	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Tier != InjectionHot {
		t.Fatalf("Tier = %q, want %q（gate 尚未实现，必须显式降级）", inj.Tier, InjectionHot)
	}
}

// —————————————————————— 热点清单排序 ——————————————————————

// 被读过的条目排在前：这是热点清单唯一的排序信号。
func TestHotListRanksByReadCount(t *testing.T) {
	body := "- a.md — 甲\n- b.md — 乙\n- c.md — 丙\n- d.md — 丁\n"
	usage := map[string]UsageEntry{
		"c.md": {Reads: 9},
		"d.md": {Reads: 3},
	}

	hot := BuildHotList(body, usage)
	lines := strings.Split(hot.Text, "\n")
	if !strings.HasPrefix(lines[0], "- c.md") {
		t.Fatalf("第一条应为读数最高的 c.md，得到 %q\n%s", lines[0], hot.Text)
	}
	if !strings.HasPrefix(lines[1], "- d.md") {
		t.Fatalf("第二条应为 d.md，得到 %q\n%s", lines[1], hot.Text)
	}
	// 并列（读数都是 0）时保持索引原序：a 在 b 前。
	if !strings.HasPrefix(lines[2], "- a.md") || !strings.HasPrefix(lines[3], "- b.md") {
		t.Fatalf("并列时应保持索引原序，得到 %q / %q", lines[2], lines[3])
	}
}

// 冷启动（没有任何 usage）时退化为索引原序，而不是"随便挑 8 条"。
func TestHotListColdStartKeepsIndexOrder(t *testing.T) {
	var b strings.Builder
	for _, n := range []string{"a.md", "b.md", "c.md"} {
		b.WriteString("- " + n + "\n")
	}
	hot := BuildHotList(b.String(), nil)
	if hot.Ranked {
		t.Fatal("没有 usage 时不该报 Ranked")
	}
	if !strings.HasPrefix(hot.Text, "- a.md") {
		t.Fatalf("冷启动应保持索引原序，得到：\n%s", hot.Text)
	}
}

// 只有 Top-8 进注入，且**必须**带页脚说明这是子集。
//
// 页脚是这一档的安全带：没有它，模型会把精简清单当成完整清单，
// 判定"这条我没记过"并重记一遍（与 truncationNotice 同一循环）。
func TestHotListCapsAtEightAndSaysItIsPartial(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("- f")
		b.WriteString(itoa(i))
		b.WriteString(".md — 说明\n")
	}
	hot := BuildHotList(b.String(), nil)

	if hot.Total != 20 || hot.Shown != HotListSize || hot.Omitted != 20-HotListSize {
		t.Fatalf("Total/Shown/Omitted = %d/%d/%d, want 20/%d/%d",
			hot.Total, hot.Shown, hot.Omitted, HotListSize, 20-HotListSize)
	}
	if n := strings.Count(hot.Text, "\n- ") + 1; n != HotListSize {
		t.Fatalf("正文条目数 = %d, want %d\n%s", n, HotListSize, hot.Text)
	}
	if !strings.Contains(hot.Text, "非完整清单") || !strings.Contains(hot.Text, "共 20 条") {
		t.Fatalf("被省略时必须说明是子集并给出总数，得到：\n%s", hot.Text)
	}
}

// 条目装得下时不该出现页脚：没有省略，就没有可说明的省略。
func TestHotListNoFooterWhenEverythingFits(t *testing.T) {
	hot := BuildHotList("- a.md\n- b.md\n", nil)
	if hot.Omitted != 0 {
		t.Fatalf("Omitted = %d, want 0", hot.Omitted)
	}
	if strings.Contains(hot.Text, "非完整清单") {
		t.Fatalf("没有省略时不该出现子集页脚：\n%s", hot.Text)
	}
}

// 长文件名的定位符**绝不截断**：截出半条路径等于给一个打不开的地址。
func TestHotListNeverTruncatesLocator(t *testing.T) {
	long := strings.Repeat("x", 80) + ".md"
	hot := BuildHotList("- ["+long+"]("+long+") — "+strings.Repeat("说", 200)+"\n", nil)

	line := strings.Split(hot.Text, "\n")[0]
	if !strings.HasPrefix(line, "- "+long) {
		t.Fatalf("定位符被截断了：%q", line)
	}
	if !strings.HasSuffix(line, "…") {
		t.Fatalf("超长摘要应以 … 结尾：%q", line)
	}
	if got := len([]rune(line)) - len([]rune("- "+long+" — ")) + 1; got < HotSummaryMinRunes {
		t.Fatalf("长定位符不该把摘要挤没：摘要 %d rune < %d（%q）", got, HotSummaryMinRunes, line)
	}
	if strings.Contains(line, "http") || strings.Contains(line, "](") {
		t.Fatalf("热点清单应渲染成 `- 路径 — 摘要`，不该再带 markdown 链接：%q", line)
	}
}

// 摘要按 rune 截断（不是字节）：中文按 70 个字符算，不是 70 字节。
func TestHotListTruncatesByRune(t *testing.T) {
	hot := BuildHotList("- a.md — "+strings.Repeat("中", 100)+"\n", nil)
	line := strings.Split(hot.Text, "\n")[0]
	if n := len([]rune(line)); n > HotLineMaxRunes {
		t.Fatalf("行宽 %d rune 超过上限 %d：%q", n, HotLineMaxRunes, line)
	}
	if strings.Count(line, "中") < 10 {
		t.Fatalf("截得太狠，摘要几乎没保留：%q", line)
	}
}

// 索引里的分组标题与散文行不该被当成条目：它会让 Total 虚高、页脚数字撒谎。
func TestHotListIgnoresHeadingsAndProse(t *testing.T) {
	body := "# Memory Index\n\n## 构建\nthese are not memories\n- a.md\n"
	hot := BuildHotList(body, nil)
	if hot.Total != 1 {
		t.Fatalf("Total = %d, want 1（标题与散文行不算条目）", hot.Total)
	}
}

// —————————————————————— usage 落盘 ——————————————————————

// RecordRead 累加并落盘在 .state/usage.json；ReadUsage 能读回来。
func TestRecordReadPersistsUsage(t *testing.T) {
	d := memDir(t)
	topic := filepath.Join(d.Path, "a.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := Config{}.withDefaults()
	for i := 0; i < 3; i++ {
		if err := c.RecordRead(d, topic); err != nil {
			t.Fatalf("第 %d 次 RecordRead: %v", i+1, err)
		}
	}

	got := c.ReadUsage(d)
	if got["a.md"].Reads != 3 {
		t.Fatalf("Reads = %d, want 3（%v）", got["a.md"].Reads, got)
	}
	if got["a.md"].Last.IsZero() {
		t.Fatal("Last 应被填上")
	}
	if _, err := os.Stat(usagePath(d)); err != nil {
		t.Fatalf("usage.json 未落盘：%v", err)
	}
}

// usage.json 不是一条记忆：IsTopic 必须拒绝它，否则它会被注入/被读到事件里。
func TestUsageFileIsNotATopic(t *testing.T) {
	d := memDir(t)
	if d.IsTopic(usagePath(d)) {
		t.Fatal("usage.json 不该被当成记忆条目")
	}
	if d.IsIndex(usagePath(d)) {
		t.Fatal("usage.json 不该被当成索引")
	}
}

// 坏掉的 usage.json 只退化成"没有统计数据"，不能让注入失败。
func TestCorruptUsageDegradesToEmpty(t *testing.T) {
	d := memDir(t)
	if err := os.MkdirAll(filepath.Dir(usagePath(d)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath(d), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (Config{}).ReadUsage(d); len(got) != 0 {
		t.Fatalf("坏文件应退化为空，得到 %v", got)
	}
	// 记录仍然能工作（从空状态重建）。
	if err := (Config{}).RecordRead(d, filepath.Join(d.Path, "a.md")); err != nil {
		t.Fatalf("坏文件后 RecordRead 应能重建：%v", err)
	}
	if got := (Config{}).ReadUsage(d); got["a.md"].Reads != 1 {
		t.Fatalf("重建后 Reads = %d, want 1", got["a.md"].Reads)
	}
}

// usage 的 key 是相对路径（"/" 分隔），子目录里的记忆也能被计数。
func TestUsageKeysAreSlashRelative(t *testing.T) {
	d := memDir(t)
	sub := filepath.Join(d.Path, "notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Config{}).RecordRead(d, filepath.Join(sub, "x.md")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(usagePath(d))
	if err != nil {
		t.Fatal(err)
	}
	var st usageState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Files["notes/x.md"]; !ok {
		t.Fatalf("key 应为 notes/x.md，得到 %v", st.Files)
	}
}

// 时间戳只是给人看的：它**不**参与过期判定（没有任何 TTL 逻辑读它）。
// 这条测试把"Last 存在"与"Last 无用"分开钉住，防止后来者顺手拿它做过期。
func TestUsageLastIsRecordedButNotAVerdict(t *testing.T) {
	d := memDir(t)
	c := Config{Now: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := c.RecordRead(d, filepath.Join(d.Path, "a.md")); err != nil {
		t.Fatal(err)
	}
	if got := c.ReadUsage(d)["a.md"].Last.Unix(); got != 1700000000 {
		t.Fatalf("Last = %d, want 1700000000", got)
	}
	// 记忆文件不存在、时间戳很旧，仍然不算"过期"：本包没有过期机制。
	if inj, err := (Config{}).BuildInjection(d); err != nil || inj.Empty() {
		t.Fatalf("空索引仍应注入说明段（err=%v）", err)
	}
}

// —————————————————————— 注入与 usage 的联动 ——————————————————————

// B 档注入的内容由 usage 决定：被读次数高的条目一定出现在注入里。
func TestHotTierInjectionUsesUsage(t *testing.T) {
	d := memDir(t)
	var b strings.Builder
	b.WriteString("# Memory Index\n")
	for i := 0; i < 12; i++ {
		b.WriteString("- f" + itoa(i) + ".md\n")
	}
	writeIndex(t, d, b.String())

	c := Config{}.withDefaults()
	for i := 0; i < 5; i++ { // f10 被读 5 次，远超其他
		if err := c.RecordRead(d, filepath.Join(d.Path, "f10.md")); err != nil {
			t.Fatal(err)
		}
	}

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inj.Text, "f10.md") {
		t.Fatalf("被读最多的条目必须出现在注入里：\n%s", inj.Text)
	}
	if strings.Contains(inj.Text, "f9.md") {
		t.Fatalf("Top-8 之外的条目不该出现：\n%s", inj.Text)
	}
}

// 开关关闭时 hot 档同样一次读盘都不发生（既有契约不能被新功能打开缺口）。
func TestHotTierRespectsDisabledSwitch(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n")
	off := false

	var reads int32
	reads = 0
	c := Config{Enabled: &off}.withDefaults()
	realRead := c.ReadFile
	c.ReadFile = func(p string) ([]byte, error) {
		reads++
		return realRead(p)
	}

	inj, err := c.BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if !inj.Empty() || reads != 0 {
		t.Fatalf("关闭时 Text 必须为空且不读盘（reads=%d）：%q", reads, inj.Text)
	}
}

// 条目装得下时说明段不能说"挑出的 8 条"：那会让模型去找 5 条不存在的东西。
func TestGuidanceDoesNotClaimPartialWhenNothingOmitted(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- a.md\n- b.md\n")

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inj.Text, "不是全部") {
		t.Fatalf("没有省略时不该声称是子集：\n%s", inj.Text)
	}
	if !strings.Contains(inj.Text, "前 200 行") {
		t.Fatalf("此时应按全量形态陈述限额：\n%s", inj.Text)
	}
}

// 真实索引行是 `- [x.md](x.md) — 摘要…`（含空格），面板的"是否已登记"必须认得出来。
//
// 反例（2026-11 修掉的那个）：只认无空格的纯文件名行 → 72 条真实记忆一条都认不出，
// 面板把整库显示成未登记。
func TestParseIndexEntriesRecognizesLinkedLines(t *testing.T) {
	body := "# Memory Index\n- [a.md](a.md) — 摘要一\n- `notes/b.md` — 摘要二\n- 只是散文，没有文件\n- c.md\n"
	got := strings.Join(parseIndexEntries(body), ",")
	if got != "a.md,notes/b.md,c.md" {
		t.Fatalf("parseIndexEntries = %q", got)
	}
}
