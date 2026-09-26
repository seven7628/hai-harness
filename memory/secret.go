package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// Secret 命中的一条敏感信息。
type Secret struct {
	// Kind 规则名（"anthropic-api-key" / "private-key" / ...），用于错误信息与测试断言。
	Kind string
	// Line 出问题的行号（1 起）。
	Line int
}

// secretRule 一条敏感信息规则。
type secretRule struct {
	kind string
	re   *regexp.Regexp
}

// secretRules 基础敏感信息规则集。
//
// 边界（明确的取舍）：这是**基础**扫描，不是完整脱敏器。
//   - 命中即**拒绝写入**（不静默改写记忆内容——半脱敏的记忆会让人误以为它完整）；
//   - 只覆盖"高置信度 + 不可撤销"的形态（模型厂商 API key、私钥块、显式标注的
//     凭据赋值）。低置信度形态（短 token、疑似哈希、base64 大块）**故意不拦**：
//     误报会逼用户把正常内容（hash、fixture、id）从记忆里删掉，那比漏一个弱 secret 更糟；
//   - 不覆盖 `.env` 路径名、SSH/GPG 公钥（本来就是公开的）、密码字段名本身
//     （"password" 这个词不是秘密）。
var secretRules = []secretRule{
	{"anthropic-api-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)},
	{"openai-api-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"explicit-credential", regexp.MustCompile(`(?i)\b(api[_-]?key|secret[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret)\b\s*[:=]\s*["']?[A-Za-z0-9/+_.-]{16,}`)},
	{"bearer-token", regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*["']?bearer\s+[A-Za-z0-9._-]{16,}`)},
}

// ScanSecrets 扫描文本中的基础敏感信息；命中返回首个命中（含行号），无命中返回 nil。
//
// 边界：只做**行级**判定（返回第一条命中的行），不做熵值分析、不做跨行拼接
// 检测——够用的最小集，且行为完全可预测（便于测试与用户预期）。
func ScanSecrets(text string) error {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		for _, rule := range secretRules {
			if rule.re.MatchString(line) {
				return fmt.Errorf("memory: possible %s on line %d: refusing to store secrets in memory", rule.kind, i+1)
			}
		}
	}
	return nil
}
