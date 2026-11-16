package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// 写后审计（P3）：每一次落到记忆目录的写入，在**写成功之后**检查三件事。
//
// # 为什么是"写后"，不是"写入前拦截"
//
// 官方形态里没有专用记忆工具——记忆就是普通 Markdown 文件，模型用 write_file /
// edit_file 直接写。要拦截就得在工具层加一道准入校验链，而那正是官方明确没有的东西
// （见 memory.go 顶部"本包明确不做"）。所以这里只做**事后审计**：
//
//   - 发现密钥 → **就地脱敏**（这是唯一会改用户内容的一档，理由见下）；
//   - 发现格式问题 → 回一句提示，不改文件（改人家写的 frontmatter 等于替它做决定）；
//   - 发现没登记进索引 → 回一句提示，不改索引（补索引是模型的活，不是宿主的）。
//
// # 为什么只有密钥敢动内容
//
// "无密钥落盘"是硬要求，而写后审计拿不到"阻止"这个动作——文件已经在盘上了。
// 唯一能满足要求的是**把密钥抹掉**。为了不误伤，模式只收高精度的几种
// （`sk-…`、`AKIA…`、`ghp_…`、`xox?-…`、PEM 头、JWT），且只替换匹配到的那一段，
// 前后文一字不动；脱敏后回一句"已脱敏"，让写的人知道内容被动过。
//
// # 提示语的自我约束
//
// 与 guidance.go 同一套：只陈述**已经发生的事实**，不写"你应该……"。
// 上一版自创的 6 条行为协议实测采纳率 12%~62%，而"你的文件没进索引"是一个
// 可验证的事实，不是劝告。

// Finding 是一条审计结论。Code 供测试与日志用，Message 是给模型/用户看的一句话。
type Finding struct {
	Code    string
	Message string
}

// AuditResult 是一次审计的全部产出。
type AuditResult struct {
	Findings []Finding
	// Redacted 表示文件内容被脱敏改写（唯一会改文件的一档）。
	Redacted bool
	// Notice 拼好的提示语（多条用换行连接；空串 = 什么都不说）。
	Notice string
}

// Empty 判断这次审计有没有话要说。
func (r AuditResult) Empty() bool { return r.Notice == "" }

// 高精度密钥模式。宁可漏，不可误伤——误伤会把用户正常写的代码片段抹掉，
// 而漏掉的密钥在"写下它的那一刻"还没有泄漏面（记忆目录是本机的）。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{8,}`),
}

// redactionMask 脱敏后的占位。保留前 3 个字符，便于事后认出"这里原来是什么形态"。
const redactionMask = "***REDACTED***"

// Audit 审计一条刚写进记忆目录的文件。
//
// 只对**记忆条目**（Dir.IsTopic）生效：索引、.state/、非 .md 一律跳过——它们不是
// 模型写给自己的事实，审计它们只会制造噪声。
//
// 任何读失败都按"这条写不关我事"处理（返回空结果 + err），不阻断写入链路：
// 审计是观测，写失败才是真失败。
func (c Config) Audit(d Dir, absPath string) (AuditResult, error) {
	var out AuditResult
	if !d.IsTopic(absPath) {
		return out, nil
	}
	c = c.withDefaults()
	raw, err := c.ReadFile(absPath)
	if err != nil {
		return out, err
	}
	body := string(raw)

	// ① 密钥：就地脱敏。
	if masked, hit := redactSecrets(body); hit {
		if err := c.atomicWrite(absPath, []byte(masked)); err != nil {
			return out, err
		}
		out.Redacted = true
		out.Findings = append(out.Findings, Finding{
			Code: "secret-redacted",
			Message: "已脱敏：这条记忆里检测到疑似密钥，已替换为 " + redactionMask +
				"（原文未保留在磁盘上）。",
		})
		body = masked
	}

	// ② frontmatter 的 type：只在**已有 frontmatter** 时检查。
	//
	// 官方第 ② 条 "Claude Code never adds frontmatter to a file that has none" ——
	// 无 frontmatter 的文件不是"格式错误"，是"不受时间戳机制管辖"的普通文件。
	// 把它报成错误会让首次开启审计时刷出一屏警告（实测 19 个历史文件）。
	if hasFrontmatter([]byte(body)) {
		if typ, ok := frontmatterValue(body, "type"); !ok || typ == "" {
			out.Findings = append(out.Findings, Finding{
				Code:    "type-missing",
				Message: "这条记忆的 frontmatter 没有 `type`（取值 user / feedback / project / reference 之一）。",
			})
		} else if !isMemoryType(typ) {
			out.Findings = append(out.Findings, Finding{
				Code:    "type-unknown",
				Message: fmt.Sprintf("frontmatter 的 `type: %s` 不在 user / feedback / project / reference 里。", typ),
			})
		}
	}

	// ③ 索引登记：没登记的记忆不会出现在启动注入里（这是级联提示，不是要求）。
	//
	// 只报"未登记"，不自动补——补索引是模型的活。也不报"索引里有但文件没了"
	// （那是陈旧条目，属于做梦的清理范围，在这里报只会让每次写都提一次旧账）。
	if listed, err := c.indexedName(d, absPath); err == nil && !listed {
		out.Findings = append(out.Findings, Finding{
			Code: "not-indexed",
			Message: fmt.Sprintf("`%s` 还没登记进 %s，启动注入里不会出现它。",
				baseName(absPath), IndexFileName),
		})
	}

	out.Notice = joinNotices(out.Findings)
	return out, nil
}

// indexedName 判断某文件是否以 basename 出现在索引里。
func (c Config) indexedName(d Dir, absPath string) (bool, error) {
	raw, err := c.ReadFile(joinIndex(d))
	if err != nil {
		return false, err
	}
	base := baseName(absPath)
	for _, name := range parseIndexEntries(string(raw)) {
		if strings.EqualFold(base, name) {
			return true, nil
		}
	}
	return false, nil
}

// redactSecrets 抹掉高精度密钥模式；hit 表示改过。
func redactSecrets(s string) (string, bool) {
	hit := false
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			hit = true
			head := m
			if len(head) > 3 {
				head = head[:3]
			}
			return head + redactionMask
		})
	}
	return s, hit
}

// frontmatterValue 取 frontmatter 里某个平铺字段的值（不做 YAML 解析，与 list.go 同口径）。
func frontmatterValue(body, key string) (string, bool) {
	for _, kv := range frontmatterFields(body) {
		if kv[0] == key {
			return kv[1], true
		}
	}
	return "", false
}

// isMemoryType 判断 type 是否是官方四种之一。
func isMemoryType(t string) bool {
	switch strings.TrimSpace(strings.ToLower(t)) {
	case "user", "feedback", "project", "reference":
		return true
	}
	return false
}

// joinNotices 把多条结论拼成一段提示。
//
// 上限两条：写一次记忆最多回两句，再多就成了噪声，而噪声会被无视——
// 那还不如只说最要紧的那一句。顺序按严重度：脱敏 > 格式 > 索引。
func joinNotices(fs []Finding) string {
	if len(fs) == 0 {
		return ""
	}
	n := len(fs)
	if n > 2 {
		n = 2
	}
	lines := make([]string, 0, n)
	for _, f := range fs[:n] {
		lines = append(lines, f.Message)
	}
	return strings.Join(lines, "\n")
}

// baseName 取文件名（与 filepath.Base 等价，但避免为一个调用点 import path/filepath）。
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
