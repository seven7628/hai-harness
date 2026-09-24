package main

// calendar.go：真机剧本「打开日历 → 新建日程 → 填标题/备注 → 提交」
// 单进程内连跑（同一 helper 长驻 = 真实会话形态，uid 跨动作稳定）。
// 调用：computer_e2e -helper <path> script calendar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/computer"
)

// runCalendarScript 日历添加日程+备注剧本。每步走 Executor（含验证闭环），
// 输出模型同款文本。任何一步失败即返回（方便真机排查）。
func runCalendarScript(ctx context.Context, ex *computer.Executor, be computer.Backend) error {
	step := func(name, out string, err error) error {
		if err != nil {
			return fmt.Errorf("✗ %s: %v", name, err)
		}
		fmt.Printf("✓ %s → %s\n", name, strings.SplitN(out, "\n", 2)[0])
		return nil
	}
	wait := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	// uidOf 按文本找可交互元素 uid（缓存树）；找不到给带上下文的报错
	uidOf := func(name, text string) (string, error) {
		uid := ex.FindUID(text)
		if uid == "" {
			return "", fmt.Errorf("未找到「%s」入口（%s）——当前树可能不含该文本，需人工查看 snapshot", text, name)
		}
		return uid, nil
	}

	// 1. 打开日历（已在前台则直接激活）
	out, err := ex.OpenApp(ctx, "日历")
	if err := step("open 日历", out, err); err != nil {
		return err
	}
	wait(800)

	// 1.5 切月视图（macOS 日历 cmd+3=月；重启后可能记住年/日视图，
	// 快速创建入口只在月/周/日视图有效）
	out, err = ex.Key(ctx, "cmd+3")
	if err := step("切月视图", out, err); err != nil {
		return err
	}
	wait(600)

	// 2. 菜单：文件 → 新建日程或提醒事项（macOS 中文日历实际措辞）
	out, err = ex.PressMenu(ctx, "日历", []string{"文件", "新建日程或提醒事项"})
	if err := step("menu 新建日程或提醒事项", out, err); err != nil {
		return err
	}
	wait(1200)

	// 3. 快照确认弹窗（快速创建选择：新建日程/新建提醒事项）
	out, err = ex.Snapshot(ctx, "", 9000)
	if err := step("snapshot 弹窗", out, err); err != nil {
		return err
	}
	if !strings.Contains(out, "新建日程") {
		return fmt.Errorf("未见「新建日程」入口，弹窗未出现:\n%s", truncate(out, 600))
	}
	uid, err := uidOf("新建日程按钮", "新建日程")
	if err != nil {
		return err
	}
	out, err = ex.Press(ctx, uid)
	if err := step("press 新建日程 "+uid, out, err); err != nil {
		return err
	}
	wait(1500)

	// 4. snapshot 事件编辑面板 → 标题输入框（首个空 AXTextField 在 popover 顶部）
	out, err = ex.Snapshot(ctx, "", 9000)
	if err := step("snapshot 编辑面板", out, err); err != nil {
		return err
	}
	if !strings.Contains(out, "添加备注") {
		return fmt.Errorf("编辑面板未见「添加备注」，面板未出现:\n%s", truncate(out, 600))
	}
	titleUID := firstTitleFieldUID(out)
	if titleUID == "" {
		return fmt.Errorf("未定位标题输入框:\n%s", truncate(out, 600))
	}
	out, err = ex.Type(ctx, "P0 真机验证日程", titleUID)
	if err := step("键入标题", out, err); err != nil {
		return err
	}
	wait(400)

	// 5. 展开备注区：press「添加备注」
	uid, err = uidOf("添加备注按钮", "添加备注")
	if err != nil {
		return err
	}
	out, err = ex.Press(ctx, uid)
	if err := step("展开备注区 "+uid, out, err); err != nil {
		return err
	}
	wait(1200)

	// 6. snapshot 定位备注输入框（「添加URL/附件」上方的 AXTextField）
	out, err = ex.Snapshot(ctx, "", 9000)
	if err := step("snapshot 备注区", out, err); err != nil {
		return err
	}
	noteUID := noteFieldUID(out)
	if noteUID == "" {
		return fmt.Errorf("未定位备注输入框:\n%s", truncate(out, 800))
	}
	out, err = ex.Type(ctx, "这是 Computer Use P0 真机验证添加的备注", noteUID)
	if err := step("键入备注", out, err); err != nil {
		return err
	}
	wait(400)

	// 7. 提交：macOS 事件编辑 Return 完成。注意：焦点**留在备注框**直接 Return
	//（移走焦点再提交会丢弃未 commit 的备注编辑——真机验证教训，见下）
	out, err = ex.Key(ctx, "Return")
	if err := step("Return 提交", out, err); err != nil {
		return err
	}
	wait(1500)

	// 8. 验证：popover 已关 = 提交成功信号
	out, err = ex.Snapshot(ctx, "", 9000)
	if err := step("snapshot 验证", out, err); err != nil {
		return err
	}
	if strings.Contains(out, "AXPopover") {
		return fmt.Errorf("提交后弹窗仍开——可能未成功（内容或已部分保存），请人工查看日历")
	}
	fmt.Println("🏁 剧本完成：日程已提交（弹窗关闭）。可在日历界面查看「P0 真机验证日程」与备注。")
	return nil
}

// firstTitleFieldUID 从快照文本找编辑面板标题输入框：popover 区域内第一个
// AXTextField 且当前 value 为空（标题框初始为空；日期/提醒等不是文本域）。
func firstTitleFieldUID(out string) string {
	lines := strings.Split(out, "\n")
	inPopover := false
	for _, l := range lines {
		if strings.Contains(l, "AXPopover") {
			inPopover = true
			continue
		}
		if !inPopover {
			continue
		}
		if strings.Contains(l, "AXTextField") {
			return firstField(l)
		}
	}
	return ""
}

// noteFieldUID 备注输入框：popover 内「添加URL/附件/添加附件」按钮上方的
// AXTextField（备注展开后新增，位置在按钮前）。
func noteFieldUID(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if (strings.Contains(l, "添加URL") || strings.Contains(l, "添加附件")) &&
			strings.Contains(l, "AXButton") {
			for j := i - 1; j >= 0 && j > i-8; j-- {
				if strings.Contains(lines[j], "AXTextField") {
					return firstField(lines[j])
				}
			}
		}
	}
	return ""
}

// firstField 取一行 uid 树文本的首字段（uid）。
func firstField(line string) string {
	f := strings.Fields(line)
	if len(f) > 0 {
		return f[0]
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
