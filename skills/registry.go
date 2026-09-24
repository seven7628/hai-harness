package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// 技能发现路径（2026-08-29 双命名空间兼容）：
//
//	全局层（低优先级，跨工作区）：
//	  ~/.agents/skills        —— Claude/通用 Agent Skills 约定（主路径）
//	  ~/.go-code/skills       —— go-code 自有命名空间（兼容既有安装）
//	工作区层（高优先级，同名覆盖全局）：
//	  {ws}/.agents/skills     —— Claude/通用 Agent Skills 约定（主路径）
//	  {ws}/.go-code/skills    —— go-code 自有命名空间（兼容既有安装）
//
// 层级顺序（低 → 高）：~/.agents/skills < ~/.go-code/skills < {ws}/.agents/skills < {ws}/.go-code/skills，
// 同名技能以高层覆盖低层。目录名常量供桌面端/远端安装共用，保证全链路一致。
const (
	// AgentDirName 通用 Agent Skills 命名空间目录（~/.agents、{ws}/.agents）。
	AgentDirName = ".agents"
	// GoCodeDirName go-code 自有命名空间目录（~/.go-code、{ws}/.go-code）。
	GoCodeDirName = ".go-code"
)

// Registry 技能注册表：扫描目录载入全部技能元数据（发现阶段），
// 按名加载完整指令与资源清单（激活阶段）。
//
// 支持分层目录（NewLayeredRegistry）：低优先级在前、高优先级在后，
// 同名技能以高优先级层覆盖（桌面端 = 全局 ~/.agents/skills + ~/.go-code/skills
// 低优先 + 工作区 .agents/skills + .go-code/skills 高优先）。
//
// 启用过滤：Registry 维护 per-skill 启用集（默认全启用，SetEnabled 关闭）。
// List/发现清单只暴露启用技能（模型只看到启用的）；Load 拒绝加载已禁用技能
// （模型 load_skill 与产品 /skills load 一致）；Skill/All 为宿主专用（UI 展示/预览不过滤）。
type Registry struct {
	dir     string
	skills  map[string]*Skill
	enabled map[string]bool
}

// NewRegistry 扫描 dir 下所有含 SKILL.md 的子目录并解析元数据（单层）；
// 损坏的 SKILL.md（缺 frontmatter / name / description）跳过，不影响整体。
func NewRegistry(dir string) (*Registry, error) {
	return NewLayeredRegistry(dir)
}

// NewLayeredRegistry 扫描多层技能目录并合并（高优先级覆盖低优先级同名技能）。
// 全局层在前、工作区层在后 → 工作区同名技能覆盖全局。
func NewLayeredRegistry(layers ...string) (*Registry, error) {
	r := &Registry{skills: make(map[string]*Skill), enabled: make(map[string]bool)}
	for _, dir := range layers {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 层目录不存在：跳过（如未建 .agents/skills 的工作区）
			}
			return nil, fmt.Errorf("skills registry: %w", err)
		}
		for _, e := range entries {
			// 跟随符号链接判定目录：ego lite onboarding 会把它的 skill 以 symlink
			// 放入技能层目录（~/.agents/skills/ego-browser → app Resources/ego-skills），
			// DirEntry.IsDir() 对 symlink 返回 false 会漏扫——用 os.Stat 跟随解析。
			fi, err := os.Stat(filepath.Join(dir, e.Name()))
			if err != nil || !fi.IsDir() {
				continue
			}
			s, err := parseSkill(filepath.Join(dir, e.Name()))
			if err != nil {
				continue // 损坏技能：跳过
			}
			r.skills[s.Name] = s // 后层覆盖前层同名
		}
	}
	// 启用集默认全开（尚未被 SetEnabled 关闭）
	for name := range r.skills {
		r.enabled[name] = true
	}
	return r, nil
}

// SetEnabled 开/关技能（宿主产品偏好）：关闭后该技能从发现清单消失、
// Load 拒绝（模型与 /skills load 均不可加载）。未知技能名报错。
func (r *Registry) SetEnabled(name string, on bool) error {
	if _, ok := r.skills[name]; !ok {
		return fmt.Errorf("skill %q not found", name)
	}
	r.enabled[name] = on
	return nil
}

// IsEnabled 技能当前启用状态（宿主 UI 展示）。
func (r *Registry) IsEnabled(name string) bool {
	return r.enabled[name]
}

// List 返回启用的技能元数据（发现阶段：仅 name/description，按名排序）。
// 模型系统提示词的技能清单以此为数据源——禁用技能模型不可见。
func (r *Registry) List() []Descriptor {
	var out []Descriptor
	for name, s := range r.skills {
		if !r.enabled[name] {
			continue
		}
		out = append(out, Descriptor{Name: s.Name, Description: s.Description, Enabled: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All 返回全部技能元数据（含禁用，带 Enabled 标志）——宿主 UI（SkillsPage/面板）展示用。
func (r *Registry) All() []Descriptor {
	out := make([]Descriptor, 0, len(r.skills))
	for name, s := range r.skills {
		out = append(out, Descriptor{Name: s.Name, Description: s.Description, Enabled: r.enabled[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Skill 按名取技能原始数据（不过滤启用状态）——宿主预览/管理用（skill_get 命令）。
// 资源路径为相对技能目录的可定位路径（如 "~/.agents/skills/code-review/references/guide.md"）。
func (r *Registry) Skill(name string) (*Skill, error) {
	s, ok := r.skills[name]
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	out := *s
	out.Resources = r.resources(s)
	return &out, nil
}

// Load 按名加载技能完整指令与资源清单（激活阶段）——校验启用状态：
// 已禁用的技能拒绝加载（发现清单外的东西模型不该能取到）。
func (r *Registry) Load(name string) (*Skill, error) {
	s, err := r.Skill(name)
	if err != nil {
		return nil, err
	}
	if !r.enabled[name] {
		return nil, fmt.Errorf("skill %q is disabled", name)
	}
	return s, nil
}

// resources 收集技能资源子目录（scripts/references/assets）下的文件，
// 路径 = 技能自身目录（分层注册表下各层技能各自正确，不再依赖单 registry 根）。
func (r *Registry) resources(s *Skill) []string {
	var res []string
	for _, sub := range []string{"scripts", "references", "assets"} {
		files, err := os.ReadDir(filepath.Join(s.Dir, sub))
		if err != nil {
			continue
		}
		for _, f := range files {
			res = append(res, filepath.ToSlash(filepath.Join(s.Dir, sub, f.Name())))
		}
	}
	sort.Strings(res)
	return res
}
