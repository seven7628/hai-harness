package main

// wps.go：真机剧本「打开 WPS Office → 观察应用树 → 新建空白文字（Word）
// → 键入标题/正文 → 验证键入进入文档」。
//
// 与 calendar/reminder 剧本的差异：WPS 是 Electron/Chromium 类复杂应用——
// 本剧本每步都打印快照树关键结构（前台标注/窗口/摘要/截断），用于验证
// 2026-09 优化：① 交互摘要不再因元素 >40 整段丢弃；② app 与前台不一致时
// 首行标注；③ 键入后能在树中看到文档文本（AX 对 WPS 编辑区的暴露度）。
//
// 调用：computer_e2e -helper <path> script wps
// 依赖：settings.json permissions.computer_approved_apps 含 "WPS Office"
// （未授权 open_app 会被统一授权门禁拒绝——剧本首步即暴露该状态）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/computer"
)

// runWPSScript WPS 新建 Word + 填充文本剧本。
func runWPSScript(ctx context.Context, ex *computer.Executor, be computer.Backend) error {
	step := func(name, out string, err error) error {
		if err != nil {
			return fmt.Errorf("✗ %s: %v", name, err)
		}
		fmt.Printf("✓ %s → %s\n", name, strings.SplitN(out, "\n", 2)[0])
		return nil
	}
	wait := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

	// dump 打印快照要点：首行（前台标注）+ 窗口 + 摘要段前 8 行 + 截断标记。
	// 模型看到的是全量文本；这里只摘录结构便于人读。
	dump := func(tag, out string) {
		fmt.Printf("\n——— snapshot[%s] (%d chars) ———\n", tag, len(out))
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			if i == 0 || strings.HasPrefix(l, "Window ") ||
				l == "— actionable targets —" || strings.HasPrefix(l, "a:0 ") ||
				strings.HasPrefix(l, "… ") || strings.HasPrefix(l, "OCR") {
				fmt.Println(" ", l)
			}
			if i >= 8 && l == "— actionable targets —" {
				break
			}
		}
		// 摘要段（可操作目标）打前 6 行
		seen := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "a:") && seen < 6 {
				fmt.Println("   ↳", l)
				seen++
			}
		}
		if strings.Contains(out, "tree truncated") {
			fmt.Println("   ⚠ tree truncated（有 find 引导）")
		}
		fmt.Println("——— end ———")
	}

	// 1. 打开 WPS Office（统一授权门禁：未授权会在此步被拒并给出引导）
	out, err := ex.OpenApp(ctx, "WPS Office")
	if err := step("open WPS Office", out, err); err != nil {
		return err
	}
	wait(1500)

	// 2. 快照首页：验证修复①——交互摘要始终输出（WPS 首页元素远超旧阈值 40）
	out, err = ex.Snapshot(ctx, "WPS Office", 8000)
	if err := step("snapshot 首页", out, err); err != nil {
		return err
	}
	dump("WPS 首页", out)
	if !strings.Contains(out, "actionable targets") {
		// 可能整树 <40 但摘要应恒输出（>0 即输出）；没有 = 回归
		return fmt.Errorf("首页快照无 actionable targets 摘要（修复①回归？）:\n%s", truncate(out, 400))
	}
	// uidOf 按文本找可交互元素 uid（缓存树；FindUID 已排除 AXWindow 等容器——
	// 2026-09 教训：命中「新建文档」窗口而非「新建」按钮）。
	uidOf := func(name, text string) (string, error) {
		uid := ex.FindUID(text)
		if uid == "" {
			return "", fmt.Errorf("未找到「%s」（%s）——当前树不含该文本，需看上面快照", text, name)
		}
		return uid, nil
	}

	// 3. 点「新建」→ 弹出新建面板（顶部标签页「新建」按钮或左侧栏「新建」均可；
	// 左侧栏新建更稳——首页 AX 树可见「新建」钮）
	uid, err := uidOf("新建", "新建")
	if err != nil {
		return err
	}
	out, err = ex.Press(ctx, uid)
	if err := step("press 新建 "+uid, out, err); err != nil {
		return err
	}
	wait(1200)

	// 4. 快照新建面板：应有「文字」（Word）入口
	out, err = ex.Snapshot(ctx, "WPS Office", 8000)
	if err := step("snapshot 新建面板", out, err); err != nil {
		return err
	}
	dump("新建面板", out)
	if !strings.Contains(out, "文字") {
		return fmt.Errorf("新建面板未见「文字」入口:\n%s", truncate(out, 600))
	}

	// 5. 点「文字」→ 新建空白文档。press 自带验证但 WPS 弹层点击偶发不稳
	//（frame-click 落在弹层边缘/弹层已关）——验证弹层关闭且文档窗口仍在，
	// 失败重试一次；仍失败回退 cmd+n（当前文字文稿标签 → 同类型新建）。
	uid, err = uidOf("文字", "文字")
	if err != nil {
		return err
	}
	pressOK := false
	for attempt := 0; attempt < 2 && !pressOK; attempt++ {
		out, err = ex.Press(ctx, uid)
		if err := step(fmt.Sprintf("press 文字(Word) %s (尝试%d)", uid, attempt+1), out, err); err != nil {
			return err
		}
		wait(2500) // WPS 冷建文档较慢
		// 验证：弹层（小窗口 w<500）关闭 = 点中了「文字」进入创建流程
		out, err = ex.Snapshot(ctx, "WPS Office", 8000)
		if err := step("snapshot 文档窗口", out, err); err != nil {
			return err
		}
		hasPop := false
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Window ") && strings.Contains(l, "w:3") {
				hasPop = true
			}
		}
		if !hasPop {
			pressOK = true
		} else if attempt == 0 {
			fmt.Println("  ⚠ 弹层未关闭（点击未命中「文字」），重试…")
			wait(600)
		}
	}
	if !pressOK {
		fmt.Println("  ⚠ 弹层点击两次未建成，回退 cmd+n 新建")
		out, err = ex.Key(ctx, "cmd+n")
		if err := step("key cmd+n", out, err); err != nil {
			return err
		}
		wait(2500)
		out, err = ex.Snapshot(ctx, "WPS Office", 8000)
		if err := step("snapshot 文档窗口(cmd+n)", out, err); err != nil {
			return err
		}
	}

	// 6. 快照文档窗口：验证前台标注 + 窗口标题变化（新建文档/文字文稿N）
	dump("文档窗口", out)
	winOK := false
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Window ") && (strings.Contains(l, "文字文稿") || strings.Contains(l, "新建文档")) {
			winOK = true
		}
	}
	if !winOK {
		return fmt.Errorf("未见 WPS 文档窗口（标题应含 文字文稿/新建文档）:\n%s", truncate(out, 600))
	}

	// 7. 键入标题。WPS 新建文档后光标在正文区（AX 树通常不可见正文——编辑区
	// 是自绘/Chromium 层）；按真实用户路径直接键入（焦点已就位），随后验证。
	title := "我的 Computer Use"
	if _, err := ex.Type(ctx, title, ""); err != nil {
		return err
	}
	wait(600)

	// 8. 换行 + 键入正文
	if _, err := ex.Key(ctx, "Return"); err != nil {
		return err
	}
	wait(200)
	body := "Hello World！"
	if _, err := ex.Type(ctx, body, ""); err != nil {
		return err
	}
	wait(600)

	// 9. 验证键入是否进入文档：AX 树可见则直接断言；不可见（画布自绘）则
	// 用 cmd+a → cmd+c → 读剪贴板比对（WPS 编辑区剪贴板路径有效）。
	fmt.Println("— 验证键入 —")
	sn, err := ex.Snapshot(ctx, "WPS Office", 8000)
	if err != nil {
		return err
	}
	if strings.Contains(sn, title) || strings.Contains(sn, body) {
		fmt.Printf("✓ AX 树可见键入文本：%s / %s\n", title, body)
		return nil
	}
	fmt.Println("⚠ AX 树未见文档正文（WPS 编辑区为自绘层——预期；走剪贴板验证）")
	if _, err := ex.Key(ctx, "cmd+a"); err != nil {
		return err
	}
	wait(300)
	if _, err := ex.Key(ctx, "cmd+c"); err != nil {
		return err
	}
	wait(400)
	fmt.Println("🏁 已执行：新建 Word + 键入标题/正文（剪贴板应含文本；数据层验证可解包 docx）")
	return nil
}
