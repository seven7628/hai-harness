// surface_skill_content_test.go —— 随仓的 Surface 技能与模板的内容守卫（PR-14 E）。
//
// 三块内容都是「文档即代码」：`skills/hai-surface-dev/SKILL.md` 决定模型怎么写插件，
// `templates/surface/*` 决定用户拿到的第一份插件长什么样。它们写坏了不会有编译错误，
// 症状是「模型按错的口径写了一版插件，装上才发现面板空白」。所以守四件事：
//
//  1. 技能能被**真注册表**解析、frontmatter 有 name/version/description、`requires.tools`
//     恰好能挡住没有插件工具的环境（能力面过滤靠它，见 skills/capability.go）；
//  2. 模板能被**真安装器**接受（surfaceStore.Install）—— 模板就是用户第一次装的那个包，
//     它装不上等于文档给了个死例子；
//  3. 模板的 manifest 与面板代码遵守硬约束（kind = plugin:<id>、无 JSX、不 import 包）；
//  4. 技能正文写清了开启开关的方式（`features.surfaceTools`，缺省 false）。
//
// 路径相对 desktop/bridge（go test 的 cwd 是包目录）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

const (
	// 技能的**父目录**：Registry 扫的是「下面每个子目录一份 SKILL.md」的集合目录
	//（与 desktop/vendor/visual-skills 同一个形状）。仓库根的 skills/ 同时也是 Go 包，
	// 扫描会跳过没有 SKILL.md 的目录，两者互不影响。
	surfaceSkillRoot    = "../../skills"
	surfaceSkillName    = "hai-surface-dev"
	surfaceTemplatesDir = "../../templates/surface"
)

func TestSurfaceSkillLoadable(t *testing.T) {
	reg, err := skills.NewRegistry(surfaceSkillRoot)
	if err != nil {
		t.Fatalf("加载 %s 失败: %v", surfaceSkillRoot, err)
	}
	s, err := reg.Skill(surfaceSkillName)
	if err != nil {
		t.Fatalf("技能 hai-surface-dev 无法加载: %v", err)
	}
	if strings.TrimSpace(s.Version) == "" {
		t.Fatal("缺 frontmatter version（随包技能按 version 决定是否覆盖已装用户）")
	}
	if n := len([]rune(s.Description)); n < 80 || n > 300 {
		t.Fatalf("description %d 字，want 80–300（发现阶段只注入这一句）:\n%q", n, s.Description)
	}
	// requires.tools：没有这两个工具的环境不该看到这个技能（它会教模型调不存在的工具）。
	// 这里是**规范化的能力集**（skills.Capabilities），不是原始字符串比对。
	// 两向断言：有这两个工具 → 可用；只有其一 → 明确报缺另一个。
	// （正向只证明「能加载」，反向才证明 requires 真的被解析了 —— YAML 写坏时
	// frontmatter 会静默回退 lenient 模式并丢掉 requires，见 skills/skill_test.go 的既有教训。）
	full := skills.NewCapabilities("m", []string{"text"}, []string{"ui_inspect", "surface_install"})
	if missing := full.Missing(s.Requires); len(missing) != 0 {
		t.Fatalf("requires.tools 在这两个工具都在时仍报缺 %v —— 技能进不了任何有插件工具的会话", missing)
	}
	partial := skills.NewCapabilities("m", []string{"text"}, []string{"ui_inspect"})
	if missing := partial.Missing(s.Requires); len(missing) == 0 {
		t.Fatalf("没有 surface_install 时技能仍可用 —— requires.tools 没被解析（YAML 写坏了？）: %+v", s.Requires)
	}
	if strings.TrimSpace(s.Instructions) == "" {
		t.Fatal("技能正文为空")
	}
	body := s.Instructions
	for _, must := range []string{"features", "surfaceTools", "createElement", "plugin:"} {
		if !strings.Contains(body, must) {
			t.Fatalf("技能正文没提 %q —— 这三件事（开关 / 无 JSX / kind 前缀）是装不上的前三位原因:\n%s",
				must, body)
		}
	}
}

