package conversion

import (
	"context"
	"os"

	"github.com/seven7628/hai-harness/tools/builtin"
)

// 本文件是 Service 与「既有转换实现」之间的**唯一**接缝：三个 previewer 一行不改，
// 只是被包成 Converter。转换逻辑（脚本、解析库、沙箱、超时内的进程管理）全部留在
// tools/builtin —— 这一层只做三件事：接产物路径、交回清理函数、把 nil 变成可读错误。

// PreviewerConverters 把三个既有 previewer 组成 kind → Converter 的映射。
//
// 为什么由调用方（bridge）传入 previewer 实例、而不是这里 new 一个：previewer 要用
// 进程级的受管 Python 运行时与按工作区构造的沙箱工厂，二者都在 bridge 侧装配；
// 在这里再建一套等于两个运行时、两份沙箱策略（迟早不同步）。
func PreviewerConverters(x *builtin.XlsxPreviewer, d *builtin.DocxPreviewer, p *builtin.PptxPreviewer) map[string]Converter {
	return map[string]Converter{
		"xlsx": xlsxConverter{pv: x},
		"docx": docxConverter{pv: d},
		"pptx": pptxConverter{pv: p},
	}
}

// Cleanup 的语义（三个适配器共用，写在一次）：
//
// previewer 成功时会**保留**自己的临时目录（产物要留给浏览器 file:// 读，见其 Convert
// 注释）。走到这里时产物已经被复制进缓存目录、转换器的那个目录再没人读，故立刻回收 ——
// 否则每次预览都会在 runtime/tmp 留下一份等小时级年龄清扫才收走的副本（大表就是几 MB
// 的短期双倍占用）。只删 previewer 自己刚建的这一个目录，不碰 src、不碰缓存、不碰工作区。
func cleanupTempDir(dir string) func() {
	return func() { _ = os.RemoveAll(dir) }
}

type xlsxConverter struct{ pv *builtin.XlsxPreviewer }

func (c xlsxConverter) Convert(ctx context.Context, src, ws string) (Output, error) {
	if c.pv == nil {
		return Output{}, NewError(CodeFailed, "受管 Python 运行时未装配，无法预览 xlsx")
	}
	res, err := c.pv.Convert(ctx, src, ws)
	if err != nil {
		return Output{}, err
	}
	return Output{HTMLPath: res.HTMLPath, Cleanup: cleanupTempDir(res.Dir)}, nil
}

type docxConverter struct{ pv *builtin.DocxPreviewer }

func (c docxConverter) Convert(ctx context.Context, src, ws string) (Output, error) {
	if c.pv == nil {
		return Output{}, NewError(CodeFailed, "受管 Python 运行时未装配，无法预览 docx")
	}
	res, err := c.pv.Convert(ctx, src, ws)
	if err != nil {
		return Output{}, err
	}
	return Output{HTMLPath: res.HTMLPath, Cleanup: cleanupTempDir(res.Dir)}, nil
}

type pptxConverter struct{ pv *builtin.PptxPreviewer }

func (c pptxConverter) Convert(ctx context.Context, src, ws string) (Output, error) {
	if c.pv == nil {
		return Output{}, NewError(CodeFailed, "受管 Python 运行时未装配，无法预览 pptx")
	}
	res, err := c.pv.Convert(ctx, src, ws)
	if err != nil {
		return Output{}, err
	}
	return Output{HTMLPath: res.HTMLPath, Cleanup: cleanupTempDir(res.Dir)}, nil
}
