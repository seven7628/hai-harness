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
//
// 注意：本方法**不**按能力过滤（等价于 ListFor(nil)）。会话装配请用 ListFor：
// 同一个注册表会被不同模型/工具集的会话共享（desktop/bridge 的 per-workspace
// 注册表就是这样），在这一层做全局过滤会把 A 会话的能力当成 B 会话的。
func (r *Registry) List() []Descriptor {
	return r.ListFor(nil)
}

// All 返回全部技能元数据（含禁用，带 Enabled 标志）——宿主 UI（SkillsPage/面板）展示用。
func (r *Registry) All() []Descriptor {
	return r.AllFor(nil)
}

// Skill 按名取技能原始数据（不过滤启用状态）——宿主预览/管理用（skill_get 命令）。
// 资源路径为相对技能目录的可定位路径（如 "~/.agents/skills/code-review/references/guide.md"）。
func (r *Registry) Skill(name string) (*Skill, error) {
	s, ok := r.skills[name]
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	out := *s
	// 恒返回非 nil 切片：nil 切片序列化成 JSON `null`，而所有消费方（前端
	// skillDetail.resources.length / 命令输出拼接）都按数组用 —— 一个没有
	// scripts/references/assets 的技能会因此把整块 UI 打崩（2026-10 真机事故：
	// visual-ui 等三个随包技能都没有资源目录 → 技能详情一打开就 TypeError）。
	// 空技能给 `[]`，绝不返回 null。
	out.Resources = append([]string{}, r.resources(s)...)
	return &out, nil
}

// ListFor 返回在给定能力面下可用的已启用技能（发现阶段）。
// caps == nil = 不过滤（等价 List），供不支持能力过滤的宿主/测试使用。
// 按名排序；状态说明见 UnavailableFor。
func (r *Registry) ListFor(caps *Capabilities) []Descriptor {
	out := make([]Descriptor, 0, len(r.skills))
	for _, name := range r.names() {
		if !r.enabled[name] {
			continue
		}
		s := r.skills[name]
		if len(caps.Missing(s.Requires)) > 0 {
			continue
		}
		out = append(out, Descriptor{Name: s.Name, Description: s.Description, Enabled: true, Requires: s.Requires})
	}
	return out
}

// AllFor 返回全部技能元数据（含禁用与**能力不足**的），caps == nil = 不过滤。
// 宿主 UI 用它把"这个会话用不了、为什么"如实显示出来（UnavailableFor 给原因）。
func (r *Registry) AllFor(caps *Capabilities) []Descriptor {
	out := make([]Descriptor, 0, len(r.skills))
	for _, name := range r.names() {
		s := r.skills[name]
		out = append(out, Descriptor{
			Name: s.Name, Description: s.Description,
			Enabled:  r.enabled[name] && len(caps.Missing(s.Requires)) == 0,
			Requires: s.Requires,
		})
	}
	return out
}

// UnavailableFor 技能在当前会话不可用的原因（空 = 可用或该技能不存在）。
// 宿主 UI 展示 + load_skill 拒绝文案共用，保证「面板说的」和「工具说的」逐字一致。
func (r *Registry) UnavailableFor(name string, caps *Capabilities) string {
	s, ok := r.skills[name]
	if !ok {
		return ""
	}
	missing := caps.Missing(s.Requires)
	if len(missing) == 0 {
		return ""
	}
	return caps.reason(name, missing)
}

// names 已排序的技能名（List/All 共用，避免两处各自 sort 一份拷贝）。
func (r *Registry) names() []string {
	out := make([]string, 0, len(r.skills))
	for name := range r.skills {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Load 按名加载技能完整指令与资源清单（激活阶段）——校验启用状态：
// 已禁用的技能拒绝加载（发现清单外的东西模型不该能取到）。
//
// 等价于 LoadFor(name, nil)：**不**做能力过滤。会话装配请用 LoadFor，否则会与
// ListFor 的过滤结果错位（清单里没有、工具却能加载同名技能）。
func (r *Registry) Load(name string) (*Skill, error) {
	return r.LoadFor(name, nil)
}

// LoadFor 按名加载技能，并校验能力面（caps == nil = 不过滤）。
// 拒绝顺序刻意是「不存在 → 禁用 → 能力不足」：前两者是用户可修的配置问题，
// 后者是"换个模型才行"，文案必须各自说清（见 Capabilities.reason）。
func (r *Registry) LoadFor(name string, caps *Capabilities) (*Skill, error) {
	s, err := r.Skill(name)
	if err != nil {
		return nil, err
	}
	if !r.enabled[name] {
		return nil, fmt.Errorf("skill %q is disabled", name)
	}
	if msg := r.UnavailableFor(name, caps); msg != "" {
		return nil, fmt.Errorf("%s", msg)
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
