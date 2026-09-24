package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExternalSkillSource 描述一个外部技能目录的发现状态。
// RootExists 按产品约定决定导入功能是否可用；SkillsDirExists/SkillCount
// 用于设置页显示该来源下是否实际存在技能目录。
type ExternalSkillSource struct {
	Name            string `json:"name"`
	Root            string `json:"root"`
	SkillsDir       string `json:"skills_dir"`
	RootExists      bool   `json:"root_exists"`
	SkillsDirExists bool   `json:"skills_dir_exists"`
	SkillCount      int    `json:"skill_count"`
}

// ExternalSkillsStatus 是 Codex/Claude Code 外部技能的发现结果。
type ExternalSkillsStatus struct {
	Enabled bool                  `json:"enabled"`
	Sources []ExternalSkillSource `json:"sources"`
}

// ExternalSkillImport 是一次技能目录复制结果。相同名称从两个来源出现时会有两条记录，
// 后处理的来源会覆盖先处理的来源。
// LinkTarget 非空 = 来源是符号链接，目标目录里重建了**同一条链接**（值 = 链接解析后的
// 真实路径，单一来源：改源即两边生效）；为空 = 复制了真实目录内容。
type ExternalSkillImport struct {
	Source     string `json:"source"`
	Name       string `json:"name"`
	Target     string `json:"target"`
	LinkTarget string `json:"link_target,omitempty"`
}

// ExternalSkillsImportReport 是外部技能导入结果。
type ExternalSkillsImportReport struct {
	Status   ExternalSkillsStatus  `json:"status"`
	Imported []ExternalSkillImport `json:"imported"`
}

// externalSkill 一个来源技能条目：真实目录，或指向目录的符号链接。
type externalSkill struct {
	name string
	path string // 来源路径 {source.SkillsDir}/{name}
	link string // 非空 = 来源是符号链接，值为解析后的真实目标（导入时重建链接用）
}

// listExternalSkills 列出技能目录下的技能条目（**跟随符号链接**）：
//   - 真实子目录 → 技能；
//   - 符号链接且解析目标是目录 → 技能（记录解析后的真实路径）；
//   - 普通文件、指向文件的链接、断链 → 不是技能，跳过。
//
// 为什么必须跟随：~/.claude/skills 与 ~/.codex/skills 下的技能常以链接形式存在
// （ego lite onboarding、dotfiles/多机共享都这么做）。DirEntry.IsDir() 对 symlink
// 恒为 false，会把这类技能整条漏掉（计数为 0、导入静默跳过）——2026-09 修复。
func listExternalSkills(skillsDir string) ([]externalSkill, error) {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, err
	}
	out := make([]externalSkill, 0, len(entries))
	for _, e := range entries {
		path := filepath.Join(skillsDir, e.Name())
		fi, err := os.Stat(path) // 跟随链接：判定链接目标是否为目录
		if err != nil || !fi.IsDir() {
			continue // 断链 / 普通文件 / 指向文件的链接
		}
		sk := externalSkill{name: e.Name(), path: path}
		if e.Type()&os.ModeSymlink != 0 {
			// 链接要落到目标目录里：相对链接按来源目录解析后落成绝对路径，
			// 否则照抄相对路径会在目标目录下解析到别处（甚至断链）。
			real, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue // 链接链中某段缺失：不是可用技能
			}
			sk.link = real
		}
		out = append(out, sk)
	}
	return out, nil
}

// DetectExternalSkills 扫描用户主目录下的 ~/.codex 和 ~/.claude。
// 按要求，只要任一根目录存在且确实是目录，导入功能就可用；技能子目录不存在
// 不会让功能失效，导入时会跳过该来源。
func DetectExternalSkills(home string) ExternalSkillsStatus {
	status := ExternalSkillsStatus{Sources: externalSkillSources(home)}
	for i := range status.Sources {
		s := &status.Sources[i]
		if info, err := os.Stat(s.Root); err == nil && info.IsDir() {
			s.RootExists = true
			status.Enabled = true
		}
		if info, err := os.Stat(s.SkillsDir); err == nil && info.IsDir() {
			s.SkillsDirExists = true
			if items, err := listExternalSkills(s.SkillsDir); err == nil {
				s.SkillCount = len(items) // 计数与导入同一口径（都跟随链接）
			}
		}
	}
	return status
}

