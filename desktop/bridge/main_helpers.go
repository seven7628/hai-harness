// main_helpers.go —— main.go 拆出的自包含工具函数（V2 #48 大文件拆分第一步）。
//
// 纯搬移、零逻辑改动：这些函数不依赖 dispatch/manager 内部状态，可独立测试。
// 剩余巨型 dispatch switch 的拆分待 git 基线建立后进行（无 git 时大搬移风险高）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/tools/builtin"

	"github.com/pmezard/go-difflib/difflib"
)

// sessionTitle：取会话第一条 user 文本作标题（无内容回退 id——保持旧调用语义；
// listSessions 已改走 resolveDiskSessionTitle，含 "New Chat" 与 AI 标题）。
func sessionTitle(path, id string) string {
	if first := firstUserTextFromFile(path); first != "" {
		return truncateTitle(first)
	}
	return id
}

// firstUserTextFromFile 读会话 jsonl 第一条非空 user 文本（冷磁盘会话标题回退；
// 无内容返回 ""，由调用方决定 "New Chat" 或 id）。
func firstUserTextFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	if sc.Scan() {
		var rec struct {
			Type     string         `json:"type"`
			Messages []core.Message `json:"messages"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Type == "history" {
			for _, m := range rec.Messages {
				if m.Role != core.User {
					continue
				}
				for _, c := range m.Content {
					if c.Type == core.ContentTypeText {
						if t := strings.TrimSpace(c.Content); t != "" {
							return t
						}
					}
				}
			}
		}
	}
	return ""
}

// —— Skills（2026-08-14）：分层注册表 + 启用过滤 + /skill 面板 ——
// agentSkillsDir 全局 Agent Skills 主目录（~/.agents/skills，跨工作区；2026-08-29 起
// 对话安装/外部导入的主路径）。
func agentSkillsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, skills.AgentDirName, "skills")
}

// globalSkillsDir 全局 go-code 技能目录（~/.go-code/skills，跨工作区；兼容旧路径，
// 2026-08-29 起为次优先层，新安装走 ~/.agents/skills）。
func globalSkillsDir() string {
	return filepath.Join(metricsHomeDir(), "skills")
}

// skillsEnabledPath 技能启用状态持久化（每工作区）：{"disabled":[names]}。
func skillsEnabledPath(ws string) string {
	return filepath.Join(ws, ".go-code", "skills.json")
}

// loadSkillsDisabled 读已禁用技能名集合（缺省空）。
func loadSkillsDisabled(ws string) []string {
	var v struct {
		Disabled []string `json:"disabled"`
	}
	if b, err := os.ReadFile(skillsEnabledPath(ws)); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	return v.Disabled
}

// saveSkillsDisabled 持久化已禁用技能名集合。
// 工作区 `.go-code` 目录可能不存在（技能多来自 {ws}/.agents/skills 或全局
// ~/.agents/skills 的普通项目）：不建目录就写会 ENOENT —— 禁用状态静默丢失、
// 重启后技能又启用（2026-09 修复）。返回错误供调用处记日志。
func saveSkillsDisabled(ws string, disabled []string) error {
	b, _ := json.Marshal(map[string]any{"disabled": disabled})
	path := skillsEnabledPath(ws)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// buildSkillsRegistry 构建工作区技能注册表（2026-08-29 双命名空间四层发现，
// 对齐 Claude/Codex 的 .agents 约定并兼容既有 .go-code 路径）：
//   - 低优先级层：全局 ~/.agents/skills（主）→ ~/.go-code/skills（兼容旧路径）
//   - 高优先级层：{ws}/.agents/skills（主）→ {ws}/.go-code/skills（兼容旧路径）
//   - 同名技能以高层覆盖低层；工作区层覆盖全局层
//
// 按持久化恢复禁用集。
func buildSkillsRegistry(ws string) *skills.Registry {
	reg, err := skills.NewLayeredRegistry(
		agentSkillsDir(),
		globalSkillsDir(),
		filepath.Join(ws, skills.AgentDirName, "skills"),
		filepath.Join(ws, skills.GoCodeDirName, "skills"),
	)
	if err != nil {
		return reg // 目录不可读：空注册表（skill 功能降级，不影响其他命令）
	}
	for _, name := range loadSkillsDisabled(ws) {
		_ = reg.SetEnabled(name, false)
	}
	return reg
}

// —— 自定义 Subagent（2026-08-29）：文件化定义 + spawn_agent 派发 ——
// globalSubagentsDir 全局 go-code 自定义 subagent 目录（~/.go-code/agents，跨工作区；
// 兼容旧路径，次优先）。
func globalSubagentsDir() string {
	return filepath.Join(metricsHomeDir(), "agents")
}

// globalAgentSubagentsDir 全局通用 Agent Skills 命名空间 subagent 目录
// （~/.agents/agents，跨工作区；通用协议优先，主路径）。
func globalAgentSubagentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, skills.AgentDirName, "agents")
}

// claudeSubagentsDir Claude Code 用户级 subagent 目录（~/.claude/agents，跨工作区；
// 兼容层：只读发现，claude 无 frontmatter 的文件按宽松解析兜底）。
func claudeSubagentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "agents")
}

// toInt64 兼容多值类型取整（unix ms；json 反序列化为 float64/int64 均可能）
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

// sessionUpdatedAt 会话 updated_at：in-memory lastActive → events 文件 mtime →
// sessions 文件 mtime → 0。冷磁盘会话以 lastActive=0 走同一回退链。
// maxDiffBytes 审批 diff 文本上限（与 builtin 保持一致，超限截断仅留统计）
const maxDiffBytes = 8 << 10

// unifiedDiff 行级 unified diff（old→new），返回 +- 统计与文本。
func unifiedDiff(path, old, new string) (added, removed int, unified string, truncated bool) {
	if old == new {
		return 0, 0, "", false
	}
	al, bl := splitLines(old), splitLines(new)
	for _, ops := range difflib.NewMatcher(al, bl).GetGroupedOpCodes(3) {
		for _, op := range ops {
			switch op.Tag {
			case 'i':
				added += op.J2 - op.J1
			case 'd':
				removed += op.I2 - op.I1
			case 'r':
				removed += op.I2 - op.I1
				added += op.J2 - op.J1
			}
		}
	}
	u, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: al, B: bl, FromFile: path, ToFile: path, Context: 3})
	if err != nil {
		return added, removed, "", true
	}
	if len(u) > maxDiffBytes {
		return added, removed, truncateUTF8(u, maxDiffBytes) + "\n[file diff truncated]\n", true
	}
	return added, removed, u, false
}

// splitLines 按行切分并保留行尾 \n（与 builtin 同款；空串返回 nil）。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// truncateUTF8 按字节上限截断且不劈裂多字节字符。
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// denyHarnessConfigPath 拒绝 go-code 自身配置/密钥路径（~/.go-code）——与工具层
// builtin.denyHarnessConfigPath 同保护对象（settings.json 可能含 api_key）。
func denyHarnessConfigPath(abs string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cfgDir := filepath.Clean(filepath.Join(home, ".go-code"))
	if abs == cfgDir || strings.HasPrefix(abs, cfgDir+string(os.PathSeparator)) {
		// 受管子树放行（~/.go-code/plugins + ~/.go-code/runtime）：与 builtin 同语义
		// （受管组件与受管 Python 运行时都要文件工具可达；两处必须同步改）
		for _, sub := range []string{"plugins", "runtime"} {
			dir := filepath.Join(cfgDir, sub)
			if abs == dir || strings.HasPrefix(abs, dir+string(os.PathSeparator)) {
				return nil
			}
		}
		return fmt.Errorf("path %q is go-code's own config/secret directory and is protected", abs)
	}
	return nil
}

// resolveWorkspacePath 解析预览类命令的文件路径（**只做路径解析，不做读取**）：
//   - 相对路径 → 锚定 workspace（.. 逃逸拒绝）；
//   - 绝对路径 → 原样采用（支持工作区外，与 builtin resolve 同语义；go-code 自身
//     配置/密钥目录 ~/.go-code 拒绝——同 denyHarnessConfigPath 保护对象）。
//
// 抽出来给 file_preview 与 xlsx_preview 共用：两条通路对「路径合法性」的口径必须一致
// （否则同一路径一个能预览一个不能，用户只会看到「有时能看有时不能」），各自写一份
// 迟早漂移 —— file_preview 的读取逻辑（大小上限/截断）仍留在 readFilePreview。
func resolveWorkspacePath(workspace, raw string) (string, error) {
	if filepath.IsAbs(raw) {
		p := filepath.Clean(raw)
		if err := denyHarnessConfigPath(p); err != nil {
			return "", err
		}
		return p, nil
	}
	base := filepath.Clean(workspace)
	p := filepath.Clean(filepath.Join(base, raw))
	if p != base && !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace", raw)
	}
	return p, nil
}

// previewInputPath 三条预览通路（xlsx_preview / docx_preview / pptx_preview）共用的
// 输入路径校验：返回绝对的、可读的**普通文件**路径；不通过时已回错误响应（ok=false）。
//
// 为什么必须共用一份（而不是每条通路各写一遍）：越界路径、受保护目录（~/.go-code）、
// 目录、不存在的文件，这四种拒绝的**理由与措辞**是用户可见的产品行为 —— 「看的是 Word」
// 不该得到比「看的是表格」更松或更严的判定，也不该冒出第二种说法。三条通路唯一的差别
// 是 kind 这个词本身（"path 是目录，不是 docx 文件"）。
func (m *manager) previewInputPath(c command, ws, path, kind string) (string, bool) {
	if path == "" {
		m.resp(c, false, "path 为空", nil)
		return "", false
	}
	// 绝对路径不依赖 workspace（工作区外统一 open-file）；仅相对路径需要锚定。
	if ws == "" && !filepath.IsAbs(path) {
		m.resp(c, false, "workspace 为空", nil)
		return "", false
	}
	// 路径口径与 file_preview 完全一致（同一个 resolveWorkspacePath）：越界/受保护目录
	// 的拒绝理由不应因为「看的是文档」而变。
	abs, err := resolveWorkspacePath(ws, path)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil {
		m.resp(c, false, "无法读取文件: "+err.Error(), nil)
		return "", false
	}
	if info.IsDir() {
		m.resp(c, false, "path 是目录，不是 "+kind+" 文件: "+abs, nil)
		return "", false
	}
	return abs, true
}

// readFilePreview 读文件供预览（统一 open-file 能力，2026-09）：路径解析见
// resolveWorkspacePath；本函数负责读取与截断。
//   - 全量不截断（文件栏「最终文件」= 完整内容）超出上限时截断并报 truncated。
//
// binary 标记（2026-09 补）：二进制文件（xlsx/docx/pdf/图片）**不该当字符串预览**——
// 前端会把 content 喂给 shiki 高亮，结果是乱码 + 白耗渲染（实测：从文件树点开 .xlsx
// 走的就是这条路；产出卡入口有专门分支，文件栏入口此前没有）。故读头部做一次
// LookBinary 判定，binary=true 时前端改为提示「该格式请在产出卡预览或用外部应用打开」。
// 判定用 tools/builtin.LookBinary（与 read_file 的二进制拒绝同一份判据，避免两处漂移）。
func readFilePreview(workspace, raw string) (content string, lines int, truncated, binary bool, err error) {
	p, err := resolveWorkspacePath(workspace, raw)
	if err != nil {
		return "", 0, false, false, err
	}
	// 大小上限：预览只读前 maxPreviewBytes（V2 资源边界观察项）。超大文件全量
	// ReadFile 会占内存 + 全量发给前端——截断预览即可（前端已有 truncated 语义）。
	f, err := os.Open(p)
	if err != nil {
		return "", 0, false, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, false, false, err
	}
	// 二进制判定：只读头部（判据本身只看前 8KB），避免为判定整读大文件。
	head := make([]byte, 8*1024)
	n, herr := io.ReadFull(f, head)
	if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
		return "", 0, false, false, herr
	}
	if builtin.LookBinary(head[:n]) {
		return "", 0, false, true, nil // 不返回内容：调用方只需知道「这是二进制」
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, false, false, err
	}
	if info.Size() > maxPreviewBytes {
		b, rerr := io.ReadAll(io.LimitReader(f, maxPreviewBytes))
		if rerr != nil {
			return "", 0, false, false, rerr
		}
		s := string(b)
		lines = strings.Count(s, "\n")
		return s, lines, true, false, nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", 0, false, false, err
	}
	s := string(b)
	lines = strings.Count(s, "\n")
	return s, lines, false, false, nil
}

// maxPreviewBytes file_preview 单文件预览上限（1 MiB；超限截断 + truncated 标记）。
// injectJSONKeys 把键值对注入 JSON 对象首部（保持合法 JSON）。
// 每个 kv 的 Value 是已 JSON 序列化的值（string 用 json.Marshal 转义，map 用预序列化）。
type jsonKeyVal struct {
	Key   string
	Value []byte // 已 JSON 序列化的值
}

func injectJSONKeys(base []byte, kvs []jsonKeyVal) ([]byte, error) {
	if len(kvs) == 0 {
		return base, nil
	}
	out := make([]byte, 0, len(base)+128)
	out = append(out, '{')
	for i, kv := range kvs {
		if i > 0 {
			out = append(out, ',')
		}
		kb, _ := json.Marshal(kv.Key)
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, kv.Value...)
	}
	// 追加原对象内容（跳过首 { 与尾 }）
	rest := base
	if len(rest) > 0 && rest[0] == '{' {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[len(rest)-1] == '}' {
		rest = rest[:len(rest)-1]
	}
	if len(rest) > 0 {
		out = append(out, ',')
		out = append(out, rest...)
	}
	out = append(out, '}')
	return out, nil
}

// approvalExtras 计算审批预演 diff 的宿主附加字段（替代原 augmentApprovalDiff 的 map 操作）。
