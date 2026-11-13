// Package conversion 收口「文档 → HTML 预览产物」的转换：限额、内容寻址缓存、清理。
//
// 为什么要有这一层（R08a）：转换逻辑本来就在 tools/builtin 的三个 previewer 里且是对的
// （1802 行，含大量实测取舍），缺的是**通道级的纪律** —— 一次预览该有多长时限、同时能跑几个、
// 同一份文件打开两次要不要重转、产物堆到多大开始回收。这些不属于任何一个转换器，
// 属于「谁在调它们」。故：previewer 一行不改，把它们的调用收进 Service。
//
// 与 tools/builtin 的分工：
//   - builtin：给定一份文件怎么转出 HTML（解析库、脚本、沙箱、产物落受管 tmp）；
//   - conversion：排队、超时、缓存、回收，以及把结果整理成「一个稳定的产物路径」。
//
// 输出位置（硬约束）：缓存目录在 appdata（~/.go-code/previews），**绝不在工作区**。
// 预览产物不是用户资产，不该出现在 git status / 文件树 / 产出卡片里。
package conversion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 限额默认值（01-design §4.7）。集中在这里，便于测试注入小值并保持生产口径唯一。
const (
	// DefaultConcurrency 同时进行的转换数上限。
	//
	// 为什么是 2：转换是 CPU/IO 双吃的子进程（openpyxl/mammoth/python-pptx），单机同时
	// 跑满 N 个只会让每一个都变慢，且 previewer 的 60s 时限会开始误伤「其实只是排在后面」
	// 的请求。2 = 「来回切两个标签页」仍并发，再多就排队。
	DefaultConcurrency = 2
	// DefaultTimeout 单次转换时限（不含排队时间，见 Convert 注释）。
	DefaultTimeout = 60 * time.Second
	// DefaultMaxInputBytes 输入硬上限。
	//
	// 为什么不单独设一档「50 MiB 软上限」：软上限只有两种落地方式，都不能接受 ——
	// （a）成功但带警告字段 = 改响应形状（本 PR 的硬约束是不改前端/不改形状）；
	// （b）在 50 MiB 就拒 = 与验收口径（100 MiB 才给 too_large）冲突，且会让「99 MiB 的
	// 演示文稿」在能转的情况下被拒。故只有**一道**硬闸：>100 MiB 直接 too_large。
	DefaultMaxInputBytes = 100 << 20
	// DefaultMaxCacheBytes 缓存总量上限（超出按 LRU 回收）。
	DefaultMaxCacheBytes = 512 << 20
	// DefaultMaxCacheAge 缓存条目过期时间（启动时清扫）。
	DefaultMaxCacheAge = 24 * time.Hour
)

// DefaultCacheDir 缓存的默认位置：appdata 下的 previews（不在工作区）。
func DefaultCacheDir(appData string) string { return filepath.Join(appData, "previews") }

// Converter 一次真实转换。实现方自己负责进程管理（现有 previewer → sandbox：
// exec.CommandContext 语义 + 独立进程组 + 超时/取消时 kill 整个进程组）。
//
// 为什么接口只有「一个文件进、一个 HTML 出」：Service 不该知道 openpyxl 还是 mammoth，
// 也不该知道脚本长什么样 —— 它只管排队/超时/缓存。测试注入假实现即可离线覆盖全部限额。
type Converter interface {
	// Convert 把 src（绝对路径）转成 HTML。workspace = 沙箱读写根。
	Convert(ctx context.Context, src, workspace string) (Output, error)
}

// Output 转换产物。
type Output struct {
	HTMLPath string // 产物绝对路径（转换器自己的临时目录里）
	// Cleanup 清掉转换器自己的临时目录（可为 nil）。Service 在把产物复制进缓存之后调用；
	// **只允许删自己的临时目录**，不许碰 src、缓存目录或工作区。
	Cleanup func()
}

// Request 一次转换请求。
type Request struct {
	Path      string            // 输入文件（绝对路径；调用方已完成路径校验）
	Kind      string            // "xlsx" | "docx" | "pptx"
	Workspace string            // 沙箱读写根（不参与缓存键，见 CacheKey）
	Params    map[string]string // 转换参数（参与缓存键；当前三条通路都为空）
}

// Result 转换结果。
type Result struct {
	HTMLPath string        // 产物绝对路径（缓存内）
	Key      string        // 缓存键（诊断/测试用，不进响应信封）
	Cached   bool          // 是否命中缓存（未调用 Converter）
	Took     time.Duration // 本次耗时（含排队与哈希）
}

// Options 服务装配参数（零值字段用默认值）。
type Options struct {
	Converters  map[string]Converter
	CacheDir    string
	Concurrency int
	Timeout     time.Duration
	MaxInput    int64
	MaxCache    int64
	MaxAge      time.Duration
	Now         func() time.Time
}

// Service 转换服务（一个进程一个；并发闸门是进程级，不能每次调用新建）。
type Service struct {
	converters map[string]Converter
	cache      *Cache
	sem        chan struct{}
	timeout    time.Duration
	maxInput   int64
	now        func() time.Time
}

