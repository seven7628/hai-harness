package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seven7628/hai-harness/agents"
	"gopkg.in/yaml.v3"
)

// AgentDefinition 一个文件化自定义子 agent 定义（{name}.md：yaml 头部 + 职责正文）。
//
//	my-subagent.md
//	├── frontmatter（yaml，--- 包裹）
//	│    name: my-subagent          # 必填：注册表键
//	│    description: ...           # 必填：何时委托（注入系统提示词清单）
//	│    tools: ["Read", "Grep"]    # 可选：v1 仅元数据
//	│    model: sonnet              # 可选：v1 仅元数据
//	└── 正文                        # 子 agent 的 system prompt 人设层
type AgentDefinition struct {
	Name         string   // yaml name（注册表键）
	Description  string   // yaml description（何时委托；注入系统提示词清单）
	Tools        []string // yaml tools（可选；v1 仅元数据）
	Model        string   // yaml model（可选；v1 仅元数据）
	Instructions string   // 正文：子 agent 的 system prompt 人设层
	Source       string   // 来源文件路径（展示/调试）
}

// DefinitionRegistry 自定义 subagent 定义注册表：分层扫描 {layer}/*.md + 同名高层覆盖。
//
// 与任务注册表（Registry，agent_spawn 用）命名区分：本注册表只管理**定义文件**
// 的发现与加载（发现阶段 List / 激活阶段 Load），不涉任务生命周期。
// 命名空间：全局 ~/.go-code/agents 低优先 + 工作区 {ws}/agents 高优先（同名覆盖）。
type DefinitionRegistry struct {
	defs map[string]*AgentDefinition
}

// Layer 一个发现层：目录 + 解析严格度。
//
// Lenient = 允许「无 frontmatter / frontmatter 缺字段」的 .md：Claude Code 的
// ~/.claude/agents/*.md 既有文件常是纯 markdown（首行 `# 标题`，无 yaml 头部），
// 严格层会把它们全部当损坏跳过 —— 兼容层必须能补出元数据：
// name = 文件名（去 .md），description = 首个非空行（去前导 #）。
type Layer struct {
	Dir     string
	Lenient bool
}

// NewDefinitionRegistry 扫描多层定义目录并合并（高优先级覆盖低优先级同名）。
// 全部层按**严格**解析（Agent Skills 约定：必须有 frontmatter + name/description）。
func NewDefinitionRegistry(layers ...string) (*DefinitionRegistry, error) {
	ls := make([]Layer, 0, len(layers))
	for _, dir := range layers {
		ls = append(ls, Layer{Dir: dir})
	}
	return NewLayeredDefinitionRegistry(ls...)
}

// NewLayeredDefinitionRegistry 同 NewDefinitionRegistry，但每层可单独指定解析严格度
// （Layer.Lenient：外部产品既有无头部文件的兼容层用）。
func NewLayeredDefinitionRegistry(layers ...Layer) (*DefinitionRegistry, error) {
	r := &DefinitionRegistry{defs: make(map[string]*AgentDefinition)}
	for _, layer := range layers {
		dir := layer.Dir
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 层目录不存在：跳过（如未建 agents/ 的工作区）
			}
			return nil, fmt.Errorf("subagent registry: %w", err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			// 跟随符号链接：dotfiles/多机共享常把定义文件做成链接（{name}.md →
			// 仓库里的真文件）。DirEntry 对 symlink 既不认目录也不认普通文件，
			// 旧口径（e.IsDir() + 后缀）会整条漏掉；链接指向目录 → 不是定义文件。
			fi, err := os.Stat(filepath.Join(dir, e.Name()))
			if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
				continue // 链接指向目录/特殊文件（fifo 等）不是定义文件；断链跳过
			}
			def, err := parseAgentFileIn(filepath.Join(dir, e.Name()), layer.Lenient)
			if err != nil {
				continue // 损坏定义：跳过（与 skills 损坏跳过同风格）
			}
			r.defs[def.Name] = def // 后层覆盖前层同名
		}
	}
	return r, nil
}

