// preview_convert.go —— 三条预览命令（xlsx/docx/pptx）的共同通路（R08a 收口）。
//
// 为什么单独成文件（而不是塞进 main_helpers.go）：那个文件的约定是「自包含、不依赖
// manager 状态」的纯搬移函数，而本函数正是 manager 的通道侧接线（路径校验 → 转换服务
// → 响应信封）。放在一起会让「这个文件里能不能依赖 manager」变成看人的记忆。
package main

import (
	"github.com/seven7628/hai-harness/desktop/bridge/conversion"
)

// handlePreviewConvert 三条预览命令（xlsx_preview / docx_preview / pptx_preview）的
// 共同通路（R08a 收口）：路径校验 → 交给转换服务（排队/限额/缓存）→ 回产物路径。
//
// 为什么收成一个函数：三条通路从命令名到 cleanup 逐字同构，唯一差别是 kind。收口前
// 它们是三段复制出来的 goroutine，加一条新格式就要再抄一遍限额与缓存口径 —— 而口径
// （并发 2 / 60s / 失败不缓存）恰恰是最不该有三份副本的东西。
//
// 为什么转换仍放 goroutine：首次调用可能先 bootstrap 受管运行时（建 venv + pip 装约
// 42MB），同步做会把 dispatch 主循环卡住（整个桌面端在此期间无响应）。响应经 m.resp
// 回写（线程安全）——这是既有行为，本 PR 不改。
func (m *manager) handlePreviewConvert(c command, ws, kind string) {
	path := str(c.Payload, "path")
	abs, ok := m.previewInputPath(c, ws, path, kind)
	if !ok {
		return
	}
	pyRT := m.pythonRuntime()
	if pyRT == nil { // 理论上不会（pythonRuntime 恒返回非 nil），留作装配变化的兜底
		m.resp(c, false, "受管 Python 运行时未装配，无法预览 "+kind, nil)
		return
	}
	svc, err := m.previewService(pyRT)
	if err != nil {
		m.respCode(c, false, "转换服务不可用: "+err.Error(), conversion.CodeFailed, nil)
		return
	}
	// 可取消的 ctx（R08a 纪律面）：工作区关闭/退出时取消 —— 取消会转到
	// sandbox.runExec 的 kill(-pgid)，不是「等它自己跑完」。换工作区不取消：
	// 转换产物是按内容寻址的，换个工作区看同一份文件仍然算数。
	ctx, done := m.previewCancels.begin(ws)
	go func() {
		defer done()
		res, cerr := svc.Convert(ctx, conversion.Request{Path: abs, Kind: kind, Workspace: ws})
		if cerr != nil {
			// 错误码进信封（前端可据 code 区分「超时该重试」与「这份文件转不了」）；
			// error 文本保持人话 —— 形状不变，只多一个可选字段。
			m.respCode(c, false, cerr.Error(), conversion.CodeOf(cerr), nil)
			return
		}
		// 响应形状与收口前逐字一致（{path, html_path}），前端一行不改。
		m.resp(c, true, "", map[string]any{"path": path, "html_path": res.HTMLPath})
	}()
}
