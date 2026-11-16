package main

// Dream 跨轮状态（dream_state.go）与任务书注入的测试。
//
// 这一组的重点不是"字段对不对"，而是**信任级**与**预算**：
//   - 状态里的归档事实只能来自宿主（参数 ∩ 返回），模型自述不算数；
//   - 任务书是有界的：第一轮不出现、超限先砍库再砍条目、整段 ≤4096 字节；
//   - 坏文件不能挡住巡检，但也不能被静默吞掉。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDreamStateRoundTrip(t *testing.T) {
	withDreamDir(t)
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)
	st := emptyDreamState()
	st.UpdatedAt = now
	st.LastNotes = "上一轮汇总"
	st.Repos["a"] = dreamRepoState{
		LastRunAt: now,
		Topics:    9,
		Runs:      2,
		Archived:  []dreamArchivedFact{{File: "x.md", Reason: "锚点失效", At: now}},
	}
	if err := saveDreamState(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	p := dreamStatePath()
	if p == "" {
		t.Fatal("状态路径为空")
	}
	if fi, err := os.Stat(p); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("状态文件应为 0600，实际 %v", fi.Mode().Perm())
	}
	got := loadDreamState()
	if got.Version != dreamStateVersion || got.LastNotes != st.LastNotes {
		t.Fatalf("往返丢字段: %+v", got)
	}
	rs := got.Repos["a"]
	if rs.Runs != 2 || rs.Topics != 9 || len(rs.Archived) != 1 || rs.Archived[0].File != "x.md" {
		t.Fatalf("库状态不对: %+v", rs)
	}
}