// ImportExternalSkills 把 ~/.codex/skills 和 ~/.claude/skills 下的直接子目录
// 导入到 target。按 Codex → Claude 的顺序处理，因此两边同名时 Claude Code
// 最后写入并覆盖 Codex 版本。每个技能先复制到临时目录，再替换目标目录，
// 避免复制失败时留下半份技能。
//
// 符号链接技能（来源是链接）：目标目录里重建**同一条链接**而不是复制内容 ——
// 单一来源，改源即两边生效（用户口径；断链则技能失效，与来源一样）。
func ImportExternalSkills(home, target string) (ExternalSkillsImportReport, error) {
	status := DetectExternalSkills(home)
	report := ExternalSkillsImportReport{
		Status:   status,
		Imported: make([]ExternalSkillImport, 0),
	}
	if !status.Enabled {
		return report, errors.New("未检测到 ~/.codex 或 ~/.claude 目录")
	}
	if target == "" {
		return report, errors.New("技能目标目录为空")
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return report, fmt.Errorf("解析技能目标目录: %w", err)
	}
	for _, source := range status.Sources {
		// 导入时会在 target 下创建/替换目录；如果 target 位于来源树内，
		// 复制过程中可能一边读取来源一边改写来源，结果不确定且可能丢数据。
		if pathWithin(source.Root, targetAbs) {
			return report, fmt.Errorf("技能目标目录不能位于来源目录内: %s", source.Root)
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return report, fmt.Errorf("创建技能目标目录: %w", err)
	}

	for _, source := range status.Sources {
		if !source.SkillsDirExists {
			continue
		}
		items, err := listExternalSkills(source.SkillsDir)
		if err != nil {
			return report, fmt.Errorf("读取 %s: %w", source.SkillsDir, err)
		}
		for _, sk := range items {
			name := sk.name
			dst := filepath.Join(target, name)
			rec := ExternalSkillImport{Source: source.Name, Name: name, Target: dst}
			switch {
			case sk.link == "":
				// 技能规范是「一个目录包含 SKILL.md」：真实目录整棵复制。
				if err := replaceSkillDir(sk.path, dst); err != nil {
					return report, fmt.Errorf("导入 %s/%s: %w", source.Name, name, err)
				}
			case samePath(sk.link, dst):
				// 链接指向的正是目标位置本身（如 .claude/skills/x → .agents/skills/x）：
				// 技能已在位，再「替换」会把自己的来源挪走 —— 跳过，不碰。
				continue
			case sameLink(dst, sk.link):
				continue // 已经是同一条链接（重复导入）：不做无谓的备份/替换
			default:
				linked, err := replaceSkillLink(sk.link, dst)
				if err != nil {
					return report, fmt.Errorf("导入 %s/%s: %w", source.Name, name, err)
				}
				if linked {
					rec.LinkTarget = sk.link
				}
			}
			report.Imported = append(report.Imported, rec)
		}
	}
	return report, nil
}

// samePath 两个路径是否指向同一位置。优先解析符号链接后再比（`/tmp` →
// `/private/tmp` 这类系统别名、目录链接都要看穿——自指链接防护依赖它），
// 路径不存在等解析失败时退回 Abs + Clean 比较。
func samePath(a, b string) bool {
	return cleanedPath(a) == cleanedPath(b)
}

// cleanedPath 路径的规范化比较形态（解析链接 → 绝对 + Clean）。
func cleanedPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// sameLink 目标处是否已是解析到 linkTarget 的符号链接（幂等判定）。
func sameLink(dst, linkTarget string) bool {
	fi, err := os.Lstat(dst)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	real, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return false
	}
	return samePath(real, linkTarget)
}

