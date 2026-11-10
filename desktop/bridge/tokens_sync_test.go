package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 手机端样式必须与桌面端同源（docs/mobile-remote/TECH_PLAN.md §3.8）。
//
// 手机页面由 bridge 用 go:embed 托管，拿不到 Vite 里的 index.css，所以 token
// 是**逐字复制**的。这个测试防两件事：
//  1. 手机端偷偷引入桌面没有的颜色（另起色板）；
//  2. 桌面端调色后手机端不跟（视觉分叉）。
//
// 只校验「手机端出现的十六进制颜色都能在桌面端找到」——反向不要求（桌面有分类色
// 手机用不到）。字体同理。
func TestMobileTokensMatchDesktop(t *testing.T) {
	// 桌面调色板不只在 index.css：sigil v2 的 8 个色相/状态色由 Hub 自己持有
	// （hub.css 的 --sigil-* 与 avatar.ts 的 HUES 表），手机 avatar_model.js
	// 是从这两处逐字复制的。取样必须覆盖它们，否则会把「同源复制」误判成
	// 「手机另起色板」。本意不变：手机出现的颜色必须能在桌面端找到。
	desktopPaths := []string{
		"../app/src/index.css",
		"../app/src/hub/hub.css",
		"../app/src/hub/avatar.ts",
	}
	const mobilePath = "mobile_web/tokens.css"

	desktop, err := os.ReadFile(desktopPaths[0])
	if err != nil {
		t.Fatalf("读桌面样式失败: %v", err)
	}
	palette := string(desktop)
	for _, f := range desktopPaths[1:] {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读桌面调色板 %s 失败: %v", f, err)
		}
		palette += "\n" + string(b)
	}
	mobile, err := os.ReadFile(mobilePath)
	if err != nil {
		t.Fatalf("读手机 token 失败: %v", err)
	}

	hexRe := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	desktopSet := map[string]bool{}
	for _, c := range hexRe.FindAllString(palette, -1) {
		desktopSet[strings.ToLower(c)] = true
	}
	// P4-03c：不只 token 层，主题色可能出现在 index.html 的 theme-color、
	// app.js/hub_model.js 的主题常量里 —— 这些也必须是桌面已有的颜色。
	for _, f := range []string{
		"mobile_web/tokens.css",
		"mobile_web/style.css",
		"mobile_web/app.js",
		"mobile_web/hub_model.js",
		"mobile_web/avatar_model.js",
		"mobile_web/index.html",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读手机文件 %s 失败: %v", f, err)
		}
		for _, c := range hexRe.FindAllString(string(b), -1) {
			if !desktopSet[strings.ToLower(c)] {
				t.Errorf("%s 的颜色 %s 在桌面端不存在（不要另起色板）", f, c)
			}
		}
	}

	index, err := os.ReadFile("mobile_web/index.html")
	if err != nil {
		t.Fatalf("读手机 index.html 失败: %v", err)
	}
	if !strings.Contains(string(index), `name="theme-color"`) {
		t.Error("手机页面缺少 meta theme-color（P4-03c 主题切换要求）")
	}

	for _, font := range []string{"IBM Plex Sans", "IBM Plex Mono"} {
		if !strings.Contains(string(mobile), font) {
			t.Errorf("手机端缺少字体 %s", font)
		}
		if !strings.Contains(string(desktop), font) {
			t.Errorf("桌面端缺少字体 %s", font)
		}
	}

	// 圆角是桌面组件语言的一部分，两边必须一致
	if !strings.Contains(string(mobile), "--radius: 8px") || !strings.Contains(string(desktop), "--radius: 8px") {
		t.Error("--radius 应为 8px（桌面与手机一致）")
	}

	// 字体文件必须在 embed 目录里（否则手机上回落系统字体，视觉立刻分叉）
	for _, f := range []string{
		"mobile_web/fonts/ibm-plex-sans-latin.woff2",
		"mobile_web/fonts/ibm-plex-mono-400.woff2",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("缺少随包字体 %s: %v", f, err)
		}
	}
}
