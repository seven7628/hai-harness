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