// pathWithin child 是否位于 parent 之内（用于「目标目录不能落在来源树里」的护栏）。
// 两侧都先解析符号链接：目标可以是链接（用户在别处建好 ~/.agents/skills → 某处），
// 不解析就会漏判「其实写在来源树里」。
func pathWithin(parent, child string) bool {
	parentAbs := cleanedPath(parent)
	childAbs := cleanedPath(child)
	rel, err := filepath.Rel(parentAbs, childAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func externalSkillSources(home string) []ExternalSkillSource {
	return []ExternalSkillSource{
		{Name: "codex", Root: filepath.Join(home, ".codex"), SkillsDir: filepath.Join(home, ".codex", "skills")},
		{Name: "claude", Root: filepath.Join(home, ".claude"), SkillsDir: filepath.Join(home, ".claude", "skills")},
	}
}

// replaceSkillDir 准备临时副本后替换 dst。备份路径与目标同父目录，保证 Rename
// 不跨文件系统；替换失败时尽力恢复旧目录。
func replaceSkillDir(src, dst string) error {
	return replaceSkillDirWithRename(src, dst, os.Rename)
}

// replaceSkillDirWithRename 是 replaceSkillDir 的可测试实现。rename 参数只用于
// 验证目标替换失败后的恢复路径；生产调用始终传入 os.Rename。
func replaceSkillDirWithRename(src, dst string, rename func(string, string) error) error {
	tmp, err := stageSkillDir(src, dst)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // 替换成功后 tmp 已被 rename 走，这里是失败路径的清理
	return swapIntoPlace(dst, tmp, rename)
}

// stageSkillDir 在 dst 同父目录下备好技能内容副本，返回暂存路径（调用方负责清理）。
func stageSkillDir(src, dst string) (string, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".go-code-skill-import-")
	if err != nil {
		return "", err
	}
	if err := copySkillTree(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// replaceSkillLink 在目标处重建指向 linkTarget 的符号链接（同一条链接语义）。
// 返回 linked=false 表示平台不允许创建链接（如 Windows 未开开发者模式），
// 已退化为**解引用复制**：技能仍可用（内容是副本），只是不再与来源同步。
func replaceSkillLink(linkTarget, dst string) (bool, error) {
	staging, err := os.MkdirTemp(filepath.Dir(dst), ".go-code-skill-link-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(staging)
	// MkdirTemp 建的是目录，而链接需要一个不存在的路径。
	if err := os.Remove(staging); err != nil {
		return false, err
	}
	if err := os.Symlink(linkTarget, staging); err != nil {
		return false, replaceSkillDir(linkTarget, dst)
	}
	return true, swapIntoPlace(dst, staging, os.Rename)
}

// swapIntoPlace 用已备好的 staged 对象替换 dst：旧对象先移到同父目录备份，
// 替换失败时回滚（staged 与备份都在 dst 同父目录，Rename 不跨文件系统）。
// 无论后续哪一步失败都清理临时备份，避免反复导入留下垃圾。
func swapIntoPlace(dst, staged string, rename func(string, string) error) error {
	parent := filepath.Dir(dst)
	backup, err := os.MkdirTemp(parent, ".go-code-skill-backup-")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	defer os.RemoveAll(backup)

	oldExists := false
	if _, err := os.Lstat(dst); err == nil {
		oldExists = true
		if err := rename(dst, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := rename(staged, dst); err != nil {
		if oldExists {
			if restoreErr := rename(backup, dst); restoreErr != nil {
				return fmt.Errorf("%w（恢复旧技能目录失败: %v）", err, restoreErr)
			}
		}
		return err
	}
	return nil
}

// copySkillTree 复制技能目录树到 dst（目录）：
//   - 真实目录 / 普通文件：照抄（权限取常规位，限制为 owner/group/other）；
//   - 符号链接：按解析后的绝对路径**重建同一条链接**（与顶层符号链接技能同一
//     语义：单一来源、改源即两边生效）。相对链接必须重新锚定——照抄相对路径会在
//     目标位置解析到别处甚至断链；断链跳过（来源那边本来也取不到内容）；
//   - 其他类型（fifo/socket/设备）：跳过。
func copySkillTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(s)
			if err != nil {
				continue
			}
			if err := os.Symlink(real, d); err == nil {
				continue
			}
			// 平台不允许创建链接（Windows 未开开发者模式等）→ 退化为内容复制，
			// 导入结果仍可用，只是该条不再与来源同步。
			if err := copySkillPath(real, d); err != nil {
				return err
			}
			continue
		}
		if err := copySkillPath(s, d); err != nil {
			return err
		}
	}
	return nil
}

// copySkillPath 复制单个条目（目录递归 / 常规文件 / 其余跳过）。
func copySkillPath(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return nil // 取不到（权限/断链）：跳过而不是整次导入失败
	}
	switch {
	case fi.IsDir():
		return copySkillTree(src, dst)
	case fi.Mode().IsRegular():
		return copySkillFile(src, dst, fi.Mode().Perm())
	default:
		return nil // 特殊文件类型：技能用不到，不复制
	}
}

// copySkillFile 复制单个常规文件（内容 + 常规权限位）。
func copySkillFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	return copyErr
}
