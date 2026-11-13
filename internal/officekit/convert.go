package officekit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/sandbox"
)

// 转换失败的分类错误（bridge 映射成响应信封的稳定码，见 conversion.Code*）。
//
// 为什么用 sentinel 而不是在错误串里认字（"timeout"）：前端要按码分支
// （超时该重试 / 这份文件转不了），字符串匹配是「文案一改就静默失效」的坑。
var (
	// ErrTimeout 单次转换超时（已 kill 进程组）。
	ErrTimeout = errors.New("officekit: 转换超时")
	// ErrCanceled 调用方取消（工作区关闭/应用退出）。
	ErrCanceled = errors.New("officekit: 转换已取消")
	// ErrConvertFailed soffice 退出非零 / 没产出 PDF / 输入不可读。
	ErrConvertFailed = errors.New("officekit: 转换失败")
)

// ConvertOptions 一次转换的可调项（零值 = 生产口径）。
type ConvertOptions struct {
	// Timeout 单次时限；<= 0 → DefaultTimeout。
	Timeout time.Duration
	// ProfileRoot 独立 profile 的父目录（空 = os.TempDir()）。
	ProfileRoot string
}

// ConvertResult 一次转换的产物。
type ConvertResult struct {
	PDFPath string // 产物绝对路径（在 outDir 里）
	Dir     string // = outDir（调用方清理）
	// ProfileDir 本次用的独立 profile（已删除；只留路径做诊断/断言）。
	ProfileDir string
	// Wall 实际墙钟耗时（含冷启）。
	Wall time.Duration
}

// Convert 把 src 转成 PDF，产物落在 outDir，返回 PDF 路径。
//
// 三个不变量（05-preview-media-research §8.2；每一个都对应一次真实事故形态）：
//  1. **profile 隔离**：每个 job 一个 `-env:UserInstallation=file://<tmp>`。共用时第二个
//     并发转换会撞上 LibreOffice 的 user-installation 锁（报 "user installation is locked"
//     或干脆串行阻塞 —— 用户看到的是「转第二个文件就没反应」）；
//  2. **杀进程组**：soffice 会 fork soffice.bin，只杀父进程会留孤儿并占住 profile 目录。
//     这里经 sandbox.RunArgs → runExec，超时/取消都是 kill(-pgid)；
//  3. **产物必须真的存在**：退出码 0 不等于产出（soffice 对不支持的输入常常 0 退出、
//     什么都不写）。所以最后一定 stat 一次，找不到 PDF 就是失败 —— 不许让调用方拿到
//     一个不存在的路径（它会被原样塞进内嵌浏览器，表现为白屏）。
func (k *Kit) Convert(ctx context.Context, src, outDir string, opt ConvertOptions) (ConvertResult, error) {
	if k == nil || k.Bin == "" {
		return ConvertResult{}, ErrUnavailable
	}
	if src == "" || outDir == "" {
		return ConvertResult{}, fmt.Errorf("%w: src/outDir 为空", ErrConvertFailed)
	}
	if info, err := os.Stat(src); err != nil || !info.Mode().IsRegular() {
		return ConvertResult{}, fmt.Errorf("%w: 输入不是可读的普通文件: %s", ErrConvertFailed, filepath.Base(src))
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return ConvertResult{}, fmt.Errorf("%w: 建输出目录: %v", ErrConvertFailed, err)
	}

	// 每个 job 独立 profile：并发不撞锁，进程被杀也不污染下一次（目录 defer 删）。
	profileRoot := opt.ProfileRoot
	if profileRoot == "" {
		profileRoot = os.TempDir()
	}
	profileDir, err := os.MkdirTemp(profileRoot, "lo-profile-")
	if err != nil {
		return ConvertResult{}, fmt.Errorf("%w: 建独立 profile: %v", ErrConvertFailed, err)
	}
	defer os.RemoveAll(profileDir)

	args := []string{
		"--headless", "--norestore", "--nolockcheck", "--nodefault",
		"-env:UserInstallation=" + fileURL(profileDir),
		"--convert-to", "pdf",
		"--outdir", outDir,
		src,
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var exitCode int
	var timedOut, canceled bool
	start := time.Now()
	out, runErr := sandbox.RunArgs(ctx, k.Bin, args, sandbox.ExecSpec{
		Command:  k.Bin,
		Cwd:      outDir,
		Timeout:  timeout,
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Canceled: &canceled,
	})
	wall := time.Since(start)
	if runErr != nil {
		return ConvertResult{}, fmt.Errorf("%w: 启动 soffice: %v", ErrConvertFailed, runErr)
	}
	switch {
	case canceled:
		return ConvertResult{}, ErrCanceled
	case timedOut:
		// 带上部分输出（前 200 字符）：超时原因常常就在里面（缺字体/文件损坏）。
		return ConvertResult{}, fmt.Errorf("%w（%s 上限 %s）: %s", ErrTimeout, k.Bin, timeout, firstLine(out, 200))
	case exitCode != 0:
		return ConvertResult{}, fmt.Errorf("%w（退出码 %d）: %s", ErrConvertFailed, exitCode, firstLine(out, 400))
	}

	pdf, err := findPDF(outDir, src)
	if err != nil {
		return ConvertResult{}, err
	}
	return ConvertResult{PDFPath: pdf, Dir: outDir, ProfileDir: profileDir, Wall: wall}, nil
}

// fileURL 把本地目录转成 LibreOffice 需要的 file URL。
//
// 为什么要 URL 编码（不能简单拼 "file://"+path）：profile 路径来自临时目录，
// 用户名/工作区名里的空格与中文会被 soffice 的 URL 解析截断或当成转义，
// 结果是「参数看起来对、profile 落到了别处」—— 又一个静默共用 profile 的入口。
func fileURL(dir string) string {
	u := url.URL{Scheme: "file", Path: dir}
	return u.String()
}

// findPDF 在 outDir 里找本次产出的 PDF。
//
// 优先 `basename(src).pdf`（soffice 的常规行为）；找不到就退到「目录里唯一的 PDF」。
// 退路为什么存在：部分输入（如 .xls 的某些工件）会按文档标题而不是文件名命名产物。
func findPDF(outDir, src string) (string, error) {
	want := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))+".pdf")
	if info, err := os.Stat(want); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return want, nil
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", fmt.Errorf("%w: 读输出目录: %v", ErrConvertFailed, err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil && info.Size() > 0 {
			found = append(found, filepath.Join(outDir, e.Name()))
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		return "", fmt.Errorf("%w: soffice 未产出 PDF", ErrConvertFailed)
	}
	return found[0], nil
}