func TestDreamStateCorruptFileBackedUp(t *testing.T) {
	dir := withDreamDir(t)
	p := dreamStatePath()
	if err := os.WriteFile(p, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadDreamState()
	if len(got.Repos) != 0 {
		t.Fatalf("坏文件应回落到空状态，得到 %+v", got)
	}
	// "不能挡住巡检"与"不能吞掉旧文件"两条都要满足。
	if _, err := os.Stat(p + dreamStateBadSuffix); err != nil {
		t.Fatalf("坏文件应留档为 .bad: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dreamStateFile)); !os.IsNotExist(err) {
		t.Fatalf("原文件应已改名移走，err=%v", err)
	}
}

func TestDreamStateLegacyFileNormalized(t *testing.T) {
	withDreamDir(t)
	// 手写 / 老版本：没有 version、repos 为 null。
	if err := os.WriteFile(dreamStatePath(), []byte(`{"last_notes":"x","repos":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := loadDreamState()
	if st.Version != dreamStateVersion {
		t.Fatalf("缺 version 应补成 %d，得到 %d", dreamStateVersion, st.Version)
	}
	if st.Repos == nil {
		t.Fatal("Repos 应补成空 map（写入路径会直接往里塞）")
	}
	if st.LastNotes != "x" {
		t.Fatalf("老文件里的字段不该丢: %+v", st)
	}
}

func TestDreamStateNewerVersionTreatedEmpty(t *testing.T) {
	withDreamDir(t)
	if err := os.WriteFile(dreamStatePath(), []byte(`{"version":99,"repos":{"a":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadDreamState(); len(got.Repos) != 0 {
		t.Fatalf("未来版本应回落空状态，得到 %+v", got)
	}
}

func TestMergeDreamFactsDedupeNewestFirstAndCap(t *testing.T) {
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)
	prev := []dreamArchivedFact{
		{File: "old1.md", Reason: "旧理由1"},
		{File: "dup.md", Reason: "旧理由"},
	}
	var fresh []dreamArchivedFact
	for i := 0; i < 25; i++ {
		fresh = append(fresh, dreamArchivedFact{File: "n" + itoa(i) + ".md", Reason: "r"})
	}
	fresh = append(fresh, dreamArchivedFact{File: "dup.md", Reason: "新理由"}) // 归档→恢复→再归档

	got := mergeDreamFacts(prev, fresh, now)
	if len(got) != dreamStateFactsKeep {
		t.Fatalf("应裁到 %d 条，得到 %d", dreamStateFactsKeep, len(got))
	}
	if got[0].File != "dup.md" {
		t.Fatalf("最新的一条应在最前，得到 %+v", got[0])
	}
	if got[0].Reason != "新理由" {
		t.Fatalf("同一文件只留最新理由，得到 %q", got[0].Reason)
	}
	// 去重后不该再出现第二次。
	n := 0
	for _, f := range got {
		if f.File == "dup.md" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("dup.md 应只剩一条，得到 %d", n)
	}
	// fresh 倒序：最后追加的 n24 应排在 dup 之后。
	if got[1].File != "n24.md" {
		t.Fatalf("新事实应按发生顺序倒排，得到 %+v", got[1])
	}
}

func TestDreamPromptFirstRunHasNoPrior(t *testing.T) {
	if got := dreamPrompt(emptyDreamState()); got != dreamPromptBody {
		t.Fatalf("第一轮不该出现「上一轮判断」段:\n%s", got)
	}
	if strings.Contains(dreamPrompt(emptyDreamState()), "上一轮判断") {
		t.Fatal("空状态里出现了上一轮段落")
	}
}

func TestDreamPromptPriorSection(t *testing.T) {
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)
	st := emptyDreamState()
	st.UpdatedAt = now.Add(-24 * time.Hour)
	st.Repos["agent-server-g"] = dreamRepoState{
		LastRunAt: now.Add(-24 * time.Hour),
		Topics:    80,
		Runs:      3,
		Archived: []dreamArchivedFact{
			{File: "reactagent-traps.md", Reason: "10-17 的进行中快照，S2/S3/S4 已闭合", At: now},
		},
	}
	sec := dreamPromptPrior(st, now)
	// 留一份渲染结果在 -v 里：这段文字是模型每晚唯一会读到的"昨天"，改文案时
	// 先看这里，比读代码快。
	t.Logf("渲染出的上一轮段落：\n%s", sec)
	if !strings.HasPrefix(sec, "## 上一轮判断") {
		t.Fatalf("段落标题不对:\n%s", sec)
	}
	for _, want := range []string{"不是事实，可以推翻", "2026-11-15", "agent-server-g", "已巡检 3 轮", "80 条", "reactagent-traps.md", "S2/S3/S4 已闭合"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("缺 %q:\n%s", want, sec)
		}
	}
	// 第二轮的完整任务书里应能看到它，且仍以固定正文开头。
	full := dreamPrompt(st)
	if !strings.HasPrefix(full, dreamPromptBody) || !strings.Contains(full, "reactagent-traps.md") {
		t.Fatalf("第二轮任务书没接上上一轮段落:\n%s", full)
	}
}

func TestDreamPromptPriorBudget(t *testing.T) {
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)
	st := emptyDreamState()
	st.UpdatedAt = now
	long := strings.Repeat("理", 300) // 300 rune → 必须截到 120
	for i := 0; i < 20; i++ {        // 超过 12 个库的上限
		id := "repo-" + itoa(i)
		var facts []dreamArchivedFact
		for j := 0; j < 10; j++ { // 超过每库 6 条的上限
			facts = append(facts, dreamArchivedFact{File: id + "-f" + itoa(j) + ".md", Reason: long, At: now})
		}
		st.Repos[id] = dreamRepoState{LastRunAt: now.Add(-time.Duration(i) * time.Hour), Topics: 5, Runs: 1, Archived: facts}
	}
	sec := dreamPromptPrior(st, now)
	if sec == "" {
		t.Fatal("超预算时不该整段丢弃（先砍库再砍条目）")
	}
	if len(sec) > dreamPriorSectionBytes {
		t.Fatalf("整段应 ≤%d 字节，实际 %d", dreamPriorSectionBytes, len(sec))
	}
	if n := strings.Count(sec, "\n- "); n > dreamPriorReposMax {
		t.Fatalf("库数应 ≤%d，实际 %d", dreamPriorReposMax, n)
	}
	// 单条理由截断：段落里不该出现 300 个"理"的原文。
	if strings.Contains(sec, strings.Repeat("理", dreamPriorReasonRunes+1)) {
		t.Fatal("单条理由没有截到 120 rune")
	}
	// 最近的库排在前面：repo-0 的 LastRunAt 最新，必须是列表第一行。
	// （预算会砍库，"repo-19 在不在"不可断言——只钉"第一个是谁"。）
	if !strings.Contains(sec, "\n- repo-0（") {
		t.Fatalf("最近的库应排在第一行:\n%s", sec)
	}
	if i := strings.Index(sec, "\n- repo-1（"); i >= 0 && strings.Index(sec, "\n- repo-0（") > i {
		t.Fatalf("库应按最近巡检倒序:\n%s", sec)
	}
	// 每库最多 6 条：repo-0 的 f6..f9 不该出现。
	if strings.Contains(sec, "repo-0-f6.md") {
		t.Fatal("每库条目数没有截到 6")
	}
}

func TestDreamPromptPriorStaleRepo(t *testing.T) {
	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local)
	st := emptyDreamState()
	st.UpdatedAt = now
	st.Repos["old-repo"] = dreamRepoState{
		LastRunAt: now.Add(-120 * 24 * time.Hour), // 超过 90 天
		Topics:    9,
		Runs:      1,
		Archived:  []dreamArchivedFact{{File: "x.md", Reason: "半年前的依据", At: now}},
	}
	sec := dreamPromptPrior(st, now)
	if !strings.Contains(sec, "很久没巡检") {
		t.Fatalf("超 90 天的库应只留一行提醒:\n%s", sec)
	}
	if strings.Contains(sec, "x.md") {
		t.Fatalf("超 90 天的库不该列归档明细:\n%s", sec)
	}
}

func TestDreamTallyFactsOnlyConfirmedArchives(t *testing.T) {
	tl := newDreamTally()
	// 模型提了 3 条，宿主只归档了 1 条（另外 2 条被保护清单/冷却挡下）。
	tl.observe(dreamToolEvent("memory_dream",
		`{"action":"apply","repo":"a","decisions":"{\"decisions\":[{\"file\":\"x.md\",\"action\":\"archive\",\"reason\":\"锚点失效\"},{\"file\":\"y.md\",\"action\":\"archive\",\"reason\":\"疑似过时\"},{\"file\":\"z.md\",\"action\":\"keep\",\"reason\":\"还不确定\"}]}"}`,
		"记忆归档：归档 1 条：x.md；跳过 2 条（y.md 被保护清单点名；z.md 保留）；快照 .state/dream/1"))
	cap := tl.capture()
	facts := cap.Facts["a"]
	if len(facts) != 1 || facts[0].File != "x.md" {
		t.Fatalf("只有真的落盘的才进状态，得到 %+v", facts)
	}
	if facts[0].Reason != "锚点失效" {
		t.Fatalf("理由应取自 apply 参数，得到 %q", facts[0].Reason)
	}
	// 台账条数与事实条数同源但不同用途：运行记录看条数。
	var archived int
	for _, r := range cap.Repos {
		if r.ID == "a" {
			archived = r.Archived
		}
	}
	if archived != 1 {
		t.Fatalf("台账归档条数应为 1，得到 %d", archived)
	}
}

func TestDreamTallyFactsObjectDecisions(t *testing.T) {
	// decisions 直接给对象（真机见过两种形态）：也要能取到理由。
	tl := newDreamTally()
	tl.observe(dreamToolEvent("memory_dream",
		`{"action":"apply","repo":"b","decisions":{"decisions":[{"file":"q.md","action":"archive","reason":"被推翻"}]}}`,
		"记忆归档：归档 1 条：q.md；快照 s"))
	facts := tl.capture().Facts["b"]
	if len(facts) != 1 || facts[0].Reason != "被推翻" {
		t.Fatalf("对象形态的 decisions 没解析出来: %+v", facts)
	}
}

func TestUpdateDreamStateMergesAndKeepsUntouched(t *testing.T) {
	withDreamDir(t)
	now := time.Now()
	// 上一轮：两个库都有状态。
	prev := emptyDreamState()
	prev.Repos["a"] = dreamRepoState{LastRunAt: now.Add(-24 * time.Hour), Topics: 10, Runs: 1}
	prev.Repos["b"] = dreamRepoState{LastRunAt: now.Add(-24 * time.Hour), Topics: 20, Runs: 5}
	if err := saveDreamState(prev); err != nil {
		t.Fatal(err)
	}
	// 这一轮只巡检了 a。
	updateDreamState(
		[]dreamRepoResult{{ID: "a", Topics: 12, Archived: 1}},
		map[string][]dreamArchivedFact{"a": {{File: "x.md", Reason: "锚点失效", At: now}}},
		"本轮汇总",
	)
	st := loadDreamState()
	if st.Repos["a"].Runs != 2 || st.Repos["a"].Topics != 12 || len(st.Repos["a"].Archived) != 1 {
		t.Fatalf("a 应被更新: %+v", st.Repos["a"])
	}
	if st.Repos["b"].Runs != 5 || st.Repos["b"].Topics != 20 {
		t.Fatalf("没巡检的库不该被清掉: %+v", st.Repos["b"])
	}
	if st.LastNotes != "本轮汇总" {
		t.Fatalf("收尾正文应进 LastNotes，得到 %q", st.LastNotes)
	}
}

func TestUpdateDreamStateKeepsNotesOnEmptyFinal(t *testing.T) {
	withDreamDir(t)
	prev := emptyDreamState()
	prev.LastNotes = "上一轮的话"
	if err := saveDreamState(prev); err != nil {
		t.Fatal(err)
	}
	// 这一轮出错了：没有收尾正文，不能把上一轮的结论抹成空。
	updateDreamState([]dreamRepoResult{{ID: "a"}}, nil, "   ")
	if got := loadDreamState().LastNotes; got != "上一轮的话" {
		t.Fatalf("空正文不该覆盖上一轮 notes，得到 %q", got)
	}
}
