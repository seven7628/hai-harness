package agents

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// 产出文件采集器（Artifacts，2026-09-15）：per-Run 收集**回传结构化 Diff 的工具**落盘的文件
// （write_file/edit_file 写入的文件，run_python 声明的 outputs），供 AgentEnd.Artifacts
// 携带给宿主渲染「本次产出」卡片（本地客户端：路径可直接打开）。
//
// 采集判据是**结构化 Diff**（tools.ToolDiffProvider → engine 填入 events.ToolResponse.Diff，
// 见 tools/types.go），而不是工具名本身：Diff.Path 由工具在自己写盘/执行后算出，是唯一可靠
// 的来源（工具 args 可能是脚本源码、result 可能是 stdout，字符串解析必然误报）。下面的白名单
// 是第二道闸门 —— 未来某个工具若无意中带上 Diff，不会悄悄混进产出卡；新工具接入时在此登记。
//
// 为什么必须**运行中捕获**而不是结束后盘点：
//
//	core.ToolResult 不含 Diff 字段（core/tool.go）——Diff 只存在于事件层的
//	events.ToolResponse（engine 经 ToolDiffProvider 填入）。AgentEnd 组装时无法
//	从 ac 的任何既有状态反推 ± 行数，故必须在 ToolResponse 经过时截获。
//
// 零额外 IO：size/lines/指纹已由**工具侧**算好（write/edit 在写入时从内存内容算，
// tools/builtin/diff.go finalizeDiff；run_python 在执行后按声明的 outputs 读回文件算），
// 本采集器只做内存聚合，不碰文件系统。因此不存在「IO 阻塞事件流」或「大文件拖慢收尾」。
const maxArtifacts = 50 // 单轮产出上限（超出置 ArtifactsTruncated）

// artifactDelta 一次工具调用对某文件的变更记录（原始累积项，未合并）。
type artifactDelta struct {
	path    string
	created bool
	added   int
	removed int
	size    int64
	lines   int64
	sha256  string
}

// artifactCollector 输出累积器（per-Run，AgentContext 持有）。
//
// 并发：并行工具批会由各自 goroutine 并发调用事件 handler（tools/engine.go 每个
// 调用一个 goroutine，handler 在 execute 内被调用），故累积必须加锁。锁只保护
// 累积；最终顺序在 collect 时统一重排（不依赖 goroutine 调度，保证确定性）。
type artifactCollector struct {
	root string // 工作区根（显示路径归一化用；空 = SDK 独立使用，不做相对化）

	mu  sync.Mutex
	acc []artifactDelta
}

// newArtifactCollector 创建采集器；root 为工作区根目录（可为空）。
func newArtifactCollector(root string) *artifactCollector {
	return &artifactCollector{root: root}
}

// wrap 包装事件 handler：旁路观测 ToolResponse（只累积，不修改、不拦截、不阻塞）。
// 返回的 handler 语义与入参完全一致——调用方无感。
func (c *artifactCollector) wrap(next events.EventHandler) events.EventHandler {
	if c == nil {
		return next
	}
	return func(ctx context.Context, e events.Event) {
		if tr, ok := e.(*events.ToolResponse); ok {
			c.observe(tr)
		}
		next(ctx, e)
	}
}

// observe 记录一次成功且带回结构化 Diff 的工具调用（无 IO，仅内存累积）。
func (c *artifactCollector) observe(tr *events.ToolResponse) {
	if c == nil || tr == nil || tr.IsError || tr.Diff == nil || tr.Diff.Path == "" {
		return // 失败调用 / 未回传 Diff 的工具 → 不产生产出
	}
	switch tr.Name {
	case "write_file", "edit_file", "run_python":
	default:
		return // 白名单外（含 bash）不采集：没有结构化路径，字符串解析会误报
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acc = append(c.acc, artifactDelta{
		path:    tr.Diff.Path,
		created: tr.Diff.Created,
		added:   tr.Diff.Added,
		removed: tr.Diff.Removed,
		size:    tr.Diff.Size,
		lines:   tr.Diff.Lines,
		sha256:  tr.Diff.SHA256,
	})
}

// collect 汇总产出清单：合并同路径 → 过滤噪声 → 排序 → 截断 → 归一化。
// 全程纯内存（size/lines/指纹已由工具算好）。返回 nil 表示无产出（AgentEnd 不带字段）。
func (c *artifactCollector) collect() (items []core.Artifact, truncated bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	acc := append([]artifactDelta(nil), c.acc...)
	c.mu.Unlock()
	if len(acc) == 0 {
		return nil, false
	}
	merged := mergeArtifactDeltas(acc)
	merged = filterNoiseArtifacts(merged)
	if len(merged) == 0 {
		return nil, false
	}
	// created 优先（新建更可能是「本轮成果」），其余保持首次出现序
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].created && !merged[j].created
	})
	if len(merged) > maxArtifacts {
		merged, truncated = merged[:maxArtifacts], true
	}
	items = make([]core.Artifact, 0, len(merged))
	for _, m := range merged {
		items = append(items, core.Artifact{
			Name:    filepath.Base(m.path),
			Path:    c.displayPath(m.path),
			Kind:    artifactKind(m.created),
			Added:   m.added,
			Removed: m.removed,
			Size:    m.size,
			Lines:   m.lines,
			SHA256:  m.sha256,
		})
	}
	return items, truncated
}