// firstLine 取输出里第一行非空内容并截断（错误消息要短，且不能把整个 stderr 灌进信封）。
func firstLine(s string, max int) string {
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		// runExec 在非零退出时会在末尾追加 "[exit: ...]"：那是我们自己加的结局标记，
		// 不是 soffice 的话，跳过它免得错误串变成「[exit: exit status 1]」这种同义反复。
		if line == "" || strings.HasPrefix(line, "[exit:") {
			continue
		}
		if len(line) > max {
			return line[:max] + "…"
		}
		return line
	}
	return "（无输出）"
}

// warmSrc 预热用的一次性输入（40 字节的 txt）。
//
// 为什么是 txt 而不是空文档：空文件会让 soffice 报「读不了」；一段真文本走的是与真实
// 文档相同的初始化路径（字体枚举 + profile 落盘），但不依赖任何用户文件。
const warmSrc = "办公组件预热：这一行不会出现在任何用户的文档里。\n"

// Warm 跑一次空转换，把 fontconfig 与 LibreOffice profile 建起来（05 §8.2 第 3 条）。
//
// 失败不致命：预热的收益是把「首次真实转换 3–6s 冷启」挪到安装/首启期，
// 它失败时真实转换只是重新变慢，功能不受影响 —— 故返回 error 只供日志，不阻断任何路径。
func (k *Kit) Warm(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "lo-warm-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "warm.txt")
	if err := os.WriteFile(src, []byte(warmSrc), 0o600); err != nil {
		return err
	}
	_, err = k.Convert(ctx, src, dir, ConvertOptions{})
	return err
}

// WarmOnce 首次（且仅一次）预热；kit 不可用或预热失败都只是返回错误，不改变可用性。
//
// 只在启动时调用一次（bridge 在探测到 kit 可用后异步跑）：它要起一个 soffice 进程，
// 放在命令循环上是 3–6s 的白等。
func (p *Prober) WarmOnce(ctx context.Context) error {
	kit := p.Kit()
	if kit == nil {
		return ErrUnavailable
	}
	var werr error
	p.warm.Do(func() { werr = kit.Warm(ctx) })
	return werr
}
