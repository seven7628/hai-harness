package main

// 办公技能**真实内容**回归测试（2026-09）：
//
// 为什么需要：office_skill_sync_test.go / office_skill_wiring_test.go 都在 t.TempDir() 里
// **自造**技能源，于是「随包分发的那一份内容」从未被任何测试加载过。而 skills.Registry 对
// 损坏的 SKILL.md 是**静默跳过**（registry.go → parseSkill 出错 continue）：frontmatter 缺
// name/description、YAML 语法错、正文为空，都不会报错，用户侧只表现为「技能不在清单里」，
// CI 全绿。本测试直接加载 desktop/vendor/office-skills（Electron main 在 dev 下注入的正是
// 这个目录），把「四个技能都在、名字对得上、描述够长、正文含运行指引与能力边界」变成红灯
// 条件。
//
// 不做 t.Skip：内容缺失就是红——这正是本测试存在的理由。

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/skills"
)

// officeSkillsSourceDir 随包分发的技能源目录（相对 desktop/bridge——go test 的 cwd 是包目录；
// 与 desktop/app/electron/main.ts dev 分支注入的路径是同一个目录）。
const officeSkillsSourceDir = "../vendor/office-skills"

// devSkillsDir 开发期工作区技能层（相对 desktop/bridge）。工作区层优先级**高于**全局层
// （skills/registry.go），改技能内容时须与 vendor 同步（docs/office/TECH_PLAN.md §3.3）。
const devSkillsDir = "../../.agents/skills"

// wantOfficeSkills 期望随包分发的技能集合：**恰好**这四个，多一个少一个都红。
var wantOfficeSkills = []string{"docx", "pdf", "pptx", "xlsx"}

// minDescriptionLen 描述长度下限：发现阶段只把 name/description 注入系统提示词，
// description 是模型判断「何时该用这个技能」的唯一依据，太短等于没写。
const minDescriptionLen = 80

// TestOfficeSkillContentDirsExactlyMatch 随包技能目录必须恰好是 docx/pdf/pptx/xlsx：
// 少一个（误删目录、漏同步）或多一个（新增技能没同步更新本测试）都立刻红。
func TestOfficeSkillContentDirsExactlyMatch(t *testing.T) {
	got := listOfficeSkillDirs(t)
	want := slices.Clone(wantOfficeSkills)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("随包办公技能目录不匹配（%s）:\n got  %v\n want %v\n"+
			"少一个 = 用户侧少一个技能；多一个 = 需要同步更新本测试的 wantOfficeSkills",
			officeSkillsSourceDir, got, want)
	}
}

// TestOfficeSkillContentEachSkillIsUsable 每个技能都必须真能被注册表**加载**，
// 且元数据足以让模型判断何时该用、正文含运行指引与能力边界。
func TestOfficeSkillContentEachSkillIsUsable(t *testing.T) {
	reg, err := skills.NewRegistry(officeSkillsSourceDir)
	if err != nil {
		t.Fatalf("加载随包技能目录 %s 失败: %v", officeSkillsSourceDir, err)
	}
	// 用 All() 而不是 List()：List() 只返回**启用**技能（本仓库已知坑：看着像「0 个技能」），
	// 这里要看全部技能，所以取宿主视图 All()。
	all := reg.All()
	names := make([]string, 0, len(all))
	for _, d := range all {
		names = append(names, d.Name)
	}
	t.Logf("注册表（%s）解析出 %d 个技能: %v", officeSkillsSourceDir, len(all), names)
	if len(all) != len(wantOfficeSkills) {
		t.Fatalf("注册表只解析出 %d 个技能 %v，期望 %d 个 %v —— "+
			"某个 SKILL.md 的 frontmatter 坏了会被静默跳过（parseSkill 出错 continue）",
			len(all), names, len(wantOfficeSkills), wantOfficeSkills)
	}

	for _, name := range wantOfficeSkills {
		s, err := reg.Load(name)
		if err != nil {
			t.Fatalf("技能 %q 无法加载: %v\n"+
				"（SKILL.md 不存在、frontmatter 缺 name/description、YAML 语法错、正文为空，注册表都会静默跳过该技能）",
				name, err)
		}
		if s.Name != name {
			t.Errorf("技能 %s: frontmatter name = %q，期望与目录名一致（%q）——名字对不上模型就 load_skill 不到",
				name, s.Name, name)
		}
		if n := utf8.RuneCountInString(s.Description); n <= minDescriptionLen {
			t.Errorf("技能 %s: description 只有 %d 个字符（要求 > %d）：%q\n"+
				"description 是模型判断「何时该用」的唯一依据，太短等于没写",
				name, n, minDescriptionLen, s.Description)
		}
		if !strings.Contains(strings.ToLower(s.Description), "use this when the user asks") {
			t.Errorf("技能 %s: description 未按既有范式收尾（缺 \"Use this when the user asks ...\"）：%q\n"+
				"这句话告诉模型什么请求该触发本技能，缺了技能就不会被想起", name, s.Description)
		}
		if strings.TrimSpace(s.Instructions) == "" {
			t.Errorf("技能 %s: SKILL.md 正文为空（注册表要求 instructions 非空）", name)
		}
		if !strings.Contains(s.Instructions, "## How to run this") {
			t.Errorf("技能 %s: 正文缺 \"## How to run this\" 章节 —— 模型拿不到运行方式（run_python、工作区相对路径、已装/未装模块）", name)
		}
		if !strings.Contains(s.Instructions, "## When to tell the user you cannot do it") {
			t.Errorf("技能 %s: 正文缺 \"## When to tell the user you cannot do it\" 章节 —— 没有能力边界，模型会对做不到的事硬编", name)
		}
		t.Logf("技能 %s: description %d 字符 / instructions %d 字符 / 目录 %s",
			name, utf8.RuneCountInString(s.Description), utf8.RuneCountInString(s.Instructions), s.Dir)
	}
}

