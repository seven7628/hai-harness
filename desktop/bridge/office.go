// office.go —— LibreOffice kit 的宿主侧（R08d）：探测缓存、设置页状态、PDF 产物缓存、
// 逐页栅格 + 缓存。转换本体在 internal/officekit（那是**只做转换**的环境层）。
//
// 三条降级纪律（05-preview-media-research §8.5）：
//   - kit 不可用 → 预览继续走既有近似渲染（docx=mammoth / xlsx=openpyxl / pptx=python-pptx），
//     响应里如实带 renderer="approximate"（前端据此出横幅）；
//   - kit 可用但这一份文件转不动 → 同样落回近似路径，并把 kit 的失败原因记进日志 ——
//     「kit 坏了」不该让用户连近似预览都看不到；
//   - 任何情况下都不自动下载 kit（分发是 PR-13）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/desktop/bridge/conversion"
	"github.com/seven7628/hai-harness/internal/officekit"
)

const (
	// officeConvertConcurrency 同时跑的 soffice 数。与 conversion 的并发 2 同口径：
	// soffice 单实例就要几百 MB 常驻，再多只会让每个都变慢并触发 60s 误伤。
	officeConvertConcurrency = 2
	// officePDFName / officeManifestName 缓存条目里的产物名（见 conversion.Cache.PutFile）。
	officePDFName      = "preview.pdf"
	officeManifestName = "manifest.json"
)

// officeKitState 办公组件的进程级状态（懒建，见 (*manager).officeKitState）。
type officeKitState struct {
	prober *officekit.Prober
	sem    chan struct{}
	// render 可注入（测试用它替换掉真 Python，验缓存/页号/限额而不依赖运行时）。
	render func(ctx context.Context, py officekit.PythonRuntime, pdf, outDir string, opt officekit.RenderOptions) (officekit.RenderResult, error)
}

// officeKitState 取（或懒建）办公组件状态。
//
// 为什么是进程级单例：探测要哈希 400MB 的二进制，信号量必须**全局**（会话之间共用一个
// soffice 池），预热也只需要一次。每次会话新建 = 每个会话各跑两个 soffice。
func (m *manager) officeKitState() *officeKitState {
	m.officeMu.Lock()
	defer m.officeMu.Unlock()
	if m.office == nil {
		m.office = &officeKitState{
			prober: officekit.NewProber(officekit.ProbeOptions{}),
			sem:    make(chan struct{}, officeConvertConcurrency),
			render: officekit.RenderImages,
		}
	}
	return m.office
}

// officeKitStatus 设置页/横幅要的状态（refresh = 用户点了「重新探测」）。
func (m *manager) officeKitStatus(refresh bool) officekit.Status {
	st := m.officeKitState().prober.Status()
	if refresh {
		st = m.officeKitState().prober.Refresh()
	}
	return st
}

// officeKitGone kit 的 bin 是否已经不在了（用户卸载 / 清理 app 数据 / 换了一台机器同步过来）。
//
// 为什么单看 stat 就够：Prober 的 Kit() 是**缓存**的，它不会随着文件被删而变 —— 这正是
// 「卸载后横幅还写着可用」的机制。转换是第一个能观察到事实变化的时刻，所以在这里判。
func officeKitGone(kit *officekit.Kit) bool {
	if kit == nil || kit.Bin == "" {
		return false
	}
	_, err := os.Stat(kit.Bin)
	return errors.Is(err, os.ErrNotExist)
}

// invalidateOfficeKit 作废探测缓存，并把新状态推给 renderer（R08d 复评 P1）。
//
// 为什么必须推：状态是**进程级缓存**，不推的话横幅会一直显示「可用」，直到用户自己
// 去点设置页的「重新探测」—— 而用户卸载后看到的应该是「已缺失 + 怎么装」，不是
// 「一切正常」的谎。事件名与命令同名（office_kit_status），前端用它更新同一份 store 字段。
//
// 为什么不必同步：重探要重新哈希 kit 二进制（几百 MB），放在转换的回落路径上会把
// 「已经失败的那次预览」再拖几秒；横幅晚几百毫秒消失没有代价。
func (m *manager) invalidateOfficeKit(goneBin string) {
	st := m.officeKitState().prober.Refresh()
	fmt.Fprintf(os.Stderr, "[officekit] kit 已不在（%s），状态刷新为 %s\n", goneBin, st.State)
	m.out.writeLine(map[string]any{
		"event_type": "office_kit_status",
		"data":       map[string]any{"kit": st},
	})
}

// warmOfficeKit 启动时预热一次（在 kit 首次可用时跑一次空转换，把冷启成本挪走）。
//
// 失败不致命、也不打印成错误：它只是「让第一次真实转换快一点」的优化。
func (m *manager) warmOfficeKit() {
	defer func() { _ = recover() }() // 预热绝不能把 bridge 启动搞崩
	st := m.officeKitState()
	if !st.prober.Status().Available {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*officekit.DefaultTimeout)
	defer cancel()
	if err := st.prober.WarmOnce(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "[officekit] 预热失败（不影响转换）:", err)
	}
}

