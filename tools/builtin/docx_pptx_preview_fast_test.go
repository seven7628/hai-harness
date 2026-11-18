package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// DocxPreviewer / PptxPreviewer 的**快测试**（默认跑，含 CI）：注入假受管运行时 + 假沙箱，
// 不联网、不建 venv。两者结构逐字镜像 XlsxPreviewer，故测试形态也镜像
// xlsx_preview_fast_test.go（同一组契约必须对三条通路都成立，否则「泛化」就是假的）：
// 参数校验（输入路径/临时目录选址）、命令行形状（固定解释器 + 三个单引号路径）、
// 成败判定（标记行/非零退出/沙箱拦截/产物缺失）、失败路径清理、并发互不删除。
//
// 「真造 docx/pptx + 真跑 mammoth/python-pptx」的慢测试见 docx_pptx_preview_real_test.go。

// opvFakeSandbox 记录 ExecSpec 的假沙箱：按脚本需要「产出」HTML（模拟解释器行为）。
type opvFakeSandbox struct {
	mu      sync.Mutex // 并发 Convert 共用同一个假沙箱 ⇒ 字段受锁保护（-race）
	spec    sandbox.ExecSpec
	script  string // Run 时读到的脚本原文（验证传给解释器的是固定脚本）
	out     string
	err     error
	write   bool // 是否真的写一个 HTML 产物（模拟成功转换）
	failMsg string
}

