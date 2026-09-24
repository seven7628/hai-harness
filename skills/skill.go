package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill 一个技能（Agent Skills 开放标准：目录 + SKILL.md）。
//
//	my-skill/
//	├── SKILL.md          # 必选：frontmatter（name/description）+ 指令正文
//	├── scripts/          # 可选：可执行代码
//	├── references/       # 可选：资料
//	└── assets/           # 可选：模板/资源
//
// 渐进披露（progressive disclosure）：
//   - 发现（Discovery）：Registry.List 只暴露元数据，注入系统提示词；
//   - 激活（Activation）：模型调用 load_skill 工具，完整指令进入上下文。
type Skill struct {
	Name        string
	Description string

	License      string         // 可选：许可证（frontmatter license）
	Version      string         // 可选：内容版本（frontmatter version，如 "1.0.0"）——随包分发的技能靠它决定是否覆盖用户机上的旧副本（见 desktop/bridge/office_skill_sync.go）
	Metadata     map[string]any // 可选：任意键值元数据（frontmatter metadata 子映射）
	AllowedTools []string       // 可选：预批准工具白名单（allowed-tools 空格分隔；远端技能安全提示用）

	Instructions string   // 完整指令（SKILL.md 正文）
	Resources    []string // 技能资源（scripts/references/assets 下的文件，相对路径）
	Dir          string   // 技能目录
}

// Descriptor 技能的轻量元数据（发现阶段）：物理上不含指令正文。
// Enabled 标志宿主 UI 展示用（All）；List 只返回启用项。
type Descriptor struct {
	Name        string
	Description string
	Enabled     bool
}

// parseSkill 解析一个技能目录：SKILL.md 必须以 frontmatter 开头，
// 且必须含 name 与 description，否则视为损坏技能。
func parseSkill(skillDir string) (*Skill, error) {
	path := filepath.Join(skillDir, "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, body, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := &Skill{
		Name:         strOf(meta["name"]),
		Description:  strOf(meta["description"]),
		License:      strOf(meta["license"]),
		Version:      strOf(meta["version"]),
		Metadata:     mapOf(meta["metadata"]),
		AllowedTools: parseAllowedTools(meta["allowed-tools"]),
		Instructions: strings.TrimSpace(body),
		Dir:          skillDir,
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ParseSkill 解析单个技能目录（skills/remote 远端安装校验用；同 parseSkill）。
func ParseSkill(skillDir string) (*Skill, error) {
	return parseSkill(skillDir)
}

// strOf 前端字段转 string（yaml 可能给非字符串，如带引号/数字）。
func strOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	// yaml 数字/布尔等：序列化兜底
	if b, err := yaml.Marshal(v); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// mapOf metadata 字段转 map（yaml 解析后为 map[string]any 或 map[any]any）。
func mapOf(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out
	}
	return nil
}

// parseAllowedTools 解析 allowed-tools 空格分隔字符串 → 切片。
func parseAllowedTools(v any) []string {
	s := strOf(v)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// Validate 校验技能完整性：元数据必填、指令正文非空。
func (s *Skill) Validate() error {
	switch {
	case s.Name == "":
		return errors.New("name is required in frontmatter")
	case s.Description == "":
		return errors.New("description is required in frontmatter")
	case s.Instructions == "":
		return errors.New("instructions (SKILL.md body) are required")
	}
	return nil
}

// parseFrontmatter 解析 SKILL.md 的 frontmatter（YAML）。
// 返回元数据 map 与正文（closing --- 之后）。
//
// 宽容策略（对齐 Agent Skills 客户端实现指南）：
//   - 真 YAML 优先：支持 metadata 嵌套映射、多行/含冒号描述（引号或块标量）；
//   - YAML 解析失败回退单行 key: value 逐行解析（部分工具生成的 frontmatter
//     `description: Use this when: ...` 在严格 YAML 下非法，宽松兜底）；
//   - 无 frontmatter（首行非 ---）报错 —— SKILL.md 必须声明元数据；
//   - 未闭合 frontmatter 报错。
func parseFrontmatter(raw []byte) (map[string]any, string, error) {
	lines := splitLines(raw)
	if len(lines) == 0 {
		// 空 SKILL.md：splitLines 返回 nil，直接取 lines[0] 会 panic —— 扫描的是用户
		// 技能目录（外部导入/手写都可能有空文件），必须按「损坏技能」跳过而不是崩进程。
		return nil, "", errors.New("empty SKILL.md (no frontmatter)")
	}
	first := strings.TrimSpace(strings.TrimPrefix(lines[0], "\xef\xbb\xbf")) // 容忍 BOM
	if first != "---" {
		return nil, "", errors.New("missing frontmatter (must start with ---)")
	}
	// 找 closing ---（frontmatter 内不处理嵌套 ---：YAML 块标量用缩进，--- 只作分隔）
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, "", errors.New("unterminated frontmatter")
	}
	block := strings.Join(lines[1:end], "\n")
	body := strings.Join(lines[end+1:], "\n")

	if meta, err := parseYAMLFrontmatter(block); err == nil {
		return meta, body, nil
	}
	return parseLenientFrontmatter(block), body, nil
}

// parseYAMLFrontmatter 严格 YAML 解析（能处理 metadata 嵌套/多行值/含冒号引号值）。
func parseYAMLFrontmatter(block string) (map[string]any, error) {
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
		return nil, err
	}
	if meta == nil {
		meta = map[string]any{}
	}
	// yaml.v3 顶层键统一 string（map[string]any）；若为 map[any]any 也收敛
	return mapOf(meta), nil
}

// parseLenientFrontmatter 宽松逐行解析（严格 YAML 失败时的兜底）：
// 每行 key: value（取第一个冒号），忽略空行/注释；只覆盖顶层标量字段。
func parseLenientFrontmatter(block string) map[string]any {
	meta := make(map[string]any)
	for _, line := range splitLines([]byte(block)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return meta
}

// splitLines 按行切分（保留空行语义用于正文分隔）。
func splitLines(raw []byte) []string {
	text := string(raw)
	if len(text) == 0 {
		return nil
	}
	return strings.Split(text, "\n")
}