// acquire 取一个转换槽位；满了排队（上界 2×60s，与 conversion 的 queue_timeout 同口径）。
func (st *officeKitState) acquire(ctx context.Context) (func(), error) {
	select {
	case st.sem <- struct{}{}:
		return func() { <-st.sem }, nil
	default:
	}
	t := time.NewTimer(2 * officekit.DefaultTimeout)
	defer t.Stop()
	select {
	case st.sem <- struct{}{}:
		return func() { <-st.sem }, nil
	case <-ctx.Done():
		return nil, conversion.NewError(conversion.CodeCanceled, "办公组件转换已取消")
	case <-t.C:
		return nil, conversion.NewError(conversion.CodeQueueTimeout, "办公组件正忙（排队超过 120s），请稍后重试")
	}
}

// officeCode 把 officekit 的错误翻成响应信封的稳定码。
func officeCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, officekit.ErrTimeout):
		return conversion.CodeTimeout
	case errors.Is(err, officekit.ErrCanceled):
		return conversion.CodeCanceled
	default:
		return conversion.CodeFailed
	}
}

// convertOfficePDF 把一份办公文档转成 PDF 并写进预览缓存，返回缓存内的 PDF 路径。
//
// 缓存键与 HTML 预览同源（conversion.CacheKey：口径版本 | 脚本指纹 | kind | 文件名 |
// 参数 | 文件字节），参数位带上 kit 版本 —— 换了 kit 就该重转（PDF 导出行为会变）。
func (m *manager) convertOfficePDF(ctx context.Context, abs, kind string) (string, error) {
	st := m.officeKitState()
	kit := st.prober.Kit()
	if kit == nil {
		return "", fmt.Errorf("%w: 未安装 LibreOffice kit", officekit.ErrUnavailable)
	}
	svc, err := m.previewService(m.pythonRuntime())
	if err != nil {
		return "", err
	}
	// ContentKey：两级键（命中备忘时不读文件）—— 键与 conversion.CacheKey 逐字相同。
	key, err := svc.ContentKey(kind, filepath.Base(abs), map[string]string{
		"renderer": "libreoffice",
		"kit":      kit.Version,
	}, abs)
	if err != nil {
		return "", err
	}
	if p, ok := svc.Cache().FileAt(key, officePDFName); ok {
		return p, nil
	}

	release, err := st.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	// 再查一次：排队期间别的会话可能已经转好了同一份文件。
	if p, ok := svc.Cache().FileAt(key, officePDFName); ok {
		return p, nil
	}
	jobDir, err := os.MkdirTemp("", "office-kit-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(jobDir)
	// profile 也放 jobDir 下：一个 job 的临时目录整块回收（进程被杀也不会留 profile）。
	res, err := kit.Convert(ctx, abs, jobDir, officekit.ConvertOptions{ProfileRoot: jobDir})
	if err != nil {
		// kit 被删掉时这里报的是「启动 soffice: fork/exec …: no such file or directory」——
		// 它不只是「这份文件转不了」，而是**状态本身变了**：作废缓存 + 推新状态，
		// 否则横幅会一直说「可用」，直到用户手动点「重新探测」（R08d 复评 P1）。
		if officeKitGone(kit) {
			go m.invalidateOfficeKit(kit.Bin)
		}
		return "", err
	}
	f, err := os.Open(res.PDFPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return svc.Cache().PutFile(key, officePDFName, f, conversion.Meta{
		Kind: kind, Name: filepath.Base(abs), Created: time.Now(),
	})
}

// officeRenderOutput 逐页栅格的结果（工具与响应共用；同时是缓存条目的 manifest 形状）。
type officeRenderOutput struct {
	Pages []officekit.RenderedPage `json:"pages"`
	Total int                      `json:"total"`
	DPI   int                      `json:"dpi"`
	// Cached/PDF 不进 manifest：它们是「本次查询的处境」，不是产物的一部分。
	Cached bool   `json:"-"`
	PDF    string `json:"-"`
}

// renderOfficePages 把 PDF（或办公文档，先转 PDF）逐页栅格成 PNG，产物进同一套缓存。
//
// 缓存键带 dpi 与页号：同一份 PDF 的「全页 144dpi」与「第 3 页 72dpi」是两个条目，
// 各自命中 —— 这正是 office_render 反复看同一页时的常态。
func (m *manager) renderOfficePages(ctx context.Context, src string, pages []int, limit, dpi int) (officeRenderOutput, error) {
	if dpi <= 0 {
		dpi = officekit.DefaultDPI
	}
	st := m.officeKitState()
	if st.prober.Kit() == nil {
		return officeRenderOutput{}, fmt.Errorf("%w: 未安装 LibreOffice kit", officekit.ErrUnavailable)
	}
	pdf := src
	if !strings.EqualFold(filepath.Ext(src), ".pdf") {
		kind := officeKindOf(src)
		if kind == "" {
			return officeRenderOutput{}, fmt.Errorf("%w: 不支持的格式 %s", officekit.ErrConvertFailed, filepath.Ext(src))
		}
		p, err := m.convertOfficePDF(ctx, src, kind)
		if err != nil {
			return officeRenderOutput{}, err
		}
		pdf = p
	}
	svc, err := m.previewService(m.pythonRuntime())
	if err != nil {
		return officeRenderOutput{}, err
	}
	// ContentKey：两级键（命中备忘时不读文件）—— 键与 conversion.CacheKey 逐字相同。
	key, err := svc.ContentKey("render", filepath.Base(pdf), map[string]string{
		"dpi": strconv.Itoa(dpi), "pages": pageList(pages), "limit": strconv.Itoa(limit),
		"kit": st.prober.Kit().Version,
	}, pdf)
	if err != nil {
		return officeRenderOutput{}, err
	}
	if out, ok := readCachedRender(svc.Cache(), key, dpi, pdf); ok {
		return out, nil
	}

	release, err := st.acquire(ctx)
	if err != nil {
		return officeRenderOutput{}, err
	}
	defer release()
	if out, ok := readCachedRender(svc.Cache(), key, dpi, pdf); ok {
		return out, nil
	}
	jobDir, err := os.MkdirTemp("", "office-render-*")
	if err != nil {
		return officeRenderOutput{}, err
	}
	defer os.RemoveAll(jobDir)
	res, err := st.render(ctx, m.pythonRuntime(), pdf, jobDir, officekit.RenderOptions{DPI: dpi, Pages: pages, Limit: limit})
	if err != nil {
		return officeRenderOutput{}, err
	}
	// 产物逐个写进内容寻址条目（0600 + LRU 与其他预览产物同一套预算），返回的是
	// **缓存里的路径**：临时目录在 defer 里就删了，把它给模型 = 交付一个马上失效的路径。
	rendered := make([]officekit.RenderedPage, 0, len(res.Pages))
	for _, p := range res.Pages {
		b, rerr := os.ReadFile(p.Path)
		if rerr != nil {
			return officeRenderOutput{}, rerr
		}
		cached, werr := svc.Cache().PutFile(key, filepath.Base(p.Path), bytes.NewReader(b), conversion.Meta{
			Kind: "render", Name: filepath.Base(pdf), Created: time.Now(),
		})
		if werr != nil {
			return officeRenderOutput{}, werr
		}
		cp := p
		cp.Path = cached
		rendered = append(rendered, cp)
	}
	out := officeRenderOutput{Pages: rendered, Total: res.TotalPages, DPI: dpi}
	manifest, _ := json.Marshal(out)
	if _, err := svc.Cache().PutFile(key, officeManifestName, bytes.NewReader(manifest), conversion.Meta{
		Kind: "render", Name: filepath.Base(pdf), Created: time.Now(),
	}); err != nil {
		return officeRenderOutput{}, err
	}
	return out, nil
}

// readCachedRender 读回缓存里的渲染结果（manifest 在 + 每页 PNG 都在才算命中）。
func readCachedRender(c *conversion.Cache, key string, dpi int, pdf string) (officeRenderOutput, bool) {
	p, ok := c.FileAt(key, officeManifestName)
	if !ok {
		return officeRenderOutput{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return officeRenderOutput{}, false
	}
	var out officeRenderOutput
	if err := json.Unmarshal(b, &out); err != nil {
		return officeRenderOutput{}, false
	}
	for i := range out.Pages {
		fp, ok := c.FileAt(key, filepath.Base(out.Pages[i].Path))
		if !ok {
			return officeRenderOutput{}, false // 条目被 LRU 删了一半 → 当未命中重做
		}
		out.Pages[i].Path = fp
	}
	out.Cached = true
	out.DPI = dpi
	out.PDF = pdf
	return out, true
}

// pageList 页号的稳定字符串（缓存键的一部分：顺序/重复都要成为键的一部分）。
func pageList(pages []int) string {
	if len(pages) == 0 {
		return "all"
	}
	parts := make([]string, len(pages))
	for i, p := range pages {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// officeKindOf 扩展名 → 转换器 kind（空 = 不走 kit 的格式）。
func officeKindOf(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".docx", ".docm", ".doc":
		return "docx"
	case ".pptx", ".pptm", ".ppt":
		return "pptx"
	case ".xlsx", ".xlsm", ".xls":
		return "xlsx"
	}
	return ""
}

// handleOfficeKitStatus 设置页命令：给出状态 + 平台矩阵（不含自动下载，说明在文案里）。
func (m *manager) handleOfficeKitStatus(c command) {
	refresh := false
	if v, ok := c.Payload["refresh"].(bool); ok {
		refresh = v
	}
	st := m.officeKitStatus(refresh)
	if st.Available {
		// 首次被问到时顺手预热（异步）：设置页/横幅本来就是「用户开始关心组件」的时刻。
		go m.warmOfficeKit()
	}
	m.resp(c, true, "", map[string]any{"kit": st})
}
