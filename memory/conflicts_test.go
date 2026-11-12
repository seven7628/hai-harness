package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Dream v3 冲突层：宿主只捞候选（锚点共现 ≥2、且是写入之后的发言），判断交给模型。

func conflictEvents(t *testing.T, lines []string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "ws-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s-1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConflictCandidatesRequireTwoAnchors(t *testing.T) {
	d := memDir(t)
	// 一条记忆带两个长锚点；另一条只有一个。
	writeTopicFile(t, d, "two.md", "---\ntype: project\nmodified: 2026-10-01T00:00:00Z\n---\n\n"+
		"`RegisterS3Atomically` 与 `user_id_filter` 都在这里。\n")
	writeTopicFile(t, d, "one.md", "---\ntype: project\nmodified: 2026-10-01T00:00:00Z\n---\n\n"+
		"只提到 `RegisterS3Atomically`。\n")

	both := `{"event_type":"user_inputs_consumed","user_texts":["RegisterS3Atomically 和 user_id_filter 都要改"],"timestamp":"2026-10-05T00:00:00Z"}`
	single := `{"event_type":"user_inputs_consumed","user_texts":["RegisterS3Atomically 这个再确认下"],"timestamp":"2026-10-06T00:00:00Z"}`
	before := `{"event_type":"user_inputs_consumed","user_texts":["RegisterS3Atomically user_id_filter 早就存在"],"timestamp":"2026-09-01T00:00:00Z"}`
	root := conflictEvents(t, []string{both, single, before})

	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	cands, err := (Config{EventsRoot: root, Now: func() time.Time { return now }}).ConflictCandidates(d, 180*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].File != "two.md" {
		t.Fatalf("应只有 two.md 有候选（≥2 锚点且写入之后）：%+v", cands)
	}
	// 写入之前的发言不能作为反证。
	for _, s := range cands[0].Snippets {
		if strings.Contains(s, "早就存在") {
			t.Fatalf("写入之前的片段不该出现：%v", cands[0].Snippets)
		}
	}
}

func TestConflictVerdictsRoundTrip(t *testing.T) {
	d := memDir(t)
	cfg := Config{Now: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := cfg.SaveConflicts(d, []ConflictVerdict{
		{File: "a.md", Verdict: ConflictContradicted, Reason: "用户说改成 X"},
	}); err != nil {
		t.Fatal(err)
	}
	got := cfg.ReadConflicts(d)
	if got["a.md"].Verdict != ConflictContradicted || got["a.md"].CheckedAt == "" {
		t.Fatalf("读回不对：%+v", got)
	}
	// 覆盖同一文件，不产生第二条。
	if err := cfg.SaveConflicts(d, []ConflictVerdict{{File: "a.md", Verdict: ConflictUpheld}}); err != nil {
		t.Fatal(err)
	}
	got = cfg.ReadConflicts(d)
	if len(got) != 1 || got["a.md"].Verdict != ConflictUpheld {
		t.Fatalf("覆盖失败：%+v", got)
	}
}

// 被推翻的条目要在报告里单独露面（那是唯一会改变"记忆真假"的证据）。
func TestDreamReportsContradictions(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- [a.md](a.md) — x\n")
	writeTopicFile(t, d, "a.md", "---\ntype: project\nmodified: 2026-10-01T00:00:00Z\n---\n\nx\n")
	cfg := Config{EventsRoot: t.TempDir(), RepoFiles: func(string) ([]string, error) { return nil, os.ErrNotExist }}
	if err := cfg.SaveConflicts(d, []ConflictVerdict{{File: "a.md", Verdict: ConflictContradicted, Reason: "改成了 Y"}}); err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Dream(d)
	if err != nil {
		t.Fatal(err)
	}
	md := r.Markdown(timeZero())
	if !strings.Contains(md, "被推翻") || !strings.Contains(md, "改成了 Y") {
		t.Fatalf("报告应列出被推翻的条目与理由：\n%s", md)
	}
}