// TestOfficeSkillContentDevCopyMatchesVendor 开发期副本与随包源必须逐字一致，且工作区层
// 真能加载出这四个技能。
//
// 为什么：工作区层优先级**高于**全局层（skills/registry.go），只改 vendor 不改工作区副本时，
// 开发机上 load_skill 拿到的仍是旧内容——「改了技能却看不出变化」。两处并存是 TECH_PLAN
// §3.3 记录的过渡态（收敛路径是删掉工作区副本，此后统一走全局层同步），所以这里只要求
// 「工作区层存在时两者一致」：目录不存在（已收敛/打包环境）不算失败，但存在却分叉必红。
func TestOfficeSkillContentDevCopyMatchesVendor(t *testing.T) {
	if _, err := os.Stat(devSkillsDir); err != nil {
		t.Logf("工作区技能层 %s 不存在（已按 TECH_PLAN §3.3 收敛，或非仓库环境）→ 无副本可比对", devSkillsDir)
		return
	}
	// 工作区层真能加载（分层注册表的真实路径，不是只看文件在不在）。
	reg, err := skills.NewLayeredRegistry(devSkillsDir)
	if err != nil {
		t.Fatalf("加载工作区技能层 %s 失败: %v", devSkillsDir, err)
	}
	for _, name := range wantOfficeSkills {
		if _, err := reg.Load(name); err != nil {
			t.Errorf("工作区技能层 %s 加载技能 %q 失败: %v", devSkillsDir, name, err)
		}
		vendorPath := filepath.Join(officeSkillsSourceDir, name, "SKILL.md")
		vendor, err := os.ReadFile(vendorPath)
		if err != nil {
			t.Fatalf("随包技能 %s 不可读: %v", vendorPath, err)
		}
		devPath := filepath.Join(devSkillsDir, name, "SKILL.md")
		dev, err := os.ReadFile(devPath)
		if err != nil {
			t.Errorf("工作区技能层缺 %s: %v\n"+
				"（只改 vendor 不改工作区层时，开发机加载的仍是旧内容——两处须同时改）", devPath, err)
			continue
		}
		if !bytes.Equal(dev, vendor) {
			t.Errorf("技能 %s 两处副本不一致（工作区层优先级更高，开发机加载的是它）:\n  vendor: %s\n  dev   : %s\n"+
				"改技能内容时须同时改两处（docs/office/TECH_PLAN.md §3.3）", name, vendorPath, devPath)
		}
	}
}

// TestOfficeSkillContentSyncsToGlobalLayer 随包源里的每个技能（含新增的 pptx）必须能走
// 真实分发链路落到全局层：ensureOfficeSkillsSynced() → ~/.agents/skills/<name> → 注册表可加载。
// 这同时是「新增技能只需加子目录、Go/TS 接线不动」的机器化证明（接线只按目录遍历，
// 见 office_skill_sync.go；docx/xlsx/pdf 三个名字没有写进任何代码）。
func TestOfficeSkillContentSyncsToGlobalLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, officeSkillsSourceDir)

	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("同步随包技能到全局层失败: %v", err)
	}
	globalDir := filepath.Join(home, ".agents", "skills")
	for _, name := range wantOfficeSkills {
		dst := filepath.Join(globalDir, name, "SKILL.md")
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Errorf("技能 %s 未同步到全局层（%s）: %v\n（打包版用户就是这样拿到技能的：源目录每个子目录 = 一个技能）", name, dst, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(officeSkillsSourceDir, name, "SKILL.md"))
		if err != nil {
			t.Fatalf("随包技能 %s 不可读: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("全局层的 %s/SKILL.md 与随包源内容不一致——同步必须是逐字复制", name)
		}
	}
	// 全局层 + 注册表 = 打包版用户的真实加载路径。
	reg, err := skills.NewRegistry(globalDir)
	if err != nil {
		t.Fatalf("加载全局层技能目录 %s 失败: %v", globalDir, err)
	}
	for _, name := range wantOfficeSkills {
		if _, err := reg.Load(name); err != nil {
			t.Errorf("全局层的技能 %q 无法加载: %v", name, err)
		}
	}
}

// listOfficeSkillDirs 列出随包技能源目录下的技能子目录名（升序）。
// 跟随 symlink 判目录：与 skills.Registry、ensureOfficeSkillsSynced 的发现语义一致。
func listOfficeSkillDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(officeSkillsSourceDir)
	if err != nil {
		t.Fatalf("随包技能源目录 %s 不可读: %v", officeSkillsSourceDir, err)
	}
	var dirs []string
	for _, e := range entries {
		fi, err := os.Stat(filepath.Join(officeSkillsSourceDir, e.Name()))
		if err != nil || !fi.IsDir() {
			continue
		}
		dirs = append(dirs, e.Name())
	}
	slices.Sort(dirs)
	return dirs
}