// List 返回元数据清单（发现阶段：仅 name/description，按名排序）——
// 注入系统提示词（渐进披露）。实现 agents.SubagentLister。
func (r *DefinitionRegistry) List() []agents.SubagentDescriptor {
	out := make([]agents.SubagentDescriptor, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, agents.SubagentDescriptor{Name: d.Name, Description: d.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All 返回全部定义（含元数据+来源，宿主 UI/调试用；含未注入清单的字段）。
func (r *DefinitionRegistry) All() []*AgentDefinition {
	out := make([]*AgentDefinition, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load 按名加载完整定义（激活阶段）。未找到报错。
func (r *DefinitionRegistry) Load(name string) (*AgentDefinition, error) {
	d, ok := r.defs[name]
	if !ok {
		return nil, fmt.Errorf("subagent %q not found", name)
	}
	return d, nil
}

// parseAgentFile 解析单个定义文件（严格）：yaml frontmatter（--- 包裹）→ 元数据，
// 正文 → Instructions。宽容策略（对齐 skills.parseFrontmatter）：真 YAML 优先；
// 解析失败回退逐行宽松解析。
func parseAgentFile(path string) (*AgentDefinition, error) {
	return parseAgentFileIn(path, false)
}

// parseAgentFileIn 解析单个定义文件。lenient = 允无 frontmatter / 缺字段的外部文件
// （Claude Code 层）：无头部时整篇当正文，name 取文件名、description 取首个非空行；
// 头部缺某字段时同样按此补齐。严格模式缺 name/description 仍报错（旧契约不变）。
func parseAgentFileIn(path string, lenient bool) (*AgentDefinition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, body, err := parseAgentFrontmatter(raw)
	if err != nil {
		if !lenient {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		meta, body = map[string]any{}, string(raw) // 无 frontmatter：整篇当正文
	}
	def := &AgentDefinition{
		Name:         strings.TrimSpace(strOf(meta["name"])),
		Description:  strings.TrimSpace(strOf(meta["description"])),
		Tools:        strSliceOf(meta["tools"]),
		Model:        strings.TrimSpace(strOf(meta["model"])),
		Instructions: strings.TrimSpace(body),
		Source:       path,
	}
	if lenient {
		// 外部（Claude Code）文件常无头部或头部不全：从文件自身补出可用元数据，
		// 而不是整条丢弃 —— 兼容层的意义就是让这些文件能被选中。
		if def.Name == "" {
			def.Name = strings.TrimSuffix(filepath.Base(path), ".md")
		}
		if def.Description == "" {
			def.Description = firstLineDescription(body)
		}
	}
	if def.Name == "" {
		return nil, fmt.Errorf("%s: name is required in frontmatter", path)
	}
	if def.Description == "" {
		return nil, fmt.Errorf("%s: description is required in frontmatter", path)
	}
	return def, nil
}

// maxDerivedDescription 从正文首行推导 description 的长度上限（注入系统提示词的
// 清单行不该被一整段正文撑爆）。
const maxDerivedDescription = 200

// firstLineDescription 取正文首个非空行当 description（去前导 #/空白、超长截断）。
// 无可用行 → 空串（调用方按「损坏」跳过）。
func firstLineDescription(body string) string {
	for _, line := range splitLines([]byte(body)) {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxDerivedDescription {
			return string(r[:maxDerivedDescription]) + "…"
		}
		return line
	}
	return ""
}

// parseAgentFrontmatter 解析 yaml frontmatter：首行 --- 开始，找 closing ---，
// 中间块 yaml 解析（失败回退宽松逐行），返回元数据 map 与正文。
func parseAgentFrontmatter(raw []byte) (map[string]any, string, error) {
	lines := splitLines(raw)
	if len(lines) == 0 {
		// 空文件：splitLines 返回 nil，直接取 lines[0] 会 panic —— 发现阶段扫的是
		// 用户目录，任意空 .md 都能踩到，必须按「损坏定义」处理而不是崩进程。
		return nil, "", fmt.Errorf("empty file (no frontmatter)")
	}
	first := strings.TrimSpace(strings.TrimPrefix(lines[0], "\xef\xbb\xbf")) // 容忍 BOM
	if first != "---" {
		return nil, "", fmt.Errorf("missing frontmatter (must start with ---)")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, "", fmt.Errorf("unterminated frontmatter")
	}
	block := strings.Join(lines[1:end], "\n")
	body := strings.Join(lines[end+1:], "\n")

	var meta map[string]any
	if err := yaml.Unmarshal([]byte(block), &meta); err == nil && meta != nil {
		return meta, body, nil
	}
	return parseLenientAgentFrontmatter(block), body, nil
}

// parseLenientAgentFrontmatter 宽松逐行解析（严格 YAML 失败时的兜底）：
// 每行 key: value（取第一个冒号），忽略空行/注释；只覆盖顶层标量字段。
func parseLenientAgentFrontmatter(block string) map[string]any {
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

// strOf frontmatter 字段转 string（yaml 可能给非字符串，如带引号/数字）。
func strOf(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	if b, err := yaml.Marshal(v); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// strSliceOf 解析 yaml 字段（tools）→ []string；兼容数组/字符串/标量。
// 字符串既支持空格分隔（go-code 约定），也支持逗号分隔（Claude Code 的
// `tools: Read, Grep, Glob` 形态）。
func strSliceOf(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strOf(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	default:
		s := strOf(v)
		if s == "" {
			return nil
		}
		if strings.Contains(s, ",") {
			out := make([]string, 0, strings.Count(s, ",")+1)
			for _, part := range strings.Split(s, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
		return strings.Fields(s)
	}
}

// splitLines 按行切分（保留空行语义用于正文分隔）。
func splitLines(raw []byte) []string {
	text := string(raw)
	if len(text) == 0 {
		return nil
	}
	return strings.Split(text, "\n")
}
