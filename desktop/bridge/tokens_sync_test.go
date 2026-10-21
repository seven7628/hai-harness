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
	const desktopPath = "../app/src/index.css"
	const mobilePath = "mobile_web/tokens.css"

	desktop, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("读桌面样式失败: %v", err)
	}
	mobile, err := os.ReadFile(mobilePath)
	if err != nil {
		t.Fatalf("读手机 token 失败: %v", err)
	}

	hexRe := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	desktopSet := map[string]bool{}
	for _, c := range hexRe.FindAllString(string(desktop), -1) {
		desktopSet[strings.ToLower(c)] = true
	}
	for _, c := range hexRe.FindAllString(string(mobile), -1) {
		if !desktopSet[strings.ToLower(c)] {
			t.Errorf("手机端颜色 %s 在桌面端不存在（不要另起色板）", c)
		}
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
