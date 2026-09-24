package main

// 办公技能随包分发同步（2026-09）：
//
// 问题：docx/xlsx/pdf 三个办公技能此前只活在工作区 `.agents/skills/`——技能发现虽然
// 有全局层，但那三个目录从没被投放到全局层，于是换个 workspace 就看不到，打包给
// 用户也不会随包带走（分发缺口）。
//
// 方案：对齐 ego-browser skill 的分发范式（ego_skill_sync.go）——Electron main spawn
// bridge 时以 env GO_CODE_OFFICE_SKILLS_DIR 传内置技能源目录（dev = 仓库
// desktop/vendor/office-skills；打包 = Resources/office-skills）；bridge 启动把源目录
// 下的每个技能子目录同步到 `~/.agents/skills/<name>`（全局层，跨工作区）。
// 目标是让打包版用户开箱就能 load_skill(docx|xlsx|pdf)，不依赖仓库工作区布局。
//
// **内容更新机制（2026-09-18 用户拍板：frontmatter `version`）**：早期实现是「目标存在即跳过」，
// 后果是**技能内容再也到不了已装用户**——实测已咬到两次：① 运行时加了 `pypdfium2`/`mammoth` 后
// 技能里的模块清单过时；② pptx 技能的 `dump_deck` 排序缺陷修好了也发不出去（技能内容就是代码，
// 会带 bug 发布）。现改为按 SKILL.md 的 `version` 比较：
//
//	装了但没有 version（历史副本）→ 视为更旧 → 覆盖（否则老用户永远停在首装那天）
//	装了 < 内置                      → 覆盖
//	装了 >= 内置                     → 跳过（用户想永久保留自己的改法：把 version 改大即可）
//	内置读不出 version               → 跳过（不拿来源不明的内容覆盖用户副本；内容测试守这一条）
//	目标是 symlink / 非目录          → 一律跳过（用户的显式选择，不动，也不跟随写入）
//
// 覆盖采取「合并式复制」（copyDirTree）：办公技能目前只有 SKILL.md，不需要删除语义；
// 将来若技能带上 scripts/ 且需要**删文件**，这里要改成"先清空目标再复制"。
//
// 为什么一个 env 传「技能集合目录」而不是每个技能一个 env：新增技能只需往源目录加
// 子目录，Go 与 TS 两侧接线都不用动（技能名不落进代码）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seven7628/hai-harness/skills"
)

// officeSkillsDirEnv bridge 环境变量：内置办公技能集合目录（Electron main 注入）。
// 该目录下每个子目录 = 一个技能（docx/ xlsx/ pdf/ …）。
const officeSkillsDirEnv = "GO_CODE_OFFICE_SKILLS_DIR"

