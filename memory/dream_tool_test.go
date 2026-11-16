package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 证据必须是"能直接读的 JSON"，且把判断依据（锚点/转录/冲突）都带上。
func TestDreamEvidenceIsReadableJSON(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	if err := os.WriteFile(filepath.Join(d.Path, "a.md"), []byte("---\ntype: project\n---\n见 `nope.go`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	cfg.EventsRoot = t.TempDir()

	raw, err := cfg.DreamEvidence(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("证据不是合法 JSON：%v", err)
	}
	for _, k := range []string{"topics", "anchors", "transcripts", "conflicts", "how_to_decide"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("证据缺少 %q", k)
		}
	}
	if !strings.Contains(string(raw), "keep|archive") {
		t.Fatal("证据里应写清动作取值域")
	}
	// 归档额度是**按库大小给的**（max(2, 2%) 且 ≤5，见 lifecycle.go），工具文案里
	// 不许再写成硬性「最多 2 条」：模型会照着自我克制，大库就永远只挪 1-2 条，
	// 上限改了等于没改（2026-11 真机观察）。
	how, _ := m["how_to_decide"].(string)
	if strings.Contains(how, "最多 2 条") || strings.Contains(how, "最多两条") {
		t.Fatalf("how_to_decide 把归档上限写死成 2 条（应按库大小给额度）：%s", how)
	}
	for _, want := range []string{"额度", "大库最多 5 条", "裁掉"} {
		if !strings.Contains(how, want) {
			t.Fatalf("how_to_decide 缺少额度说明 %q：%s", want, how)
		}
	}
}

// 只有 keep 时不碰任何文件——"没决定要动"与"决定不动"在磁盘上是同一件事。
func TestApplyDreamDecisionsKeepOnlyTouchesNothing(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	p := filepath.Join(d.Path, "a.md")
	if err := os.WriteFile(p, []byte("---\ntype: project\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}

	res, err := cfg.ApplyDreamDecisions(d, []byte(`{"decisions":[{"file":"a.md","action":"keep","reason":"还有用"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 0 {
		t.Fatalf("不该归档：%+v", res)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("keep 不该动文件")
	}
}

// archive 走真实护栏：文件移入归档区、索引摘行、留快照，且可恢复。
func TestApplyDreamDecisionsArchivesWithGuards(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	old := time.Now().Add(-30 * 24 * time.Hour)
	p := filepath.Join(d.Path, "a.md")
	if err := os.WriteFile(p, []byte("---\ntype: project\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path, "b.md"), []byte("---\ntype: project\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(d.Path, "b.md"), old, old); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}

	res, err := cfg.ApplyDreamDecisions(d, []byte(`{"decisions":[{"file":"a.md","action":"archive","reason":"锚点全失效"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "a.md" {
		t.Fatalf("应归档 a.md：%+v", res)
	}
	if res.Snapshot == "" {
		t.Fatal("归档必须留快照")
	}
	idx, _ := os.ReadFile(filepath.Join(d.Path, IndexFileName))
	if strings.Contains(string(idx), "a.md") {
		t.Fatalf("索引行应被摘掉：\n%s", idx)
	}
	restored, err := cfg.Restore(d, res.Snapshot)
	if err != nil || len(restored) != 1 {
		t.Fatalf("恢复失败：%v %v", restored, err)
	}
	idx, _ = os.ReadFile(filepath.Join(d.Path, IndexFileName))
	if !strings.Contains(string(idx), "a.md") {
		t.Fatalf("恢复后索引行应回来：\n%s", idx)
	}
}
