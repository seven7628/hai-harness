package memory

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxRepoIDLen 仓库 ID（目录名）上限。
	maxRepoIDLen = 64
	// maxTopicNameLen 主题名上限。
	maxTopicNameLen = 64
	// maxBodyBytes 主题正文上限（16KB）。
	//
	// 边界：主题文件是"人可读记忆"而不是数据库；超大内容应该落到会话产物
	// （artifacts）再在记忆里留指针。16KB 与索引 25KB 同量级，保证一次
	// 索引+正文注入仍在几万 token 内。
	maxBodyBytes = 16 * 1024
	// maxTagLen 单个标签上限。
	maxTagLen = 32
	// maxTags 单个主题的标签数上限。
	maxTags = 8
	// maxTitleLen 标题上限。
	maxTitleLen = 120
	// maxSummaryLen 索引摘要行上限。
	maxSummaryLen = 200
)

// 时间戳格式（frontmatter + 索引生成时间共用）。
//
// 边界：固定 UTC RFC3339 秒精度——不带时区/纳秒才能保证渲染字节稳定
// （同一时间写入两次必须得到完全相同的字节）。
const timeLayout = "2006-01-02T15:04:05Z"

// topicNameRe 合法主题名：小写字母数字 + 单个连字符分隔。
//
// 为什么不用 `[a-zA-Z0-9_-]+`：主题名会**直接变成文件名**（topics/<name>.md），
// 大写/下划线会让 "Foo" 与 "foo" 指向不同文件却指同一主题（大小写不敏感文件系统上
// 更会互相覆盖）。统一小写连字符是唯一不会分叉的规则。
var topicNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// topicSeparatorsRe 宽松输入里要折叠成连字符的分隔符（空白/下划线/点）。
var topicSeparatorsRe = regexp.MustCompile(`[\s._]+`)

// invalidTopicCharRe 折叠分隔符后仍需剔除的字符；不能用合法 slug 正则做
// ReplaceAllString("")，那会把所有合法字符一起删掉。
var invalidTopicCharRe = regexp.MustCompile(`[^a-z0-9-]`)

// validateRepoID 校验仓库 ID 可安全用作单级目录名。
func validateRepoID(id string) error {
	if id == "" {
		return fmt.Errorf("repo id is empty")
	}
	if len(id) > maxRepoIDLen {
		return fmt.Errorf("repo id %q too long (%d > %d)", id, len(id), maxRepoIDLen)
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("repo id %q must be a single path segment", id)
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("repo id %q has unsupported character %q", id, r)
		}
	}
	return nil
}

// validateTopicName 校验并规范化主题名。
//
// 输入接受宽松写法（大小写、空格、下划线、"topic:" 前缀、".md" 后缀），
// 输出恒为规范形（小写连字符），这样 `WriteTopic("My Topic")` 与
// `WriteTopic("my-topic")` 落到同一个文件。
func validateTopicName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "topic:")
	n = strings.TrimPrefix(n, "topics/")
	n = strings.TrimSuffix(n, ".md")
	n = strings.TrimSpace(n)
	if n == "" {
		return "", fmt.Errorf("topic name is empty")
	}
	if len(n) > maxTopicNameLen {
		return "", fmt.Errorf("topic name %q too long (%d > %d)", name, len(n), maxTopicNameLen)
	}
	// 空白/下划线/点 → 连字符（"My Topic" → "my-topic"），再剔除其余非法字符，
	// 折叠重复连字符、去首尾连字符。
	n = topicSeparatorsRe.ReplaceAllString(n, "-")
	n = invalidTopicCharRe.ReplaceAllString(n, "") // 剩余非 [a-z0-9-] 字符直接删
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	n = strings.Trim(n, "-")
	if n == "" {
		return "", fmt.Errorf("topic name %q has no usable characters", name)
	}
	if !topicNameRe.MatchString(n) {
		return "", fmt.Errorf("topic name %q is not a valid slug (%q)", name, topicNameRe.String())
	}
	return n, nil
}

// ValidateScope 校验作用域取值（空 = ScopeRepo）。
func ValidateScope(s Scope) (Scope, error) {
	if s == "" {
		return ScopeRepo, nil
	}
	if !validScopes[s] {
		return "", fmt.Errorf("unknown scope %q (want one of repo/file/task)", string(s))
	}
	return s, nil
}