// ensureOfficeSkillsSynced 启动同步内置办公技能到全局技能目录（~/.agents/skills/<name>）：
//   - env 空（dev 无注入）→ no-op；
//   - 每个技能目标已存在（symlink 或真实目录）→ 跳过该技能（幂等，尊重用户改过的版本）；
//   - 源子目录缺 SKILL.md → 跳过该技能（源目录里可能混有 README 之类非技能文件）；
//   - 单个技能失败不拖累其余：错误聚合后返回，调用处只打日志。
//
// 失败不阻断启动（技能面板缺某几个技能，其余技能与全部其他功能不受影响）。
func ensureOfficeSkillsSynced() error {
	src := os.Getenv(officeSkillsDirEnv)
	if src == "" {
		return nil // dev 无 env 时跳过（技能由工作区 .agents/skills 提供）
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fmt.Errorf("home 解析失败: %v", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("内置办公技能源不可读（%s）: %w", src, err)
	}
	dstRoot := filepath.Join(home, ".agents", "skills")
	var errs []error
	for _, e := range entries {
		// 源侧跟随 symlink 判定目录（对齐 skills.Registry 的发现语义）：这样将来若把
		// vendor 里的技能子目录做成指向单一副本的 symlink，复制链路照样成立。
		srcSkill := filepath.Join(src, e.Name())
		if fi, err := os.Stat(srcSkill); err != nil || !fi.IsDir() {
			continue // 非目录（README.md 等）：不是技能，忽略
		}
		name := e.Name()
		dst := filepath.Join(dstRoot, name)
		// 目标已存在：symlink / 非目录 → 一律不动（尊重用户或外部来源；symlink 更不得跟随写入）。
		// 真实目录 → 交给 version 策略决定覆盖还是保留（见文件头「内容更新机制」）。
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				continue
			}
			update, why := shouldUpdateSkill(dst, srcSkill)
			if !update {
				if why != "" {
					fmt.Fprintf(os.Stderr, "· %s skill 保留用户副本（%s）\n", name, why)
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "· %s skill 内置版本更新（%s）→ 覆盖 %s\n", name, why, dst)
		}
		// 源必须是完整 skill（SKILL.md）
		if _, err := os.Stat(filepath.Join(srcSkill, "SKILL.md")); err != nil {
			errs = append(errs, fmt.Errorf("内置技能 %s 源不完整（缺 SKILL.md）: %w", name, err))
			continue // 其余技能继续同步
		}
		if err := os.MkdirAll(dstRoot, 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := copyDirTree(srcSkill, dst); err != nil {
			errs = append(errs, fmt.Errorf("同步技能 %s → %s: %w", name, dst, err))
			continue // 单个技能失败不拖累其余
		}
		fmt.Fprintf(os.Stderr, "· %s skill 已从内置资源同步到 %s\n", name, dst)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// shouldUpdateSkill 决定「目标已存在的技能目录」是否该被内置版覆盖，并返回一句人类可读的理由
// （空理由 = 静默保留，不打印）。策略见文件头；这里只负责比较，不做 IO 之外的判断。
//
// 注意 installed 侧读不出（缺 frontmatter / 坏 YAML / 没有 version）**算更旧**：
// 这正是「老副本要能被修好」的入口（早期同步写下的副本没有 version 字段）。
func shouldUpdateSkill(installedDir, bundledDir string) (bool, string) {
	bundled, err := skills.ParseSkill(bundledDir)
	if err != nil || strings.TrimSpace(bundled.Version) == "" {
		// 内置技能自身版本缺失/损坏：不覆盖用户副本（内容测试 office_skill_content_test.go 守这条）
		return false, ""
	}
	installed, err := skills.ParseSkill(installedDir)
	if err != nil || strings.TrimSpace(installed.Version) == "" {
		return true, "用户副本无 version（历史副本）"
	}
	cmp, ok := compareSkillVersions(installed.Version, bundled.Version)
	if !ok {
		return true, fmt.Sprintf("用户副本 version %q 不可解析", installed.Version)
	}
	if cmp < 0 {
		return true, fmt.Sprintf("%s → %s", installed.Version, bundled.Version)
	}
	if cmp == 0 {
		return false, "" // 同版本：用户可能改过内容，保留
	}
	return false, fmt.Sprintf("用户副本 %s 更新（自管版本，跳过内置 %s）", installed.Version, bundled.Version)
}

// compareSkillVersions 比较「点分数字」版本（"1.2.3"）：a<b → -1，a==b → 0，a>b → 1。
// 任一侧出现非数字段（含空）→ ok=false（调用方按"不可解析"处理，不猜大小）。
// 不引 semver 依赖：技能版本只需要单调递增，分隔符后多余字段按 0 比（"1.2" == "1.2.0"）。
func compareSkillVersions(a, b string) (int, bool) {
	pa, oka := versionParts(a)
	pb, okb := versionParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
	}
	return 0, true
}

// versionParts 拆 "1.2.3" → [1 2 3]；空串/非数字段 → ok=false。
func versionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	segs := strings.Split(v, ".")
	out := make([]int, 0, len(segs))
	for _, s := range segs {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