// mergeArtifactDeltas 合并同路径（保持首次出现序）：
//   - Added/Removed 累加（同文件多次编辑的总变更量）
//   - once created → 恒 created（本轮创建过就是新建，后续 edit 不改性质）
//   - size/lines/sha256 取**最后一次**（代表最终落盘状态）
//
// 注意 append 的 size/lines 可能为 0（增量语义，见 tools/builtin writeFileTool.Call）；
// 0 不覆盖已有的非 0 值——否则「先覆盖写、后 append」会把精确值抹成空。
func mergeArtifactDeltas(acc []artifactDelta) []artifactDelta {
	idx := make(map[string]int, len(acc))
	out := make([]artifactDelta, 0, len(acc))
	for _, d := range acc {
		if i, ok := idx[d.path]; ok {
			m := &out[i]
			m.added += d.added
			m.removed += d.removed
			m.created = m.created || d.created
			if d.size > 0 {
				m.size = d.size
			}
			if d.lines > 0 {
				m.lines = d.lines
			}
			if d.sha256 != "" {
				m.sha256 = d.sha256
			}
			continue
		}
		idx[d.path] = len(out)
		out = append(out, d)
	}
	return out
}

// artifactNoiseDirs 依赖/构建产物目录（不是「本轮成果」，见设计文档 §2.5）。
var artifactNoiseDirs = []string{
	".git/", "node_modules/", "vendor/", "dist/", "build/", ".cache/", ".next/", "__pycache__/",
}

// artifactNoiseSuffixes 工具自身临时文件/备份/日志后缀。
var artifactNoiseSuffixes = []string{".tmp", ".bak", ".log", ".swp"}

// filterNoiseArtifacts 过滤噪声路径：依赖/构建目录、临时/备份文件、隐藏文件、
// 工具自身的原子写临时文件（.atomic-*，见 tools/builtin/lock.go）。
// 不做「智能筛选」（不猜用户意图）——产出了就是事实，是否值得看交给宿主 UI。
func filterNoiseArtifacts(in []artifactDelta) []artifactDelta {
	out := in[:0]
	for _, d := range in {
		p := d.path
		lower := strings.ToLower(filepath.ToSlash(p))
		if isNoiseArtifactPath(lower) {
			continue
		}
		if strings.HasPrefix(filepath.Base(p), ".") {
			continue // 隐藏文件（.DS_Store 等）
		}
		out = append(out, d)
	}
	return out
}

// isNoiseArtifactPath 判定小写、斜杠分隔的路径是否为噪声。
func isNoiseArtifactPath(p string) bool {
	for _, dir := range artifactNoiseDirs {
		if strings.Contains(p, "/"+dir) || strings.HasPrefix(p, dir) {
			return true
		}
	}
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	if strings.HasPrefix(base, ".atomic-") {
		return true // 工具原子写临时文件
	}
	for _, suf := range artifactNoiseSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// displayPath 展示路径归一化：绝对且位于工作区内 → 相对（可读性好）；
// 否则原样返回（工作区外产物保留绝对路径，宿主可直接使用）。
// root 为空（SDK 独立使用未注入 WorkingDir）→ 原样返回，客户端本地路径能力降级但不破。
func (c *artifactCollector) displayPath(p string) string {
	if c.root == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(c.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p // 工作区外：保留绝对路径
	}
	return rel
}

// artifactKind 从 created 标志映射 Kind 取值。
func artifactKind(created bool) string {
	if created {
		return core.ArtifactKindCreated
	}
	return core.ArtifactKindModified
}