// ValidateTopic 校验并规范化主题：名称、作用域、标签、正文大小。
//
// 顺序即成本序：先做廉价且最容易失败的检查（名字），secret 扫描最贵放最后。
func ValidateTopic(t *Topic) (*Topic, error) {
	if t == nil {
		return nil, fmt.Errorf("topic is nil")
	}
	name, err := validateTopicName(t.Name)
	if err != nil {
		return nil, err
	}
	scope, err := ValidateScope(t.Scope)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(t.Kind))
	if kind == "" {
		kind = "project"
	}
	if !validKinds[kind] {
		return nil, fmt.Errorf("unknown topic kind %q", t.Kind)
	}
	tags, err := validateTags(t.Tags)
	if err != nil {
		return nil, err
	}
	body := normalizeBody(t.Body)
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("topic body too large (%d > %d bytes)", len(body), maxBodyBytes)
	}
	if t.Title != "" && !utf8.ValidString(t.Title) {
		return nil, fmt.Errorf("topic title is not valid UTF-8")
	}
	if len([]rune(t.Title)) > maxTitleLen {
		return nil, fmt.Errorf("topic title too long (%d > %d runes)", utf8.RuneCountInString(t.Title), maxTitleLen)
	}
	if len([]rune(t.Summary)) > maxSummaryLen {
		return nil, fmt.Errorf("topic summary too long (%d > %d runes)", utf8.RuneCountInString(t.Summary), maxSummaryLen)
	}
	if err := ScanSecrets(body); err != nil {
		return nil, err
	}
	if err := ScanSecrets(t.Title + "\n" + t.Summary); err != nil {
		return nil, err
	}
	return &Topic{
		Name:    name,
		Title:   t.Title,
		Summary: t.Summary,
		Kind:    kind,
		Scope:   scope,
		Tags:    tags,
		Body:    body,
		Created: t.Created.UTC().Truncate(time.Second),
		Updated: t.Updated.UTC().Truncate(time.Second),
	}, nil
}

// validateTags 规范化标签：小写、去空、排序去重、限长限量。
//
// 排序去重是**字节稳定**的一部分：否则同批标签的不同输入顺序会渲染出不同字节。
func validateTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		t := strings.ToLower(strings.TrimSpace(raw))
		t = strings.Trim(t, "#")
		if t == "" {
			continue
		}
		if len(t) > maxTagLen {
			return nil, fmt.Errorf("tag %q too long (%d > %d)", raw, len(t), maxTagLen)
		}
		for _, r := range t {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return nil, fmt.Errorf("tag %q has unsupported character %q", raw, r)
			}
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sortStrings(out)
	if len(out) > maxTags {
		return nil, fmt.Errorf("too many tags (%d > %d)", len(out), maxTags)
	}
	return out, nil
}

// normalizeBody 正文归一：统一换行 + 去首尾空行。
//
// 只做这两件，不做 Markdown 结构改写——正文的"字节稳定"由用户维护，
// 我们的归一只消除"以尾随换行结尾"这类必然差异。
func normalizeBody(body string) string {
	s := strings.ReplaceAll(body, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Trim(s, "\n")
	if s == "" {
		return ""
	}
	return s + "\n"
}

// safeJoin 在 base 下拼接**受校验的**单级名，失败即报错（不返回拼接结果）。
//
// 路径边界：第一层靠 validateTopicName/validateRepoID 拒绝分隔符与 ..，
// 第二层靠这里对结果做前缀断言（防御 validate* 被改坏/绕过）。
func safeJoin(base, name string) (string, error) {
	if err := validateRepoID(name); err != nil {
		return "", err
	}
	full := filepath.Join(base, name)
	if !isUnder(base, full) {
		return "", fmt.Errorf("path %q escapes %q", full, base)
	}
	return full, nil
}

// isUnder child 是否等于或在 base 之下（两侧都取绝对清理后的形态）。
func isUnder(base, child string) bool {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absChild)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// slugifyPath 把任意文本压成安全目录/文件名片段（小写、字母数字连字符、限长）。
func slugifyPath(s string) string {
	var b strings.Builder
	prevDash := true // 行首不产生前导连字符
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxTopicNameLen {
		out = strings.Trim(out[:maxTopicNameLen], "-")
	}
	return out
}

// sanitizeID 折叠非法字符为连字符（供 ID 构造内部使用，不做完整校验）。
func sanitizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

// sortStrings 小规模字符串排序（避免为一处调用引入 sort.Slice 的反射）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// pathBase 取 URL/路径形态字符串的末段（跨平台安全：先转 slash 再取 base）。
func pathBase(p string) string {
	return path.Base(filepath.ToSlash(p))
}
