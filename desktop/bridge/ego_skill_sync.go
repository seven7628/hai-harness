package main

// ego-browser skill 随包分发同步（2026-09）：
//
// 问题：ego-browser 技能的发现依赖 `~/.agents/skills/ego-browser`，而该目录在
// 本机由 ego lite onboarding 以 symlink 建立（→ app Resources/ego-skills）。
// 未装 ego lite / 未 onboarding 的机器上没有它 → 打包版 HAI 内置的
// Resources/ego-browser-skill 永远不被技能注册表发现（对话 skills 看不到）。
//
// 方案：对齐 browser/computer 插件的 vendor 同步——Electron main spawn bridge 时
// env GO_CODE_EGO_SKILL_DIR 传内置 skill 源目录；bridge 启动同步到
// `~/.agents/skills/ego-browser`（不存在才复制；已存在（symlink 或真实目录）跳过——
// 尊重 ego onboarding 的 symlink 与用户已有版本）。

import (
	"fmt"
	"os"
	"path/filepath"
)

// egoSkillDirEnv bridge 环境变量：内置 ego-browser skill 源目录（Electron main 注入）。
const egoSkillDirEnv = "GO_CODE_EGO_SKILL_DIR"

// ensureEgoSkillSynced 启动同步内置 ego-browser skill 到全局技能目录：
//   - 目标已存在（~/.agents/skills/ego-browser，含 symlink/真实目录）→ 跳过（幂等，
//     尊重 ego onboarding symlink 与用户版本）；
//   - 目标不存在且源目录有效 → 复制（别人机器/未装 ego lite 也能发现技能）。
//
// 失败仅记日志不阻断启动（技能面板缺 ego-browser，其余技能不受影响）。
func ensureEgoSkillSynced() error {
	src := os.Getenv(egoSkillDirEnv)
	if src == "" {
		return nil // dev 无 env 时跳过（用户已装 ego lite 场景由 symlink 覆盖）
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("home 解析失败: %v", err)
	}
	dst := filepath.Join(home, ".agents", "skills", "ego-browser")
	// 目标已存在（真实目录或 symlink）→ 不动（幂等 + 尊重既有来源）
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	// 源必须是完整 skill（SKILL.md）
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return fmt.Errorf("内置 ego-browser skill 源不完整（%s 缺 SKILL.md）: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyDirTree(src, dst); err != nil {
		return fmt.Errorf("同步 ego-browser skill → %s: %w", dst, err)
	}
	fmt.Fprintf(os.Stderr, "· ego-browser skill 已从内置资源同步到 %s\n", dst)
	return nil
}

// copyDirTree 递归复制目录树到 dst：跟随符号链接，把链接目标的内容复制成真实
// 文件（源是应用内置资源目录，副本必须自包含——照抄链接会指向 bundle 内部路径，
// 换版本/换机器即断链）。断链跳过；目录/普通文件之外的条目跳过；常规权限位保留
// （scripts/*.sh 的可执行位不能被抹掉）。链接环（链接指回祖先）报错，不无限递归。
func copyDirTree(src, dst string) error {
	return copyDirTreeAt(src, dst, 0, map[string]bool{})
}

// maxCopyDepth 复制层级上限：环检测（seen）之外再兜一层，防御解析不到真实路径的怪情形。
const maxCopyDepth = 64

// copyDirTreeAt seen = 当前这条递归路径上已展开目录的真实路径（跟随链接后）：
// 链接指回祖先时立即报错——不能靠路径长度/内核链接上限兜底，那只会静默少复制内容。
// 出栈即释放，兄弟分支各自展开（同一个目录被两处链接共用是合法的）。
func copyDirTreeAt(src, dst string, depth int, seen map[string]bool) error {
	if depth > maxCopyDepth {
		return fmt.Errorf("目录层级超过 %d 层（疑似符号链接环）: %s", maxCopyDepth, src)
	}
	real := src
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		real = resolved
	}
	if seen[real] {
		return fmt.Errorf("符号链接环：%s 指回已展开目录 %s", src, real)
	}
	seen[real] = true
	defer delete(seen, real)

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		// 跟随链接判定类型：源侧目录链接（vendor 里指向单一副本）也要复制内容。
		// DirEntry.IsDir() 对 symlink 恒 false，会把链接子目录当文件读 → EISDIR 失败。
		fi, err := os.Stat(s)
		if err != nil {
			continue // 断链：来源那边本来也取不到内容
		}
		if fi.IsDir() {
			if err := copyDirTreeAt(s, d, depth+1, seen); err != nil {
				return err
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			continue // 特殊文件类型：技能目录用不到
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