func (f *opvFakeSandbox) Run(_ context.Context, spec sandbox.ExecSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = spec
	args := strings.Split(spec.Command, " '")
	if len(args) >= 2 {
		if b, err := os.ReadFile(strings.TrimSuffix(args[1], "'")); err == nil {
			f.script = string(b)
		}
	}
	if f.write {
		// 命令行形如 '<python>' '<script>' '<输入>' '<html>'
		htmlPath := strings.TrimSuffix(args[len(args)-1], "'")
		if err := os.WriteFile(htmlPath, []byte("<html>fake</html>"), 0o600); err != nil {
			return "", err
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if f.failMsg != "" {
		return f.failMsg, nil
	}
	return f.out, nil
}

// opvFixedSandbox 恒定返回同一假沙箱的工厂（真实实现按工作区构造，见 bridge 侧
// officePreviewSandbox：Seatbelt 的读写根在构造时注入）。
func opvFixedSandbox(sbx sandbox.Sandbox) SandboxFor {
	return func(string) sandbox.Sandbox { return sbx }
}

func opvFakeRuntime(root string) *rpFakeRuntime {
	return &rpFakeRuntime{py: goruntime.Python{Root: root, VenvPython: "/usr/bin/python3", Base: "/usr/bin/python3"}}
}

func TestDocxPreviewCommandShape(t *testing.T) {
	root := t.TempDir()
	sbx := &opvFakeSandbox{write: true, out: docxOKMarker}
	p := NewDocxPreviewer(opvFakeRuntime(root), opvFixedSandbox(sbx))

	res, err := p.Convert(context.Background(), "/tmp/我 的报告.docx", "/tmp")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)

	// 固定解释器 + 脚本/输入/输出三个单引号参数；输入里的空格必须被安全转义。
	if !strings.HasPrefix(sbx.spec.Command, "'/usr/bin/python3' '") {
		t.Errorf("命令行未使用固定解释器: %s", sbx.spec.Command)
	}
	if !strings.Contains(sbx.spec.Command, "'/tmp/我 的报告.docx'") {
		t.Errorf("docx 路径未按单引号转义: %s", sbx.spec.Command)
	}
	if !strings.Contains(sbx.spec.Command, "'"+res.HTMLPath+"'") {
		t.Errorf("产物路径未按单引号转义: %s", sbx.spec.Command)
	}
	// 脚本是编译期常量（不接受任何外部输入）。
	if !strings.Contains(sbx.script, "mammoth") || !strings.Contains(sbx.script, docxOKMarker) {
		t.Error("传给解释器的不是内置转换脚本")
	}
	// 临时目录在受管运行时根下的 tmp（沙箱 carve-out）——不落在工作区/其他位置。
	if !strings.HasPrefix(res.Dir, filepath.Join(root, "tmp")) {
		t.Errorf("产物目录应位于运行时 tmp 下: %s", res.Dir)
	}
	// 目录前缀必须是 docx-preview-（清扫按前缀识别，写错会留下永不回收的残留）。
	if !strings.HasPrefix(filepath.Base(res.Dir), "docx-preview-") {
		t.Errorf("临时目录前缀应为 docx-preview-: %s", filepath.Base(res.Dir))
	}
	// cwd = 临时目录（解释器写 __pycache__ 不触碰用户目录）。
	if sbx.spec.Cwd != res.Dir {
		t.Errorf("cwd 应为产物目录: %s", sbx.spec.Cwd)
	}
	if sbx.spec.Timeout != DocxPreviewTimeout {
		t.Errorf("时限 = %v，期望 %v", sbx.spec.Timeout, DocxPreviewTimeout)
	}
}

func TestPptxPreviewCommandShape(t *testing.T) {
	root := t.TempDir()
	sbx := &opvFakeSandbox{write: true, out: pptxOKMarker}
	p := NewPptxPreviewer(opvFakeRuntime(root), opvFixedSandbox(sbx))

	res, err := p.Convert(context.Background(), "/tmp/我 的演示.pptx", "/tmp")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)

	if !strings.HasPrefix(sbx.spec.Command, "'/usr/bin/python3' '") {
		t.Errorf("命令行未使用固定解释器: %s", sbx.spec.Command)
	}
	if !strings.Contains(sbx.spec.Command, "'/tmp/我 的演示.pptx'") {
		t.Errorf("pptx 路径未按单引号转义: %s", sbx.spec.Command)
	}
	if !strings.Contains(sbx.spec.Command, "'"+res.HTMLPath+"'") {
		t.Errorf("产物路径未按单引号转义: %s", sbx.spec.Command)
	}
	if !strings.Contains(sbx.script, "python-pptx") && !strings.Contains(sbx.script, "from pptx import Presentation") {
		t.Error("传给解释器的不是内置转换脚本")
	}
	if !strings.Contains(sbx.script, pptxOKMarker) {
		t.Error("脚本缺少成功标记行")
	}
	if !strings.HasPrefix(res.Dir, filepath.Join(root, "tmp")) {
		t.Errorf("产物目录应位于运行时 tmp 下: %s", res.Dir)
	}
	if !strings.HasPrefix(filepath.Base(res.Dir), "pptx-preview-") {
		t.Errorf("临时目录前缀应为 pptx-preview-: %s", filepath.Base(res.Dir))
	}
	if sbx.spec.Cwd != res.Dir {
		t.Errorf("cwd 应为产物目录: %s", sbx.spec.Cwd)
	}
	if sbx.spec.Timeout != PptxPreviewTimeout {
		t.Errorf("时限 = %v，期望 %v", sbx.spec.Timeout, PptxPreviewTimeout)
	}
}

