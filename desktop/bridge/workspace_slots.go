package main

// 工作区槽位：把「看着像进程级单例、内容其实按工作区」的状态变成有 owner 的槽位（BR-21）。
//
// 问题（审计 BR-21）：xlsxPV / docxPV / pptxPV 是 manager 上的进程级字段，懒建一次就服务
// 所有工作区，而它们的沙箱根（Seatbelt 读写根）却来自**调用点**传进来的 workspace 参数
// （Convert(ctx, path, workspace)）。于是「沙箱根属于谁」只靠调用点自觉：新增一条预览通路
// 传错 ws、或把某个命令的 ws 换成 payload 里的另一个值，转换就会以别的工作区的读写根运行。
// 这类错配不会报错，只会静默越权 —— 这正是审计把它记 P0 的理由。
//
// 修法（不是把 Map 换成另一个全局 Map）：
//   - **owner 明确**：槽位以工作区绝对路径为 key（previewSlot.workspace），沙箱根在建槽时
//     固定，调用点不再参与决定（previewSandboxRoot 只认槽位工作区）；
//   - **回收路径明确**：删除工作区 / 关闭工作区 / 进程退出都释放槽位
//     （deleteWorkspace / shutdownWorkspace / shutdownAll），不存在只增不减的登记表；
//   - 转换器本身无后台 goroutine、无跨请求状态（产物在受管临时目录按年龄清扫），
//     故释放只需丢引用，不需要 Stop。
//
// 反过来，**不**给 hookTrust / imPlugin 造同样的槽位，原因是这两者的 owner 就是进程
// （信任库是全局文件 ~/.go-code/hooks-state.json；IM 是对账号的一条连接），按工作区分槽会
// 把「一份真相」拆成多份各自写的副本：信任库出现后写覆盖前写（静默丢失已确认的 hook），
// IM 出现同一账号多条连接（重复回消息）。它们要的是「生命周期归进程」而不是「按工作区复制」，
// 见 main.go 字段注释与 workspace_slots_test.go 的回归。

