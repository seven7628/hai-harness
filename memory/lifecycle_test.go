package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P5 生命周期（模型裁决 + 宿主执行）的回归测试。
//
// 分工：判断由模型做（不在测试范围）；这里只钉**执行的安全性**——
// 快照、日志、索引、护栏、可逆性。

func lifeFixture(t *testing.T) (Dir, time.Time) {
	t.Helper()
	root := t.TempDir()
	d := Dir{ID: "test-id", Path: filepath.Join(root, "repos", "test-id")}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	writeIndex(t, d, "# Memory Index\n- [old.md](old.md) — 过时的\n- [keep.md](keep.md) — 有效的\n")
	writeTopicFile(t, d, "old.md", "---\ntype: project\nmodified: 2026-01-01T00:00:00Z\n---\n\n# 旧结论\n\n内容\n")
	writeTopicFile(t, d, "keep.md", "---\ntype: project\nmodified: 2026-01-01T00:00:00Z\n---\n\n# 有效结论\n\n内容\n")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	// 把 mtime 拨到 30 天前：否则会被"最近 7 天内动过"这条护栏挡下（真实链路里
	// 记忆文件的 mtime 就是最后写入时间，这个设置在测护栏二时还要再调一次）。
	old := now.Add(-30 * 24 * time.Hour)
	for _, n := range []string{"old.md", "keep.md"} {
		if err := os.Chtimes(filepath.Join(d.Path, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return d, now
}

func TestApplyDecisionsArchivesReversibly(t *testing.T) {
	d, now := lifeFixture(t)
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}

	res, err := cfg.ApplyDecisions(d, []Decision{
		{File: "old.md", Action: ActionArchive, Reason: "锚点全死"},
		{File: "keep.md", Action: ActionKeep},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "old.md" {
		t.Fatalf("归档结果 = %+v", res)
	}

	// ① 文件已被移出记忆目录，且落在归档区（不是删除）。
	if _, err := os.Stat(filepath.Join(d.Path, "old.md")); !os.IsNotExist(err) {
		t.Fatal("old.md 应已移出记忆目录")
	}
	arch := cfg.archiveDir(d)
	if _, err := os.Stat(filepath.Join(arch, "old.md")); err != nil {
		t.Fatalf("归档区应有文件：%v", err)
	}
	// ② 原地在，说明没被误删。
	if _, err := os.Stat(filepath.Join(d.Path, "keep.md")); err != nil {
		t.Fatal("keep.md 不该被动")
	}

	// ③ 快照里有原文件与索引。
	if _, err := os.Stat(filepath.Join(res.Snapshot, "old.md")); err != nil {
		t.Fatalf("快照缺原文件：%v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Snapshot, IndexFileName)); err != nil {
		t.Fatalf("快照缺索引：%v", err)
	}

	// ④ 索引里那一行被摘掉，其它行不动。
	idx, _ := os.ReadFile(joinIndex(d))
	if strings.Contains(string(idx), "old.md") {
		t.Fatalf("索引里不该再有 old.md：\n%s", idx)
	}
	if !strings.Contains(string(idx), "keep.md") {
		t.Fatalf("其它行不该被动：\n%s", idx)
	}

	// ⑤ 日志有据可查。
	logRaw, err := os.ReadFile(filepath.Join(d.Path, stateDirName, "dream.log"))
	if err != nil {
		t.Fatalf("应写日志：%v", err)
	}
	if !strings.Contains(string(logRaw), "archive old.md") || !strings.Contains(string(logRaw), "锚点全死") {
		t.Fatalf("日志内容不对：\n%s", logRaw)
	}
}

// 护栏一：最近 7 天内动过的文件不碰（模型说了也不算）。
func TestApplyRefusesFreshFiles(t *testing.T) {
	d, now := lifeFixture(t)
	// 把 old.md 的修改时间改成 1 天前。
	recent := now.Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(d.Path, "old.md"), recent, recent); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	res, err := cfg.ApplyDecisions(d, []Decision{{File: "old.md", Action: ActionArchive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("新改动的文件不该被归档：%+v", res)
	}
	if !strings.Contains(strings.Join(res.Skipped, ";"), "最近 7 天内动过") {
		t.Fatalf("应说明被护栏挡下：%v", res.Skipped)
	}
}

// 护栏二：单次最多归档 20%。
func TestApplyCapsArchiveRatio(t *testing.T) {
	d, now := lifeFixture(t)
	// 再加 8 个条目，凑 10 个 → 上限 2。
	var lines []string
	for i := 0; i < 8; i++ {
		n := "f" + itoa(i) + ".md"
		writeTopicFile(t, d, n, "---\ntype: project\n---\n\n# t\n\nx\n")
		lines = append(lines, "- "+n)
	}
	writeIndex(t, d, "# Memory Index\n- [old.md](old.md) — x\n- [keep.md](keep.md) — y\n"+strings.Join(lines, "\n")+"\n")
	// 把所有文件的 mtime 拨老，免得被"7 天内"挡掉。
	old := now.Add(-30 * 24 * time.Hour)
	entries, _ := os.ReadDir(d.Path)
	for _, e := range entries {
		_ = os.Chtimes(filepath.Join(d.Path, e.Name()), old, old)
	}

	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	var decs []Decision
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && e.Name() != IndexFileName {
			decs = append(decs, Decision{File: e.Name(), Action: ActionArchive})
		}
	}
	res, err := cfg.ApplyDecisions(d, decs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) > 2 {
		t.Fatalf("超过 20%% 上限：归档 %d / 共 %d", len(res.Archived), len(decs))
	}
}

// 解析宽容：对象包裹、裸数组、带解释的文字都认。
func TestParseDecisionsTolerant(t *testing.T) {
	cases := []string{
		`{"decisions":[{"file":"a","action":"archive"}]}`,
		`[{"file":"a.md","action":"archive","reason":"x"}]`,
		"好，裁决如下：\n```json\n{\"decisions\":[{\"file\":\"a.md\",\"action\":\"archive\"}]}\n```\n完毕",
	}
	for _, c := range cases {
		got, err := ParseDecisions([]byte(c))
		if err != nil || len(got) != 1 || got[0].File != "a.md" || got[0].Action != ActionArchive {
			t.Fatalf("解析 %q → %+v %v", c, got, err)
		}
	}
	// 未知动作按 keep（宁可不做）。
	got, _ := ParseDecisions([]byte(`{"decisions":[{"file":"a.md","action":"delete"}]}`))
	if got[0].Action != ActionKeep {
		t.Fatalf("未知动作应降级为 keep：%+v", got)
	}
}

// 恢复：文件和索引行都要回到原位，这是"只归档不删除"承诺的另一半。
func TestRestorePutsFileAndIndexLineBack(t *testing.T) {
	d, now := lifeFixture(t)
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}

	res, err := cfg.ApplyDecisions(d, []Decision{{File: "old.md", Action: ActionArchive, Reason: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 {
		t.Fatalf("先要归档成功：%+v", res)
	}

	got, err := cfg.Restore(d, res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "old.md" {
		t.Fatalf("恢复结果 = %v", got)
	}
	if _, err := os.Stat(filepath.Join(d.Path, "old.md")); err != nil {
		t.Fatalf("文件应回到记忆目录：%v", err)
	}
	idx, _ := os.ReadFile(joinIndex(d))
	if !strings.Contains(string(idx), "old.md") {
		t.Fatalf("索引行应插回：\n%s", idx)
	}
	// 原来在 keep.md 之前，恢复后仍应在它之前。
	si, ki := strings.Index(string(idx), "old.md"), strings.Index(string(idx), "keep.md")
	if si < 0 || ki < 0 || si > ki {
		t.Fatalf("索引顺序变了：\n%s", idx)
	}
	// 日志要记这次恢复。
	logRaw, _ := os.ReadFile(filepath.Join(d.Path, stateDirName, "dream.log"))
	if !strings.Contains(string(logRaw), "restore old.md") {
		t.Fatalf("日志应记恢复：\n%s", logRaw)
	}
}

// 评测点名的条目不许归档：它们是"记忆还有没有用"的仪表。
func TestApplySkipsProtectedFiles(t *testing.T) {
	d, now := lifeFixture(t)
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	if err := cfg.WriteProtected(d, []string{"old.md"}); err != nil {
		t.Fatal(err)
	}
	res, err := cfg.ApplyDecisions(d, []Decision{{File: "old.md", Action: ActionArchive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 || !strings.Contains(strings.Join(res.Skipped, ";"), "保护清单") {
		t.Fatalf("应被保护清单挡下：%+v", res)
	}
}

// 小库的归档额度是 2 条（比例额度的下限）：一次挪一小步，坏了能归因。
// 大库按比例放宽、由 dreamMaxArchiveCap=5 封顶，见 TestApplyArchiveCapScalesWithRepoSize。
func TestApplyCapsAtTwoPerRun(t *testing.T) {
	d, now := lifeFixture(t)
	var lines []string
	for i := 0; i < 10; i++ {
		n := "x" + itoa(i) + ".md"
		writeTopicFile(t, d, n, "---\ntype: project\n---\n\n# t\n\nx\n")
		lines = append(lines, "- "+n)
	}
	writeIndex(t, d, "# Memory Index\n- [old.md](old.md) — x\n- [keep.md](keep.md) — y\n"+strings.Join(lines, "\n")+"\n")
	old := now.Add(-30 * 24 * time.Hour)
	entries, _ := os.ReadDir(d.Path)
	for _, e := range entries {
		_ = os.Chtimes(filepath.Join(d.Path, e.Name()), old, old)
	}
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	var decs []Decision
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && e.Name() != IndexFileName {
			decs = append(decs, Decision{File: e.Name(), Action: ActionArchive})
		}
	}
	res, err := cfg.ApplyDecisions(d, decs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) > 2 {
		t.Fatalf("小库单次归档额度 2 条，实际 %d", len(res.Archived))
	}
}

// 大库的额度按库大小放大，但硬上限是 5 条。
//
// 这条断言把「上限不是 2」钉在代码里：工具文案一度写着"单次最多 2 条"，
// 模型照着自我克制，大库（80+ 条）一天只挪 1-2 条，上限改成 max(2, 比例)≤5
// 等于没改（2026-11 真机观察）。库里刻意放 400 条：无论比例常数是 2% 还是 20%，
// 算出来都在封顶之上，所以它测的是"封顶 5 条"这件事本身。
func TestApplyArchiveCapScalesWithRepoSize(t *testing.T) {
	d, now := lifeFixture(t)
	stale := now.Add(-30 * 24 * time.Hour)
	var lines []string
	for i := 0; i < 400; i++ {
		n := "big" + itoa(i) + ".md"
		writeTopicFile(t, d, n, "---\ntype: project\n---\n\n# t\n\nx\n")
		if err := os.Chtimes(filepath.Join(d.Path, n), stale, stale); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "- "+n)
	}
	writeIndex(t, d, "# Memory Index\n"+strings.Join(lines, "\n")+"\n")
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	var decs []Decision
	for i := 0; i < 400; i++ {
		decs = append(decs, Decision{File: "big" + itoa(i) + ".md", Action: ActionArchive})
	}
	res, err := cfg.ApplyDecisions(d, decs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != dreamMaxArchiveCap {
		t.Fatalf("400 条的大库单次应归档 %d 条（比例额度被硬上限压回），实际 %d", dreamMaxArchiveCap, len(res.Archived))
	}
	if dreamMaxArchiveCap <= dreamMaxArchivePerRun {
		t.Fatalf("硬上限 %d 必须大于小库额度 %d，否则分档没有意义", dreamMaxArchiveCap, dreamMaxArchivePerRun)
	}
}

// 比例常数本身要钉住：2% 还是 20% 在"封顶 5 条"那个用例里看不出区别。
//
// 20 条的库：2% → 0.4 → 保底 2 条；20% → 4 条。一次多挪一倍是产品口径的偏差，
// 而不是实现细节——2026-11 真机发现常量写成 0.2、注释写 2%，两者对不上。
func TestApplyArchiveCapUsesTwoPercent(t *testing.T) {
	d, now := lifeFixture(t)
	stale := now.Add(-30 * 24 * time.Hour)
	var lines []string
	for i := 0; i < 20; i++ {
		n := "mid" + itoa(i) + ".md"
		writeTopicFile(t, d, n, "---\ntype: project\n---\n\n# t\n\nx\n")
		if err := os.Chtimes(filepath.Join(d.Path, n), stale, stale); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "- "+n)
	}
	writeIndex(t, d, "# Memory Index\n"+strings.Join(lines, "\n")+"\n")
	cfg := Config{Now: func() time.Time { return now }, ReadDir: os.ReadDir}
	var decs []Decision
	for i := 0; i < 20; i++ {
		decs = append(decs, Decision{File: "mid" + itoa(i) + ".md", Action: ActionArchive})
	}
	res, err := cfg.ApplyDecisions(d, decs)
	if err != nil {
		t.Fatal(err)
	}
	// 20 * 2% = 0.4，取整为 0 → 保底 dreamMaxArchivePerRun。若比例被写成 20%，
	// 这里是 4 条，断言会失败并指出"额度算法按 20% 在走"。
	if want := dreamMaxArchivePerRun; len(res.Archived) != want {
		t.Fatalf("20 条的库单次应归档 %d 条（2%%），实际 %d —— 比例常数是不是又变成 0.2 了？", want, len(res.Archived))
	}
}
