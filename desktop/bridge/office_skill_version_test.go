package main

// 技能内容「版本纪律」守卫（2026-09-18 用户拍板 frontmatter `version` 机制的配套测试）。
//
// 机制本身在 office_skill_sync.go：内置技能 version > 用户机上的 version → 覆盖，
// 否则保留（用户把 version 改大即可永久钉住自己的副本）。而这个机制的**唯一失效方式**
// 恰好是静默的：
//
//	内容改了、version 没改 → 用户永远收不到这次修订（sync 一看版本相同就跳过）
//
// 这正是 TECH_PLAN §3.3.1 记录的那两次事故的形态（模块清单过时 / pptx dump_deck 排序
// 缺陷修好了也发不出去）。所以这里把「当前内容指纹 ↔ version」钉成一张表：
//
//	凡修订技能内容，必须同时做两件事：
//	  ① 递增 SKILL.md frontmatter 的 version
//	  ② 更新本表的 version 与 sha256
//	少做任何一件，本测试就红 —— 失败信息会直接告诉维护者该做什么。
//
// 为什么用「内容哈希」而不是"只断言 version 存在"：后者挡不住"内容改了但忘了递增"。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

// officeSkillVersionBaseline 随包四个技能当前发布的 (version, SKILL.md 内容 sha256)。
var officeSkillVersionBaseline = map[string]struct{ Version, SHA256 string }{
	"docx": {"1.0.0", "583e3039d1d39163ef92ae8701b3897c7f311d736f963ed59e7a3dd9a905c740"},
	"pdf":  {"1.0.0", "413adc4beb03fa7b52ce47127a6fe76c85525f9795a185099188abbca68a87b5"},
	"pptx": {"1.0.0", "c46f7b35ac4b39f7bb4ec9ce737d377cbcc8fb4bacee4c39deaa7b6fdfb699a6"},
	"xlsx": {"1.0.0", "acd3c5f8f601d3eb9965193c7e5506b05b58dd3cf0d747f2df01a7a111843ea3"},
}

func TestOfficeSkillVersionMatchesContentFingerprint(t *testing.T) {
	for name, want := range officeSkillVersionBaseline {
		dir := filepath.Join(officeSkillsSourceDir, name)
		raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			t.Fatalf("%s: 读不到 SKILL.md: %v", name, err)
		}
		s, err := skills.ParseSkill(dir)
		if err != nil {
			t.Fatalf("%s: 解析失败（frontmatter 坏了）: %v", name, err)
		}
		if s.Version != want.Version {
			t.Errorf("%s: frontmatter version = %q，基线表记录 %q。\n"+
				"若你**有意**修订该技能：请把新的 version 与 sha256 一起更新到 officeSkillVersionBaseline —— "+
				"注意 version 必须**递增**，否则已装用户永远收不到这次修订（sync 会因版本相同而跳过）。",
				name, s.Version, want.Version)
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if got != want.SHA256 {
			t.Errorf("%s: SKILL.md 内容指纹变了（%s → %s）。\n"+
				"有意修订 → 递增 frontmatter version 并更新 officeSkillVersionBaseline（version 与 sha256 一起改）；\n"+
				"无意改动 → 回退内容。两者都不做的话，用户机上的副本不会被更新。",
				name, want.SHA256[:12], got[:12])
		}
	}
	// 反查：源目录里出现基线表没有的技能 → 新技能也必须走同一条纪律（否则它会以"无版本"发出去，
	// 而 shouldUpdateSkill 对无版本的内置技能一律不覆盖 = 永远更新不了）。
	entries, err := os.ReadDir(officeSkillsSourceDir)
	if err != nil {
		t.Fatalf("读不到源目录 %s: %v", officeSkillsSourceDir, err)
	}
	var found []string
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(officeSkillsSourceDir, e.Name())); err == nil && fi.IsDir() {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	for _, name := range found {
		if _, ok := officeSkillVersionBaseline[name]; !ok {
			t.Errorf("源目录里有新技能 %q 但基线表没有它 —— 请给它加 frontmatter version 并登记到 officeSkillVersionBaseline", name)
		}
	}
}