// 两个转换器的失败路径逐一对照：错误措辞必须完全一致（同一条通路的不同格式不该有两种语气）。
func TestOfficePreviewFailurePaths(t *testing.T) {
	// officeConv 把两个转换器收敛成同一形态（输入路径, 工作区）→ (目录, error)，
	// 好让失败路径的用例表只写一遍。两条通路的契约本就相同，测两遍是**有意的**：
	// 将来任一条被改坏（比如少了清理、换了标记常量），这里会立刻红。
	type officeConv struct {
		format string
		marker string // 该格式自己的成功标记
		run    func(root string, sbx *opvFakeSandbox) (string, error)
	}
	convFor := func(format string) officeConv {
		return officeConv{
			format: format,
			marker: map[string]string{"docx": docxOKMarker, "pptx": pptxOKMarker}[format],
			run: func(root string, sbx *opvFakeSandbox) (string, error) {
				var dir string
				var err error
				if format == "docx" {
					res, cerr := NewDocxPreviewer(opvFakeRuntime(root), opvFixedSandbox(sbx)).
						Convert(context.Background(), "/tmp/a.docx", "/tmp")
					dir, err = res.Dir, cerr
					return dir, err
				}
				res, cerr := NewPptxPreviewer(opvFakeRuntime(root), opvFixedSandbox(sbx)).
					Convert(context.Background(), "/tmp/a.pptx", "/tmp")
				return res.Dir, cerr
			},
		}
	}

	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"脚本非零退出（沙箱把原因并入输出）", "Traceback...\n[exit: exit status 1]", nil, "转换失败"},
		{"沙箱拦截", "sh: Not permitted [sandbox: 该操作被沙箱拦截]", nil, "转换失败"},
		{"Run 返回错误", "", errors.New("boom"), "执行转换脚本失败"},
		{"标记有但产物缺失", "MARKER", nil, "未产出 HTML"},
	}
	for _, format := range []string{"docx", "pptx"} {
		tg := convFor(format)
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				out := tc.out
				if out == "MARKER" {
					out = tg.marker // 必须是**该格式**的标记，写死另一个会假通过
				}
				sbx := &opvFakeSandbox{out: out, err: tc.err}
				dir, err := tg.run(root, sbx)
				if err == nil {
					t.Fatalf("期望失败，却成功了（dir=%s）", dir)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("错误信息 = %q，期望含 %q", err.Error(), tc.want)
				}
				// 失败路径必须清理临时目录（不留垃圾）。
				entries, _ := os.ReadDir(filepath.Join(root, "tmp"))
				if len(entries) != 0 {
					t.Errorf("失败后残留临时目录: %v", entries)
				}
			})
		}
	}
}

func TestOfficePreviewNoRuntime(t *testing.T) {
	if _, err := NewDocxPreviewer(nil, nil).Convert(context.Background(), "/tmp/a.docx", "/tmp"); err == nil ||
		!strings.Contains(err.Error(), "no managed Python runtime") {
		t.Errorf("docx 未装配运行时应报明确错误，实际: %v", err)
	}
	if _, err := NewPptxPreviewer(nil, nil).Convert(context.Background(), "/tmp/a.pptx", "/tmp"); err == nil ||
		!strings.Contains(err.Error(), "no managed Python runtime") {
		t.Errorf("pptx 未装配运行时应报明确错误，实际: %v", err)
	}
}

// 运行时 Ensure 失败（bootstrap 挂掉）→ 错误向上传递，不 panic。
func TestOfficePreviewRuntimeUnavailable(t *testing.T) {
	bad := &rpFakeRuntime{err: errors.New("pip 断网")}
	if _, err := NewDocxPreviewer(bad, nil).Convert(context.Background(), "/tmp/a.docx", "/tmp"); err == nil ||
		!strings.Contains(err.Error(), "managed Python runtime unavailable") {
		t.Errorf("docx 运行时不应用时报明确错误，实际: %v", err)
	}
	if _, err := NewPptxPreviewer(bad, nil).Convert(context.Background(), "/tmp/a.pptx", "/tmp"); err == nil ||
		!strings.Contains(err.Error(), "managed Python runtime unavailable") {
		t.Errorf("pptx 运行时不应用时报明确错误，实际: %v", err)
	}
}