import (
	"context"
	"sort"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// previewSlot 一个工作区的预览转换器集合（三条预览通路共用同一工作区根）。
type previewSlot struct {
	// workspace 槽位 owner：沙箱读写根就是它（建槽时固定，调用点无法替换）。
	workspace string
	// py 建槽时绑定的受管运行时（换运行时 → 槽位重建，见 previewSlotFor）。
	py   builtin.PythonRuntime
	xlsx *builtin.XlsxPreviewer
	docx *builtin.DocxPreviewer
	pptx *builtin.PptxPreviewer
	// createdAt 建槽时刻（诊断用：长期不释放＝回收路径漏了）。
	createdAt time.Time
}

// previewSandboxRoot 决定预览转换的沙箱根：**只认槽位工作区**。
//
// 调用点传进来的 workspace 仅在没有槽位工作区时兜底（ws="" 的「工作区外绝对路径预览」
// 语义：根为空 = 与旧行为一致，沙箱构造失败就按既有降级路径走 NoSandbox）。
// 抽成纯函数是为了能被单测钉住：这条不变式一旦被改回「调用点说了算」，就越权静默复发。
func previewSandboxRoot(slotWorkspace, callWorkspace string) string {
	if slotWorkspace != "" {
		return slotWorkspace
	}
	return callWorkspace
}

// previewSandboxFor 构造绑定到槽位工作区的沙箱工厂。
// builtin 侧每次 Convert 都会调用它并把自己的 workspace 参数传回来 —— 这里刻意把那
// 个参数交给 previewSandboxRoot 判定（以槽位为准），而不是直接采信。
func (m *manager) previewSandboxFor(slotWorkspace, scope string) builtin.SandboxFor {
	return func(callWorkspace string) sandbox.Sandbox {
		return m.readOnlyConvertSandbox(previewSandboxRoot(slotWorkspace, callWorkspace), scope)
	}
}

// newPreviewSlot 建一个槽位的三条转换器（构造无 I/O：Ensure 在首次 Convert 里）。
func (m *manager) newPreviewSlot(ws string, py builtin.PythonRuntime) *previewSlot {
	return &previewSlot{
		workspace: ws,
		py:        py,
		xlsx:      builtin.NewXlsxPreviewer(py, m.previewSandboxFor(ws, "xlsx_preview")),
		docx:      builtin.NewDocxPreviewer(py, m.previewSandboxFor(ws, "office_preview")),
		pptx:      builtin.NewPptxPreviewer(py, m.previewSandboxFor(ws, "office_preview")),
		createdAt: time.Now(),
	}
}

// previewSlotLimit 槽位上限（防「任意 workspace 字符串」把登记表撑大）。
//
// 正常使用远到不了这个数：桌面端同时活跃的工作区是个位数，预览只在用户点开产出时建槽。
// 设上限是为了让「谁还被登记着」有界 —— 没有界的有 owner 表也会变成泄漏。
const previewSlotLimit = 32

// previewSlotFor 取（或懒建）某工作区的预览槽位。
//
// py 参与身份判定：运行时实例被换掉（测试注入 / 未来热切）时槽位随重建，沙箱根不变。
// 锁序：先 m.pythonRuntime()（内层 m.mu）**再** previewMu —— 本函数内不再取 m.mu，
// 避免与调用方可能的 m.mu 持有形成环。
func (m *manager) previewSlotFor(ws string, pyRT *runtime.Runtime) *previewSlot {
	var py builtin.PythonRuntime
	if pyRT != nil {
		py = pyRT
	}
	m.previewMu.Lock()
	defer m.previewMu.Unlock()
	if s := m.previewSlots[ws]; s != nil && s.py == py {
		return s
	}
	if m.previewSlots == nil {
		m.previewSlots = map[string]*previewSlot{}
	}
	s := m.newPreviewSlot(ws, py)
	m.previewSlots[ws] = s
	m.evictOldestPreviewSlotsLocked(ws)
	return s
}

// evictOldestPreviewSlotsLocked 超上限时按建槽时刻淘汰最旧的（keep 不被淘汰）。
// 调用方必须持有 previewMu。
func (m *manager) evictOldestPreviewSlotsLocked(keep string) {
	for len(m.previewSlots) > previewSlotLimit {
		oldest, oldestAt := "", time.Time{}
		for k, v := range m.previewSlots {
			if k == keep {
				continue
			}
			if oldest == "" || v.createdAt.Before(oldestAt) {
				oldest, oldestAt = k, v.createdAt
			}
		}
		if oldest == "" {
			return
		}
		// 淘汰时**关掉它的常驻进程**（第 5 轮爬山第 4 步）：不关的话，这些
		// python 进程（每个约 33MB）要等 5 分钟空闲回收才消失 ⇒ 槽位反复换工作区
		// 时，就是一批进程在内存里排队等死。
		m.previewSlots[oldest].close()
		delete(m.previewSlots, oldest)
	}
}

// close 关掉槽位里的三条转换器（连同它们的常驻 worker）。
func (s *previewSlot) close() {
	if s == nil {
		return
	}
	s.xlsx.Close()
	s.docx.Close()
	s.pptx.Close()
}

// releasePreviewSlot 回收某工作区的槽位（删除/关闭工作区调用；不存在的 key 无副作用）。
func (m *manager) releasePreviewSlot(ws string) {
	m.previewMu.Lock()
	if s, ok := m.previewSlots[ws]; ok {
		s.close()
	}
	delete(m.previewSlots, ws)
	m.previewMu.Unlock()
}

// releaseAllPreviewSlots 回收全部槽位（进程退出）。
func (m *manager) releaseAllPreviewSlots() {
	m.previewMu.Lock()
	for _, s := range m.previewSlots {
		s.close()
	}
	m.previewSlots = nil
	m.previewMu.Unlock()
}

// previewSlotWorkspaces 当前仍持有槽位的工作区（升序）。
// 存在的意义：让「谁还被登记着」可被断言 —— 没有 owner 清单的进程级状态正是本条要治的病。
func (m *manager) previewSlotWorkspaces() []string {
	m.previewMu.Lock()
	defer m.previewMu.Unlock()
	out := make([]string, 0, len(m.previewSlots))
	for ws := range m.previewSlots {
		out = append(out, ws)
	}
	sort.Strings(out)
	return out
}

// —— 槽位上的三条转换（调用点拿不到 workspace 参数，沙箱根只能是槽位自己的）——

// convertXLSX .xlsx → HTML。
func (s *previewSlot) convertXLSX(ctx context.Context, path string) (builtin.XlsxConvertResult, error) {
	return s.xlsx.Convert(ctx, path, s.workspace)
}

// convertDOCX .docx → HTML。
func (s *previewSlot) convertDOCX(ctx context.Context, path string) (builtin.DocxConvertResult, error) {
	return s.docx.Convert(ctx, path, s.workspace)
}

// convertPPTX .pptx → HTML。
func (s *previewSlot) convertPPTX(ctx context.Context, path string) (builtin.PptxConvertResult, error) {
	return s.pptx.Convert(ctx, path, s.workspace)
}