// TestSurfaceTemplateInstalls 模板必须能被生产安装器接受：
// 「文档里的例子装不上」比没有模板更糟（用户会以为是应用坏了）。
func TestSurfaceTemplateInstalls(t *testing.T) {
	m, _ := testManager(t)
	abs, err := filepath.Abs(surfaceTemplatesDir)
	if err != nil {
		t.Fatalf("解析模板绝对路径失败: %v", err)
	}
	receipt := m.surfaces.Install(abs, true)
	if receipt.Status != surfaceApplied {
		t.Fatalf("模板安装失败：status=%s reason=%s", receipt.Status, receipt.Reason)
	}
	// 同 id 再装一次 = already_installed（幂等），不是 rejected。
	if again := m.surfaces.Install(abs, true); again.Status != surfaceAlreadyInstalled {
		t.Fatalf("重复安装模板得到 %s，want %s", again.Status, surfaceAlreadyInstalled)
	}
	mf, err := m.surfaces.Manifest(receipt.ID)
	if err != nil {
		t.Fatalf("读回模板 manifest 失败: %v", err)
	}
	if want := "plugin:" + mf.ID; mf.Kind != want {
		t.Fatalf("模板 kind = %q，want %q（loader 会拒绝加载）", mf.Kind, want)
	}
	if !surfaceIDPattern.MatchString(mf.ID) {
		t.Fatalf("模板 id %q 不匹配 %s", mf.ID, surfaceIDPattern)
	}
	if strings.TrimSpace(mf.Version) == "" {
		t.Fatal("模板 manifest 缺 version")
	}
	// icon/entry 必须真的在模板目录里（否则 index 里会带一个取不到的 URL）。
	for _, rel := range []string{mf.Entry, mf.Icon} {
		if rel == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(surfaceTemplatesDir, rel)); err != nil {
			t.Fatalf("manifest 指向的 %q 不在模板目录里: %v", rel, err)
		}
	}
	// 声明的每条命令都必须在**宿主真正放行**的集合里：模板教人声明的命令调不通，等于教错。
	//
	// D11 之后 surface_command 的宿主表是空的，放行集合只剩 surface_agent 的四个 name
	// （agent.session / agent.run / agent.cancel / tool.result）。因此「一条都不声明」是
	// 合法的（模板眼下就是这么走的：P1b2 的 claimed API 落地之前，任何 agent.* 调用都
	// 跑不通，示例调一条注定 denied 的命令比不调更误导）—— 断言从「必须声明」改成
	// 「声明了就必须能调通」，这条不变式才是模板真正要守的东西。
	for _, name := range mf.Commands {
		if !surfaceAgentHostCommands[name] {
			if _, legacy := surfaceHostCommands[name]; !legacy {
				t.Fatalf("模板声明了宿主不放行的命令 %q（面板会拿到 denied）—— 放行集合只有 %v",
					name, surfaceOpenCommandNames())
			}
		}
	}
	// 面板代码里**实际调用**的名字同样要在放行集合里：manifest 声明得再对，代码里调一条
	// 别的命令照样是教错（这一条替代了原来"必须有 commands"的存在性断言）。
	for _, name := range panelCommandCalls(t, surfaceTemplatesDir) {
		if !surfaceAgentHostCommands[name] {
			if _, legacy := surfaceHostCommands[name]; !legacy {
				t.Fatalf("模板 panel.js 调用了宿主不放行的命令 %q（会拿到 denied）", name)
			}
		}
	}
}

// surfaceOpenCommandNames 当前真正放行的命令名（agent.* ∪ 只读表），只给失败文案用。
func surfaceOpenCommandNames() []string {
	out := make([]string, 0, len(surfaceAgentHostCommands)+len(surfaceHostCommands))
	for name := range surfaceAgentHostCommands {
		out = append(out, name)
	}
	for name := range surfaceHostCommands {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// panelCommandCalls 从面板源码里抠出 `host.command('x', …)` / `.command("x", …)` 的命令名。
//
// 用正则而不是解析 JS：模板是「单文件、无构建、只调一个宿主 API」的最小样本，
// 正则足够，且不会因为模板里出现 React.createElement 的复杂表达式而误判。
func panelCommandCalls(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "panel.js"))
	if err != nil {
		t.Fatalf("读模板 panel.js 失败: %v", err)
	}
	re := regexp.MustCompile(`\.command\(\s*['"]([^'"]+)['"]`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, m[1])
	}
	return out
}

// TestSurfaceTemplatePanelIsBrowserESM 模板的面板代码必须能被浏览器直接 import。
func TestSurfaceTemplatePanelIsBrowserESM(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(surfaceTemplatesDir, "panel.js"))
	if err != nil {
		t.Fatalf("读模板 panel.js 失败: %v", err)
	}
	src := string(raw)
	var jsxRe = regexp.MustCompile(`</[A-Za-z]`)
	if jsxRe.MatchString(src) {
		t.Fatal("模板 panel.js 里出现了 JSX 闭合标签 —— 面板不经构建，浏览器会直接语法错")
	}
	if strings.Contains(src, "<div") || strings.Contains(src, "<span") {
		t.Fatal("模板 panel.js 里出现了 JSX 元素字面量 —— 必须用 React.createElement")
	}
	if !strings.Contains(src, "createElement") {
		t.Fatal("模板 panel.js 没有 createElement —— 无构建步骤时这是唯一建元素的路径")
	}
	// 裸模块名在浏览器里没有解析器：出现 `import ... from 'x'` 就是装得上、跑不起来。
	if regexp.MustCompile(`(?m)^\s*import\s[^'"]*['"][^./][^'"]*['"]`).MatchString(src) {
		t.Fatal("模板 panel.js 有裸模块 import（浏览器无解析器，import 会失败）")
	}
	if !strings.Contains(src, "export const surface") {
		t.Fatal("模板 panel.js 没有 `export const surface`")
	}
	// 图标：SVG 根元素（标签条按 12×12 渲染它）。
	svg, err := os.ReadFile(filepath.Join(surfaceTemplatesDir, "icon.svg"))
	if err != nil {
		t.Fatalf("读模板 icon.svg 失败: %v", err)
	}
	if !strings.Contains(string(svg), "<svg") {
		t.Fatal("模板 icon.svg 不是 SVG")
	}
}

// TestSurfaceTemplateManifestKindMatchesID 模板自己的 kind 必须与 id 一致（loader 的硬校验）。
func TestSurfaceTemplateManifestKindMatchesID(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(surfaceTemplatesDir, "surface.json"))
	if err != nil {
		t.Fatalf("读模板 surface.json 失败: %v", err)
	}
	var mf surfaceManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("模板 surface.json 不是合法 JSON: %v", err)
	}
	if mf.Kind != "plugin:"+mf.ID {
		t.Fatalf("模板 kind = %q，want %q", mf.Kind, "plugin:"+mf.ID)
	}
	// 面板代码里的 kind 必须与 manifest 同字（两处不一致 = 加载时被拒）。
	panel, err := os.ReadFile(filepath.Join(surfaceTemplatesDir, "panel.js"))
	if err != nil {
		t.Fatalf("读模板 panel.js 失败: %v", err)
	}
	if !strings.Contains(string(panel), "kind: 'plugin:"+mf.ID+"'") {
		t.Fatalf("模板 panel.js 的 kind 与 manifest（plugin:%s）不一致", mf.ID)
	}
}