// New 构造服务。Converters 不能为空（kind → 实现）；CacheDir 必填（不在工作区）。
func New(opts Options) (*Service, error) {
	if len(opts.Converters) == 0 {
		return nil, errors.New("conversion: no converters configured")
	}
	if opts.CacheDir == "" {
		return nil, errors.New("conversion: cache dir is required")
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxInput := opts.MaxInput
	if maxInput <= 0 {
		maxInput = DefaultMaxInputBytes
	}
	maxCache := opts.MaxCache
	if maxCache <= 0 {
		maxCache = DefaultMaxCacheBytes
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxCacheAge
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := NewCache(opts.CacheDir, maxCache, maxAge)
	c.now = now
	return &Service{
		converters: opts.Converters,
		cache:      c,
		sem:        make(chan struct{}, conc),
		timeout:    timeout,
		maxInput:   maxInput,
		now:        now,
	}, nil
}

// CacheDir 缓存根目录。
func (s *Service) CacheDir() string { return s.cache.Dir() }

// Sweep 启动时清扫（过期 + LRU）。不存在缓存目录不算错误。
func (s *Service) Sweep() (Stats, error) { return s.cache.Sweep() }

// Convert 转换（或命中缓存）并返回产物路径。
//
// 时序：校验 → 算键 → 查缓存 → **排队** → 起时限 → 调用转换器 → 写缓存。
//
// 两个刻意选择：
//   - 排队不计入时限。否则「5 个预览同时点」时，排在最后的那个会因为前面两个跑满 60s
//     而被判超时，可它其实一次转换都没跑过。时限描述的是「一次转换能跑多久」。
//   - 失败**绝不写缓存**。错误路径（超时/异常/取消）连 meta 都不落 —— 缓存里只有
//     「曾经成功过」的产物，否则用户会反复看到同一个旧的失败，重试也没用。
func (s *Service) Convert(ctx context.Context, req Request) (Result, error) {
	start := s.now()
	out, err := s.convert(ctx, req)
	out.Took = s.now().Sub(start)
	return out, err
}

func (s *Service) convert(ctx context.Context, req Request) (Result, error) {
	conv, ok := s.converters[req.Kind]
	if !ok {
		return Result{}, NewError(CodeInvalidArgument, "不支持的转换类型: "+req.Kind)
	}
	if req.Path == "" {
		return Result{}, NewError(CodeInvalidArgument, "path 为空")
	}
	info, err := os.Stat(req.Path)
	if err != nil {
		return Result{}, NewError(CodeNotFound, "无法读取文件: "+err.Error())
	}
	if !info.Mode().IsRegular() {
		return Result{}, NewError(CodeInvalidArgument, "path 不是普通文件: "+req.Path)
	}
	if info.Size() > s.maxInput {
		return Result{}, NewError(CodeTooLarge, fmt.Sprintf(
			"文件过大（%s > %s 上限），未转换：%s", humanBytes(info.Size()), humanBytes(s.maxInput), filepath.Base(req.Path)))
	}

	key, err := CacheKey(req.Kind, filepath.Base(req.Path), req.Params, req.Path)
	if err != nil {
		return Result{}, failed("读取输入失败: %v", err)
	}
	if p, hit := s.cache.Lookup(key); hit {
		return Result{HTMLPath: p, Key: key, Cached: true}, nil
	}

	// 并发闸门：拿不到槽位时只等 ctx（用户切走/关会话就直接放弃，不留在队列里）。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return Result{}, NewError(CodeCanceled, "转换已取消（等待并发额度时被中断）")
	}
	// 排队期间同一份内容可能已被另一路转完 → 再查一次（省掉一次重复转换）。
	if p, hit := s.cache.Lookup(key); hit {
		return Result{HTMLPath: p, Key: key, Cached: true}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	prod, cerr := conv.Convert(cctx, req.Path, req.Workspace)
	if prod.Cleanup != nil {
		defer prod.Cleanup()
	}
	if cerr != nil {
		return Result{}, s.classify(cctx, ctx, cerr)
	}
	if prod.HTMLPath == "" {
		return Result{}, failed("转换器未返回产物路径")
	}
	f, oerr := os.Open(prod.HTMLPath)
	if oerr != nil {
		return Result{}, failed("读取转换产物失败: %v", oerr)
	}
	defer f.Close()
	p, perr := s.cache.Put(key, f, Meta{Kind: req.Kind, Name: filepath.Base(req.Path), Created: s.now()})
	if perr != nil {
		return Result{}, failed("写入预览缓存失败: %v", perr)
	}
	return Result{HTMLPath: p, Key: key}, nil
}

// classify 把转换器错误翻成带码错误（超时/取消优先于「转换失败」）。
//
// 为什么先看 ctx 再看错误内容：转换器返回的是「脚本没输出成功标记 + 一段输出」，
// 超时与语法错误在文本上长得一样（都不含标记）；只有 ctx 能区分二者。
func (s *Service) classify(cctx, parent context.Context, err error) error {
	var ce *Error
	if errors.As(err, &ce) {
		return err // 转换器自己给了码（如不可用），尊重它
	}
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		return NewError(CodeTimeout, clipErr(fmt.Sprintf(
			"转换超时（超过 %s，已终止转换）: %s", s.timeout, err.Error())))
	case errors.Is(parent.Err(), context.Canceled):
		return NewError(CodeCanceled, clipErr("转换已取消: "+err.Error()))
	default:
		return failed("%s", err.Error())
	}
}

// humanBytes 人类可读字节数（错误文案用；与 bridge 侧同口径）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