// 并发转换各自独立目录，**都不互相删**（产物要留给浏览器加载；回收按年龄）。
func TestOfficePreviewConcurrentConversionsKeepArtifacts(t *testing.T) {
	root := t.TempDir()
	sbx := &opvFakeSandbox{write: true, out: docxOKMarker}
	p := NewDocxPreviewer(opvFakeRuntime(root), opvFixedSandbox(sbx))

	var wg sync.WaitGroup
	dirs := make([]string, 4)
	paths := make([]string, 4)
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := p.Convert(context.Background(), "/tmp/a.docx", "/tmp")
			if err != nil {
				t.Errorf("并发转换 %d 失败: %v", i, err)
				return
			}
			dirs[i], paths[i] = res.Dir, res.HTMLPath
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, d := range dirs {
		if d == "" {
			continue
		}
		if seen[d] {
			t.Errorf("第 %d 次转换复用了他人目录（并发会互相踩）: %s", i, d)
		}
		seen[d] = true
		if _, err := os.Stat(paths[i]); err != nil {
			t.Errorf("第 %d 次产物被他人转换删掉（浏览器会白屏）: %v", i, err)
		}
	}
	if len(seen) != len(dirs) {
		t.Errorf("应产生 %d 个独立目录，实际 %d", len(dirs), len(seen))
	}
	for d := range seen {
		_ = os.RemoveAll(d)
	}
}

