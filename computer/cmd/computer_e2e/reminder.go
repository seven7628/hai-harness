package main

// reminder.go：真机剧本「打开日历 → 新建提醒事项 → 填标题 → 提交」
// 单进程内连跑（同一 helper 长驻 = 真实会话形态，uid 跨动作稳定）。
// 调用：computer_e2e -helper <path> script reminder
//
// 真机验证教训（2026-09 沉淀）：
//  1. 日历 popover 按钮（新建日程/新建提醒事项）必须**坐标点击**——AXPress 返回
//     success 但实际关闭面板（Swift cmdPress 已改为坐标优先，见 helper）；
//  2. 提醒标题输入框是 AXTextArea（非 AXTextField）；
//  3. 键入后必须**验证 value 真进入界面**再提交（否则产生「新提醒事项」空标题
//     垃圾——真机曾误创 15 条）；
//  4. 首次创建提醒会弹系统通知授权 sheet（「继续」按钮）——需点掉；
//  5. 数据层确认：提醒存 ~/Library/Group Containers/group.com.apple.reminders/
//     Container_v1/Stores/Data-*.sqlite 的 ZREMCDREMINDER（ZTITLE 列）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/computer"
)

// runReminderScript 日历新建提醒事项剧本（带标题验证，防空标题垃圾）。
func runReminderScript(ctx context.Context, ex *computer.Executor, be computer.Backend) error {
	wait := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	step := func(name, out string, err error) error {
		if err != nil {
			return fmt.Errorf("✗ %s: %v", name, err)
		}
		fmt.Printf("✓ %s → %s\n", name, strings.SplitN(out, "\n", 2)[0])
		return nil
	}

	// 1. 打开日历
	out, err := ex.OpenApp(ctx, "日历")
	if err := step("open 日历", out, err); err != nil {
		return err
	}
	wait(800)

	// 2. 菜单：文件 → 新建日程或提醒事项（重试最多 5 次——菜单展开偶发失败）
	var uid string
	for attempt := 1; attempt <= 5; attempt++ {
		if _, err := ex.PressMenu(ctx, "日历", []string{"文件", "新建日程或提醒事项"}); err != nil {
			return err
		}
		wait(1600)
		if _, err := ex.Snapshot(ctx, "", 15000); err != nil {
			return err
		}
		if uid = ex.FindUID("新建提醒事项"); uid != "" {
			break
		}
		ex.Key(ctx, "Escape")
		wait(500)
	}
	if uid == "" {
		return fmt.Errorf("菜单 5 次未弹出快速创建面板（日历状态可能异常，建议重启日历）")
	}
	fmt.Printf("✓ 快速创建面板已弹出（按钮 %s）\n", uid)

	// 3. 处理可能的通知授权 sheet（首次创建提醒）
	ex.Snapshot(ctx, "", 15000)
	if contUID := ex.FindUID("继续"); contUID != "" {
		out, err = ex.Press(ctx, contUID)
		if err := step("点掉通知授权 sheet", out, err); err != nil {
			return err
		}
		wait(1500)
		// sheet 关后需重开菜单再走
		return fmt.Errorf("已点掉首次授权 sheet，请重跑本剧本（下次不再弹）")
	}

	// 4. Press「新建提醒事项」（helper 坐标点击优先——AXPress 会假关面板）
	out, err = ex.Press(ctx, uid)
	if err := step("press 新建提醒事项", out, err); err != nil {
		return err
	}
	wait(2200)

	// 5. 定位标题输入框（popover 内第一个可见 AXTextArea——提醒标题是 TextArea）
	out, err = ex.Snapshot(ctx, "", 15000)
	if err != nil {
		return err
	}
	tx, ty, tw := 0.0, 0.0, 0.0
	inPop := false
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "AXPopover") {
			inPop = true
			continue
		}
		if inPop && strings.Contains(l, "AXTextArea") && tw == 0 {
			if idx := strings.Index(l, "(x:"); idx >= 0 {
				body := strings.TrimSuffix(l[idx+1:], ")")
				if _, err := fmt.Sscanf(body, "x:%f y:%f w:%f h:%f", &tx, &ty, &tw, new(float64)); err == nil {
					break
				}
			}
		}
	}
	if tw == 0 {
		return fmt.Errorf("未定位标题输入框（提醒面板可能未出现）:\n%s", truncate(out, 600))
	}
	fmt.Printf("✓ 标题输入框 (%.0f,%.0f)\n", tx+20, ty+8)

	// 6. 点击聚焦（点框内偏左位置）+ 键入
	if err := be.Click(computer.Point{X: tx + 20, Y: ty + 8}); err != nil {
		return fmt.Errorf("聚焦点击: %v", err)
	}
	wait(400)
	title := "AI 测试提醒-" + time.Now().Format("150405")
	if _, err := ex.Type(ctx, title, ""); err != nil {
		return err
	}
	wait(500)

	// 7. 键入验证（防空标题垃圾——真机教训）
	out, err = ex.Snapshot(ctx, "", 15000)
	if err != nil {
		return err
	}
	if !strings.Contains(out, title) {
		return fmt.Errorf("键入未进入标题框（焦点可能不对），放弃提交避免空标题提醒")
	}
	fmt.Printf("✓ 键入已验证进入界面：%s\n", title)

	// 8. 提交（Return）
	out, err = ex.Key(ctx, "Return")
	if err := step("Return 提交", out, err); err != nil {
		return err
	}
	wait(1800)

	// 9. 验证弹窗关闭
	out, err = ex.Snapshot(ctx, "", 15000)
	if err != nil {
		return err
	}
	if strings.Contains(out, "AXPopover") {
		return fmt.Errorf("提交后 popover 仍开（可能未成功）")
	}
	fmt.Println("🏁 提醒事项已创建：" + title)
	fmt.Println("（数据层验证：提醒库 ZREMCDREMINDER 应出现该标题）")
	return nil
}
