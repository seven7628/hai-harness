package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写后审计（P3）的回归测试。
//
// 分工：本文件只测**审计判什么、回什么话、改不改内容**；
// "钩子有没有被接上"由 desktop/bridge 的接线测试负责（本包不 import 宿主）。

func writeTopicFile(t *testing.T, d Dir, name, body string) string {
	t.Helper()
	p := filepath.Join(d.Path, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// —————————————————————— 密钥：唯一会改内容的一档 ——————————————————————

// 检测到密钥必须**真的抹掉**，而不只是回一句警告——"无密钥落盘"是硬要求，
// 而写后审计拿不到"阻止"这个动作，只有就地脱敏能满足它。
func TestAuditRedactsSecretInPlace(t *testing.T) {
	d := memDir(t)
	const secret = "sk-live-abcdefghijklmnopqrstuvwxyz012345"
	p := writeTopicFile(t, d, "leaky.md", "---\ntype: project\n---\n\nkey 是 "+secret+" 别外传\n")

	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !ar.Redacted {
		t.Fatal("应报告 Redacted")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("密钥仍在磁盘上：\n%s", raw)
	}
	if !strings.Contains(string(raw), redactionMask) {
		t.Fatalf("应留下脱敏占位：\n%s", raw)
	}
	// 前后文一字不动。
	if !strings.Contains(string(raw), "key 是 ") || !strings.Contains(string(raw), "别外传") {
		t.Fatalf("脱敏不该改动其余内容：\n%s", raw)
	}
	if !strings.Contains(ar.Notice, "已脱敏") {
		t.Fatalf("提示语应说明已脱敏：%q", ar.Notice)
	}
}

// 高精度模式：常见 token 形态都要覆盖。
func TestAuditRedactsKnownTokenShapes(t *testing.T) {
	cases := []string{
		"sk-test-0123456789abcdefghij",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_0123456789abcdefghijklmnopqrstuvwx",
		"xoxb-1234567890-abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	}
	for _, tok := range cases {
		got, hit := redactSecrets("a " + tok + " b")
		if !hit {
			t.Fatalf("未识别：%q", tok)
		}
		if strings.Contains(got, tok) {
			t.Fatalf("未抹掉：%q → %q", tok, got)
		}
	}
}

// 反例：正常记忆里满是代码标识符与哈希，不能被误伤。
func TestAuditDoesNotRedactOrdinaryText(t *testing.T) {
	d := memDir(t)
	body := "---\ntype: project\n---\n\n`RegisterS3` 调用了 `user_id`，索引 56804d4d，hash 0cd9bf5d。\n"
	p := writeTopicFile(t, d, "normal.md", body)

	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Redacted {
		t.Fatalf("误伤：%+v", ar.Findings)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != body {
		t.Fatal("没有密钥时不该改写文件")
	}
}

// —————————————————————— 格式：只提醒，不改 ——————————————————————

// 有 frontmatter 但缺 type → 提示，且**不改文件**。
func TestAuditWarnsMissingTypeWithoutRewriting(t *testing.T) {
	d := memDir(t)
	body := "---\nmodified: 2026-11-01T00:00:00+08:00\n---\n\n一条没有 type 的记忆\n"
	p := writeTopicFile(t, d, "notype.md", body)

	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ar.Notice, "没有 `type`") {
		t.Fatalf("应提示缺 type：%q", ar.Notice)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != body {
		t.Fatal("格式问题不该改写文件")
	}
}

// type 取值不在官方四种里 → 提示。
func TestAuditWarnsUnknownType(t *testing.T) {
	d := memDir(t)
	p := writeTopicFile(t, d, "weird.md", "---\ntype: shopping-list\n---\n\nx\n")
	ar, _ := (Config{}).Audit(d, p)
	if !strings.Contains(ar.Notice, "shopping-list") {
		t.Fatalf("应指出非法取值：%q", ar.Notice)
	}
}

// D5：没有 frontmatter 的文件不是"格式错误"——官方从不为无 frontmatter 的文件加字段。
//
// 反例：把这类报成错误，首次开启审计会一次刷出十几个历史文件的警告。
func TestAuditDoesNotFlagFrontmatterlessFile(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- plain.md\n")
	p := writeTopicFile(t, d, "plain.md", "这是一段没有 frontmatter 的普通笔记\n")
	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !ar.Empty() {
		t.Fatalf("无 frontmatter 的文件不该产生任何提示：%q", ar.Notice)
	}
}

// —————————————————————— 索引登记：级联提示 ——————————————————————

func TestAuditNoticesUnregisteredFile(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- other.md\n")
	p := writeTopicFile(t, d, "new.md", "---\ntype: feedback\n---\n\nx\n")

	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ar.Notice, "还没登记") {
		t.Fatalf("未登记应提示：%q", ar.Notice)
	}
	// 只提示，不替模型补索引。
	raw, _ := os.ReadFile(joinIndex(d))
	if strings.Contains(string(raw), "new.md") {
		t.Fatal("审计不该自动改索引")
	}
}

func TestAuditSilentWhenRegistered(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- [ok.md](ok.md) — 说明\n")
	p := writeTopicFile(t, d, "ok.md", "---\ntype: project\n---\n\nx\n")
	ar, err := (Config{}).Audit(d, p)
	if err != nil {
		t.Fatal(err)
	}
	if !ar.Empty() {
		t.Fatalf("登记过、格式也对的记忆不该有提示：%q", ar.Notice)
	}
}

// 索引里写成子目录路径时也要认得出（parseIndexEntries 支持 markdown 链接行）。
func TestAuditRecognizesLinkedIndexEntry(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- [ok.md](ok.md) — 摘要…\n")
	p := writeTopicFile(t, d, "ok.md", "---\ntype: user\n---\n\nx\n")
	ar, _ := (Config{}).Audit(d, p)
	if !ar.Empty() {
		t.Fatalf("链接形式的索引行应被认作已登记：%q", ar.Notice)
	}
}

// —————————————————————— 边界 ——————————————————————

// 索引与 .state 不是记忆条目，审计不碰。
func TestAuditSkipsNonTopic(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n")
	ar, err := (Config{}).Audit(d, joinIndex(d))
	if err != nil || !ar.Empty() {
		t.Fatalf("索引不该被审计：%v %q", err, ar.Notice)
	}
}

// 提示最多两条：写一次记忆最多回两句话，再多就成了会被无视的噪声。
func TestAuditNoticeCappedAtTwoLines(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Memory Index\n- other.md\n")
	p := writeTopicFile(t, d, "bad.md", "---\ntype: nope\n---\n\nkey sk-live-abcdefghijklmnopqrstuvwxyz012345\n")
	ar, _ := (Config{}).Audit(d, p)
	if n := strings.Count(ar.Notice, "\n") + 1; n > 2 {
		t.Fatalf("提示 %d 行，超过上限：%q", n, ar.Notice)
	}
	// 排序：脱敏在最前。
	if !strings.HasPrefix(ar.Notice, "已脱敏") {
		t.Fatalf("最要紧的一条应排最前：%q", ar.Notice)
	}
}