// 共享清扫：三种办公预览前缀的过期残留都要收，未过期的与无关前缀必须留着。
//
// 这条与 xlsx_preview_fast_test.go 里的 TestXlsxPreviewSweepsStaleDirs 是**互补**的：
// 那条锁旧函数只认 xlsx-preview-*（既有行为），这条锁新的合并清扫会顺手收掉另外两种格式的
// 残留 —— 若只扫自己那一份，用户只预览 Word 时过期的 xlsx 产物永远没人回收。
func TestOfficeSweepsStaleDirsAllFormats(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := map[string]string{
		"xlsx": filepath.Join(base, "xlsx-preview-stale"),
		"docx": filepath.Join(base, "docx-preview-stale"),
		"pptx": filepath.Join(base, "pptx-preview-stale"),
	}
	fresh := filepath.Join(base, "docx-preview-fresh")
	other := filepath.Join(base, "run-python-keep")
	for _, d := range []string{fresh, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range stale {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range stale {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	py := goruntime.Python{Root: root, VenvPython: "/usr/bin/python3"}
	sweepStaleOfficeTempDirs(py, fresh)

	for format, d := range stale {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("过期的 %s 预览残留应被清扫: %s", format, d)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("刚生成/正在用的目录不得被清扫（可能属于另一个存活实例）: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("非预览前缀（run-python-*）的目录不得被清扫: %v", err)
	}
}

// 内置脚本的静态体检：几个「改错了会静默出问题」的点必须留在脚本里。
func TestDocxPreviewScriptInvariants(t *testing.T) {
	for _, want := range []struct{ frag, why string }{
		{`<meta charset="utf-8">`, "缺 charset 会让中文 mojibake（实测教训）"},
		{"<style>", "内嵌浏览器不继承应用样式，不写样式就是裸正文"},
		{"html.escape", "文档内容必须转义（<script> 会被当脚本执行）"},
		{"mammoth.convert_to_html", "转换必须在受管运行时里跑 mammoth"},
		{"style_map", "Word 的 Title/Subtitle 与中文样式名需显式映射（否则退化成裸 <p>）"},
		{"mammoth.images.data_uri", "内嵌图片走 data: URI（不依赖外部文件路径）"},
		{"MAX_IMAGES", "内嵌图片张数闸门"},
		{"MAX_IMAGE_BYTES", "内嵌图片总量闸门"},
		{"MAX_FRAGMENT_CHARS", "正文长度闸门（超长文档会让浏览器布局卡住）"},
		{`rfind(">", 0, MAX_FRAGMENT_CHARS)`, "超长正文必须在标签边界截断（否则半截标签吞掉后续文本）"},
		{"不还原分页", "版式损失必须写在页面上（不能让用户以为预览坏了）"},
	} {
		if !strings.Contains(docxToHTMLScript, want.frag) {
			t.Errorf("docx 内置脚本缺少 %q —— %s", want.frag, want.why)
		}
	}
}

func TestPptxPreviewScriptInvariants(t *testing.T) {
	for _, want := range []struct{ frag, why string }{
		{`<meta charset="utf-8">`, "缺 charset 会让中文 mojibake"},
		{"<style>", "内嵌浏览器不继承应用样式"},
		{"html.escape", "文稿文本必须转义（<script> 会被当脚本执行）"},
		{"from pptx import Presentation", "解析必须在受管运行时里跑 python-pptx"},
		{"EMU_PER_INCH", "几何必须按 EMU→px 换算（近似版式的依据）"},
		{"chOff", "组合形状要做子坐标系→页面坐标系的仿射换算"},
		{"clrMap", "主题配色必须走 clrMap（深色主题靠它把 bg1 映到 dk1）"},
		{"lastClr", "dk1/lt1 是 sysClr（只有 lastClr），漏掉会静默丢色"},
		{"titleStyle", "文本样式继承链要走完（默认模板的字号/对齐都在母版 txStyles 里）"},
		{"marL", "缩进与项目符号的排布依赖 marL/indent"},
		{"近似版式重建（不是渲染）", "做不到的四件事必须写在页面上（诚实标注是产品决策）"},
		{"SmartArt", "不支持的形状要在页面说明里点名"},
		{"MAX_SLIDES", "页数上限"},
		{"MAX_SHAPES_PER_SLIDE", "每页形状数上限"},
		{"MAX_SHAPES_TOTAL", "全篇形状预算（嵌套组合不再每层各算一份 ⇒ 200³）"},
		{"itertools.islice", "形状必须**惰性**取用（list(...)[:N] 会先全量物化）"},
		{"colspan", "合并单元格要给 colspan/rowspan（否则表格布局错位）"},
	} {
		if !strings.Contains(pptxToHTMLScript, want.frag) {
			t.Errorf("pptx 内置脚本缺少 %q —— %s", want.frag, want.why)
		}
	}
	// 反向判据：全量物化再切片的写法一旦回来，形状预算就只挡渲染、不挡解析
	// （声明 100 万 shape 的文件会照建 100 万个 shape 代理对象）。
	// 判据带 `[:`：只有「物化 + 取前 N 个」这个惯用法才是问题所在。
	for _, bad := range []string{"list(slide.shapes)[:", "list(shape.shapes)[:"} {
		if strings.Contains(pptxToHTMLScript, bad) {
			t.Errorf("pptx 内置脚本又出现了全量物化 %q —— 形状预算会失效", bad)
		}
	}
}

// 两个内置脚本的静态形态检查：以 main() 结尾（缺了它脚本什么都不做、标记行不出现 →
// 所有转换都会失败），且不是 Go 模板（脚本是原样写入的 raw string 常量，一旦有人用
// fmt.Sprintf 套它，Python 的 % 格式串会被当成 Go 动词）。
func TestOfficePreviewScriptsAreSelfContained(t *testing.T) {
	for name, script := range map[string]string{"docx": docxToHTMLScript, "pptx": pptxToHTMLScript} {
		if !strings.HasSuffix(strings.TrimSpace(script), "main()") {
			t.Errorf("%s 脚本应以 main() 调用结尾", name)
		}
		if strings.Contains(script, "%!") {
			t.Errorf("%s 脚本里出现了 Go 格式化失败的痕迹（%%!）", name)
		}
		// 脚本不得以 % 开头紧跟 Go 动词的形式暴露成「被格式化过」的产物：
		// 判据是 Python 的格式化调用必须还在（`% (` 或 `%s" %`）。
		if !strings.Contains(script, "%s") && !strings.Contains(script, "%d") {
			t.Errorf("%s 脚本里一个 Python 格式串都没有 —— 大概率被误当成 Go 模板改写过了", name)
		}
	}
}
