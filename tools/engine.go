package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultMaxResponseSize = 20000
	defaultToolTimeout     = 5 * time.Minute
	// toolStuckTimeout 取消后等待工具退出的硬上限：工具忽略 ctx 时不再无限等待
	//（V2 P1-RUNTIME-03：防 Session Shutdown / 进程退出被第三方工具拖住）。
	toolStuckTimeout = 5 * time.Second

	// defaultMaxImageBytes 单张工具结果图片的 data URL 上限（3 MiB ≈ 2.2MB 原始字节）。
	// 取值依据：视觉模型的有效输入分辨率有限（长边 ~1568px 后收益骤减），
	// 而 data URL 会经 stdin 单行上限（bridge maxCommandLineBytes=4MiB）与
	// 会话 jsonl / 事件日志落盘——3MiB 是「够清晰 + 不撞任何链路闸门」的折中。
	defaultMaxImageBytes = 3 << 20
	// defaultMaxResultImages 单次工具结果携带图片数量上限（防一屏截图/整目录读图刷爆上下文）。
	defaultMaxResultImages = 4
)

// Engine 是 ToolEngine 的默认实现：注册、并发受限的顺序/并行执行、事件上报。
type Engine struct {
	tools       map[string]Tool
	maxParallel int
	maxResponse int
	toolTimeout time.Duration // 单次工具执行兜底超时（工具可经 ToolTimeoutProvider 覆盖）

	// 图片闸门（ToolImageProvider 产出经此过滤；0 = 不限）。
	maxImageBytes int // 单张图片 data URL 上限（默认 3MB ≈ 2.2MB 原始字节，视觉模型足够）
	maxImages     int // 单次工具结果图片数量上限（默认 4）

	// taskAlphabet/taskIDPrefix 工具任务 id 生成（tooltask-<8位随机>；事件/占位结果/
	// 前端关联同源）。随机 id 与 subagent 侧 newTaskID 同设计（registry.go taskIDAlphabet）：
	// Engine 随 loop 重建（重启/切模型/切 persona）而重新实例化，递增序列会跨代归零 →
	// tooltask-1/tooltask-2 被多代任务复用，前端按 taskId 匹配历史块时发生串卡
	//（2026-08-30 实证：历代 read_file/todo_add 块被新一代 promote 事件误翻转）。
	taskIDSeq uint64 // 随机 id 冲突兜底序号（crypto/rand 为主，seq 混入保证进程内绝对唯一）

	lock sync.RWMutex
}

// newTaskID 生成 tooltask-<8位随机小写字母数字>（crypto/rand；字母表与 subagent 一致，
// 去 0/o/1/l/i 歧义字符）。失败回退时间戳 + 进程内序号混合（仍保证唯一）。
func (e *Engine) newTaskID() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		return "tooltask-" + string(b[:])
	}
	// crypto/rand 失败（极罕见）：时间戳 + 进程内序号混合兜底
	e.lock.Lock()
	e.taskIDSeq++
	seq := e.taskIDSeq
	e.lock.Unlock()
	return fmt.Sprintf("tooltask-%08x%04x", time.Now().UnixNano()&0xffffffff, seq&0xffff)
}

type Option func(*Engine)

// WithMaxParallel 设置并行执行的最大并发数（默认 8）。
func WithMaxParallel(n int) Option {
	return func(e *Engine) { e.maxParallel = n }
}

// SetMaxParallel 运行时设置并行上限（AgentLoop 装配时把 WithMaxParallelTools 同步给引擎）。
func (e *Engine) SetMaxParallel(n int) {
	if n <= 0 {
		return // 非法值忽略（保持当前）
	}
	e.maxParallel = n
}

// WithMaxResponseSize 设置工具响应的最大截断长度（默认 20000，0 表示不截断）。
func WithMaxResponseSize(n int) Option {
	return func(e *Engine) { e.maxResponse = n }
}

// WithToolTimeout 设置单次工具执行的兜底超时（默认 30s，0 表示不设超时）。
// 未实现 ToolTimeoutProvider 的工具统一受此限制 —— 防单个失控工具挂死整个 agent；
// 实现该接口的工具可覆盖（>0 用自身值，<0 显式不受限）。
func WithToolTimeout(d time.Duration) Option {
	return func(e *Engine) { e.toolTimeout = d }
}

// WithMaxImageBytes 设置单张工具结果图片的 data URL 上限（默认 3MiB，0 = 不限）。
// 超限图片不进模型上下文（结果文本附明示说明，模型可见后有据可依地调整策略）。
func WithMaxImageBytes(n int) Option {
	return func(e *Engine) { e.maxImageBytes = n }
}

// WithMaxResultImages 设置单次工具结果携带图片的数量上限（默认 4，0 = 不限）。
func WithMaxResultImages(n int) Option {
	return func(e *Engine) { e.maxImages = n }
}

func NewToolEngine(opts ...Option) *Engine {
	e := &Engine{
		tools:         make(map[string]Tool),
		maxParallel:   8,
		maxResponse:   defaultMaxResponseSize,
		toolTimeout:   defaultToolTimeout,
		maxImageBytes: defaultMaxImageBytes,
		maxImages:     defaultMaxResultImages,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *Engine) RegisterTool(_ context.Context, tool Tool) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.tools[tool.Name()] = tool
}

func (e *Engine) GetTool(_ context.Context, name string) (Tool, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()
	if t, ok := e.tools[name]; ok {
		return t, nil
	}
	// 未命中 → 外来命名兜底：
	//  1) 无歧义别名（toolAliases）静默解析 —— 模型习惯 Claude Code 的 Bash/Read/Write/…，
	//     这些名字含义唯一，替它翻译不会猜错工具（目标未注册时照常报错，绝不返回 nil 工具）。
	//  2) 其余返回带「did you mean」建议的错误 —— 模型据此自我修正，而不是拿着
	//     "is not registered" 继续瞎猜别的名字（实测 4 次：Bash / TodoWrite / todobash / task_list_check）。
	// 歧义名（如 TodoWrite 既可能指 todo_add 也可能指 todo_update）**不**自动解析：
	// 猜错工具比报错更糟，只给建议。
	// 注意：本方法持 RLock，建议计算必须用下面的纯函数（再取锁即死锁）。
	if alias, ok := toolAliases[name]; ok {
		if t, ok := e.tools[alias]; ok {
			return t, nil
		}
	}
	names := make([]string, 0, len(e.tools))
	for n := range e.tools {
		names = append(names, n)
	}
	sort.Strings(names) // 清单顺序确定（错误文本可断言），同 ToolParams 的理由
	return nil, fmt.Errorf("tool %q is not registered%s", name, toolSuggestion(name, names))
}

// toolAliases 常见「外来命名」→ 本 harness 工具名。只收无歧义的一对一映射：
// 歧义的（如 TodoWrite 既可能指 todo_add 也可能指 todo_update）不自动解析，
// 改为返回带建议的错误，避免替模型猜错工具。大小写敏感（"BASH"/"READ" 走建议路径）。
// 只影响 GetTool 的解析，**不改** ToolParams()（schema 清单仍是本 harness 的原名）。
var toolAliases = map[string]string{
	"Bash": "bash", "Read": "read_file", "Write": "write_file",
	"Edit": "edit_file", "Glob": "glob", "Grep": "grep",
}

const (
	// maxAvailableToolNames 错误里兜底列出的已注册工具名上限（防超长错误文本刷爆上下文）。
	maxAvailableToolNames = 20
	// minToolNameSimilarity 「像同一个工具」的最少相似度（B6 口径：公共前缀 + 最长公共
	// 子串合计值，低于此宁可不猜）。
	minToolNameSimilarity = 3
	// minFileStemSimilarity 「像同一个文件名」的最少**连续**相同字符数（stem 比较，
	// 低于此宁可不猜）：文件名词干很短，只共享 2 个字符（"nope" vs "notes" 的 "no"）
	// 不足以断定模型想读哪个文件。
	minFileStemSimilarity = 3
)

// toolSuggestion 组装「工具未注册」错误的补充说明：可选的 did-you-mean + 有界的可用清单。
// 纯函数：只读入参、不取锁（GetTool 持 RLock 时调用）。
// names 需已排序（GetTool 保证；排序只影响清单顺序，评分本身与顺序无关）。
func toolSuggestion(name string, names []string) string {
	if len(names) == 0 {
		return " (no tools are registered)"
	}
	list := names
	if len(list) > maxAvailableToolNames {
		list = list[:maxAvailableToolNames]
	}
	available := strings.Join(list, ", ")
	if len(names) > len(list) {
		available += fmt.Sprintf(", … (%d tools total)", len(names))
	}
	if best := suggestToolName(name, names); best != "" {
		return fmt.Sprintf("; did you mean %q? (available: %s)", best, available)
	}
	return fmt.Sprintf(" (available: %s)", available)
}

// suggestToolName 为未注册的工具名挑一个「最可能想用」的已注册名；无够像的候选返回 ""。
// 纯函数：只读入参、无锁、无副作用（可安全在 GetTool 的 RLock 内调用）。
// 评分 = 公共前缀长度 + 最长公共子串长度（大小写不敏感），要求至少 minToolNameSimilarity
// 个连续字符相同，否则宁可不猜：
//   - 公共前缀让 todobash → todo_add（而非只看子串命中的 bash）；
//   - 公共子串让 task_list_check → TaskList、check_read_file → read_file（前缀为 0 也能命中）。
//
// 同级取「公共前缀更长 → 公共子串更长 → 名字更短 → 字典序」，保证结果确定（可断言）。
func suggestToolName(name string, names []string) string {
	hits := rankNameMatches(name, names, 1, nil, toolNameCloseEnough)
	if len(hits) == 0 {
		return ""
	}
	return hits[0].name
}

// toolNameCloseEnough B6 原口径：公共前缀 + 最长公共子串合计 ≥ minToolNameSimilarity
// （工具名多是短复合词，前缀本身携带信息，故接受「前缀 + 子串」的合计分）。
func toolNameCloseEnough(m toolNameMatch) bool { return m.prefix+m.sub >= minToolNameSimilarity }

// rankNameMatches 是「工具名」与「文件名」两种 did-you-mean 建议共用的排序核心：
// 对每个候选按公共前缀长度 + 最长公共子串长度打分（大小写不敏感），用 accept 决定
// 「够不够像」，再按 betterThan 排序取前 limit 个（最好在前）。
// cmp 给出「比较用串」：nil = 用候选名本身（工具名场景）；文件名场景传入剥掉扩展名的
// stem（理由见 SuggestSimilarFileNames）。返回项的 name 始终是**原始候选名**（错误文案
// 里要展示的那个）。大小写不敏感全等的候选直接独占返回（最强信号，不再比分数）。
// 纯函数：只读入参、无锁、无副作用、不碰文件系统（GetTool 持 RLock 时也会调用）。
func rankNameMatches(target string, names []string, limit int, cmp func(string) string, accept func(toolNameMatch) bool) []toolNameMatch {
	if limit <= 0 {
		return nil
	}
	targetRunes := []rune(strings.ToLower(target))
	if len(targetRunes) == 0 {
		return nil // 空名无从比较：任何候选都是「0 分」，据它猜等于瞎猜
	}
	hits := make([]toolNameMatch, 0, len(names))
	for _, cand := range names {
		key := cand
		if cmp != nil {
			key = cmp(cand)
		}
		keyRunes := []rune(strings.ToLower(key))
		if len(keyRunes) == 0 {
			continue
		}
		prefix := commonRunePrefix(targetRunes, keyRunes)
		m := toolNameMatch{name: cand, runes: keyRunes, prefix: prefix, sub: 0}
		if prefix == len(targetRunes) && prefix == len(keyRunes) {
			return []toolNameMatch{m} // 全等：唯一合理答案
		}
		m.sub = longestCommonRunes(targetRunes, keyRunes)
		if !accept(m) {
			continue
		}
		hits = append(hits, m)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].betterThan(hits[j]) })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// SuggestSimilarFileNames 为「路径不存在」挑出同目录下最像的条目名（≤limit 个，最好在前）；
// 无够像的候选返回 nil。与 suggestToolName 同源（同一打分、同一确定性排序、同一全等短路），
// 纯函数：只读入参、无锁、不碰文件系统 —— 目录由调用方（tools/builtin）读，
// 本函数不得取任何锁（GetTool 持 RLock 时也走这条路径）。
//
// 与工具名建议有两处刻意收紧，因为文件名比工具名短、且带固定后缀：
//   - 先剥扩展名（fileStem）再比：扩展名只有 2-4 字符，拿全名比时 ".go"/".md" 自身就够
//     3 字命中（"alpha.go" 会命中 "beta.go"），建议沦为噪声并可能把模型带到错文件上；
//   - 门槛只看最长公共**连续**子串（fileStemCloseEnough），不接受「1 字前缀 + 2 字子串」
//     这类弱重叠（"nope" vs "notes" 只共享 "no"）。
func SuggestSimilarFileNames(target string, names []string, limit int) []string {
	hits := rankNameMatches(target, names, limit, fileStem, fileStemCloseEnough)
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.name)
	}
	return out
}

// fileStemCloseEnough 文件名门槛：至少 minFileStemSimilarity 个连续相同字符。
// 词干很短，两三字符的重叠信息量太低，宁可不猜（猜错文件比报错更糟，同 GetTool 取舍）。
func fileStemCloseEnough(m toolNameMatch) bool { return m.sub >= minFileStemSimilarity }

// fileStem 取目录条目的「词干」：去掉最后一个扩展名（"main_test.go" → "main_test"）。
// 无扩展名（Makefile）→ 整个名字；纯隐藏文件（.env / .gitignore）→ ""（无从比较：
// 拿 ".env" 与 ".eslintrc" 比只会产出噪声建议，调用方跳过空 stem）。
// 不用 filepath.Ext 的理由：它对 ".env" 返回整个名字（把文件名的全部当扩展名），
// 这里需要「无 stem = 不猜」的显式语义。
func fileStem(name string) string {
	base := filepath.Base(name)
	if dot := strings.LastIndex(base, "."); dot > 0 {
		return base[:dot]
	}
	if strings.HasPrefix(base, ".") {
		return ""
	}
	return base
}

// toolNameMatch 一次「未注册名 vs 已注册名」的相似度打分（suggestToolName 内部用）。
type toolNameMatch struct {
	name   string
	runes  []rune
	prefix int // 公共前缀长度
	sub    int // 最长公共连续子串长度
}

func (m toolNameMatch) score() int { return m.prefix + m.sub }

// betterThan 同级比较（全部相等时按字典序，保证确定性）。
func (m toolNameMatch) betterThan(o toolNameMatch) bool {
	switch {
	case m.score() != o.score():
		return m.score() > o.score()
	case m.prefix != o.prefix:
		return m.prefix > o.prefix
	case m.sub != o.sub:
		return m.sub > o.sub
	case len(m.runes) != len(o.runes):
		return len(m.runes) < len(o.runes)
	}
	return m.name < o.name
}

// commonRunePrefix 两个 rune 序列的公共前缀长度。
func commonRunePrefix(a, b []rune) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// longestCommonRunes 最长公共连续子串长度（滚动 DP，O(len(a)·len(b))；工具名很短，开销可忽略）。
func longestCommonRunes(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	best := 0
	for i := 1; i <= len(a); i++ {
		for j := range cur {
			cur[j] = 0 // 本行重置（prev/cur 交替复用）
		}
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			}
		}
		prev, cur = cur, prev
	}
	return best
}

// ToolParams 返回所有已注册工具的厂商无关 schema，按工具名字典序排序。
// 排序是缓存命中的前提：map 遍历顺序随机，顺序抖动会让同一会话多轮请求的
// 工具 schema 前缀每次失配（provider 缓存按字节前缀匹配）。勿改为无序遍历。
func (e *Engine) ToolParams(_ context.Context) ([]core.ToolSchema, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	names := make([]string, 0, len(e.tools))
	for name := range e.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	schemas := make([]core.ToolSchema, 0, len(e.tools))
	for _, name := range names {
		t := e.tools[name]
		schemas = append(schemas, core.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return schemas, nil
}

// Sequence 顺序执行，结果与输入调用顺序对齐（薄封装：审批 + runBatch 串行语义）。
// 执行前统一审批：需要人工确认的调用先收集、逐个请求决策（整轮暂停），
// 拒绝/超时的调用 Block 回传（不执行），其余正常执行。非并行工具（CanParallel=false）
// 在 runBatch 内按顺序启动（前一个完成后一个才开始），保持原串行语义。
func (e *Engine) Sequence(ctx context.Context, calls []core.ToolCall, handler events.EventHandler) ([]core.ToolResult, error) {
	decisions, err := e.preApprove(ctx, calls, handler)
	if err != nil {
		return nil, err
	}
	return e.runBatch(ctx, calls, handler, decisions, nil, 0), nil
}

// Parallel 并行执行（受 maxParallel 约束），结果与输入调用顺序对齐（薄封装：
// 审批 + runBatch 并行语义）。
// 审批语义与 Sequence 相同（执行前统一收集，并行组共享同一决策表）。
func (e *Engine) Parallel(ctx context.Context, calls []core.ToolCall, handler events.EventHandler) ([]core.ToolResult, error) {
	decisions, err := e.preApprove(ctx, calls, handler)
	if err != nil {
		return nil, err
	}
	return e.runBatch(ctx, calls, handler, decisions, nil, 0), nil
}

// RunBatch 批执行（agents.runTools 走此）：统一审批 + runBatch 核心，支持运行时 promote。
// batch 为批控制器（Session.PromoteTask 经此摘离运行中工具）；promoteAfter > 0 = 自动摘离阈值
// （决策 #5，默认关）。decisions 非 nil = 调用方已批前统一审批（跳过；runTools 传 nil，
// 由本方法审批——与 Sequence/Parallel 审批语义一致）。
func (e *Engine) RunBatch(ctx context.Context, calls []core.ToolCall, handler events.EventHandler,
	decisions map[string]bool, batch *BatchController, promoteAfter time.Duration) ([]core.ToolResult, error) {
	rich := make(map[string]approvalDecision, len(decisions))
	for id, ok := range decisions {
		rich[id] = approvalDecision{approved: ok}
	}
	if decisions == nil {
		var err error
		rich, err = e.preApprove(ctx, calls, handler)
		if err != nil {
			return nil, err
		}
	}
	return e.runBatch(ctx, calls, handler, rich, batch, promoteAfter), nil
}

// runBatch 逐调用建 toolTask 执行（决策 #2：每调用一个独立 goroutine + reparentable ctx），
// 等待可被 promote 摘离。返回结果与 calls 对齐：
//   - 正常完成 → 真实结果（与输入顺序一致）；
//   - promote 摘离 → 占位 {task_id,status:"running"}（真实结果稍后经 onDone 自动回传）；
//   - ctx 取消（run abort / 批超时）→ 未摘离任务取消（结果文本 = 取消原因），已摘离任务存活。
//
// 启动语义由工具自声明驱动：非并行工具（CanParallel=false）按序启动（前一个完成后一个
// 才开始——原 Sequence 语义）；并行工具立即启动，受 maxParallel 信号量约束（原 Parallel 语义）。
// promoteAfter > 0 时：运行超过阈值（工具可 DisableAutoPromote / AsyncPromoteAfter 覆盖）的
// 任务自动摘离（决策 #5）。batch nil = 不支持 promote（Sequence/Parallel 直跑语义）。
func (e *Engine) runBatch(ctx context.Context, calls []core.ToolCall, handler events.EventHandler,
	decisions map[string]approvalDecision, batch *BatchController, promoteAfter time.Duration) []core.ToolResult {

	results := make([]core.ToolResult, len(calls))
	tasks := make([]*toolTask, len(calls))
	doneCh := make(chan int, len(calls)) // 完成通知：goroutine 完成后投递下标（promote 后完成据 promoted 区分）
	var sem chan struct{}
	if e.maxParallel > 0 {
		sem = make(chan struct{}, e.maxParallel)
	}
	var promoteCh <-chan string
	if batch != nil {
		promoteCh = batch.promoteCh
	}

	pending := len(calls)
	// 一次性启动全部（promote 信号才能在任一任务运行期间被处理——串行等待必须移到
	// goroutine 内，否则慢的第一个串行工具会卡住启动循环，promote 无处响应）。
	// 串行语义（决策 #2 保留 Sequence）：非并行工具等上一个串行任务完成后再执行。
	isSerial := make([]bool, len(calls))
	serialPrev := make([]int, len(calls))
	lastSerial := -1
	for i, c := range calls {
		if tool, err := e.GetTool(ctx, c.Name); err == nil {
			isSerial[i] = !tool.CanParallel()
		}
		serialPrev[i] = lastSerial
		if isSerial[i] {
			lastSerial = i
		}
	}

	launch := func(i int, c core.ToolCall) {
		rctx := core.NewReparentable(ctx)
		t := &toolTask{
			taskID: e.newTaskID(),
			index:  i,
			call:   c,
			rctx:   rctx,
			done:   make(chan struct{}),
		}
		// 单次任务时限：工具级 ToolTimeout/ToolTimeoutProvider 声明 → PerCallTimeout（读本次
		// 参数，如 bash timeout）覆盖，落在可摘离的 liftableTimeout 上——promote 时可 Lift
		// 解除（后台任务不限时），且解除对下层工具动态生效。
		taskCtx := context.Context(rctx.Context())
		if timeout := e.timeoutFor(ctx, c); timeout > 0 {
			t.lb = newLiftableTimeout(rctx.Context(), timeout)
			taskCtx = t.lb
		}
		t.taskCtx = taskCtx
		tasks[i], results[i].Id = t, c.Id
		if batch != nil {
			batch.mu.Lock()
			batch.tasks[t.taskID] = t
			batch.mu.Unlock()
		}
		go func(i int, t *toolTask) {
			// 串行批：等上一个串行任务完成（保持 Sequence 语义）；并行工具跳过。
			// 等待中取消 → 取消结果（防等待循环 deadlock）。
			if isSerial[i] && serialPrev[i] >= 0 {
				select {
				case <-tasks[serialPrev[i]].done:
				case <-rctx.Context().Done():
					t.result = core.ToolResult{Id: t.call.Id, Result: rctx.Context().Err().Error(), IsError: true}
					close(t.done)
					doneCh <- i
					return
				}
			}
			t.started.Store(time.Now().UnixNano())
			if sem != nil {
				select { // 信号量 ctx 感知：槽等待中取消 → 取消结果（防等待循环 deadlock）
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-rctx.Context().Done():
					t.result = core.ToolResult{Id: t.call.Id, Result: rctx.Context().Err().Error(), IsError: true}
					close(t.done)
					doneCh <- i
					return
				}
			}
			t.result = e.execute(t.taskCtx, t.call, handler, decisions, t.taskID)
			t.stopTimeout() // 收尾：停时限盒（防 timer/watcher 泄漏）
			close(t.done)
			// promote 后完成：结果自动回传（close 之后保证结果已写；未摘离则不推、走收集）。
			// deliverOnce 与等待循环的 done-check 补发竞态安全（只交付一次）。
			if t.promoted.Load() && batch != nil {
				t.deliver(batch)
			}
			doneCh <- i
		}(i, t)
	}
	for i, c := range calls {
		launch(i, c)
	}

	// 自动摘离阈值（决策 #5）：每任务有效阈值 = 工具 AsyncPromoteAfter() 覆盖 / DisableAutoPromote
	// 禁用 / 否则批级 promoteAfter。deadline 锚 = 任务实际开始时间。
	autoAfter := make([]time.Duration, len(calls))
	autoAny := false
	for i, c := range calls {
		aa := promoteAfter
		if tool, err := e.GetTool(ctx, c.Name); err == nil {
			if da, ok := tool.(DisableAutoPromote); ok && da.DisableAutoPromote() {
				aa = 0 // 工具禁用自动摘离（用户主动 promote 仍可用）
			}
			if at, ok := tool.(AsyncPromoteAfter); ok {
				switch v := at.AsyncPromoteAfter(); {
				case v > 0:
					aa = v // 工具覆盖批级阈值
				case v < 0:
					aa = 0 // 显式禁用
				}
			}
		}
		autoAfter[i] = aa
		if aa > 0 {
			autoAny = true
		}
	}

	// markPromoted 摘离标记：promoted + Detach（存活过 run 结束/abort，决策 #3/#8）
	// + 解除时限（promote 后台任务 = 不限时，liftableTimeout.lift 对下层工具动态生效）。
	// 手动路径在 BatchController.Promote 同步执行（Promote 返回即已 Detach）；
	// 自动路径在定时器触发时执行。走 batch.detach（计数与手动路径统一、幂等）。
	markPromoted := func(t *toolTask) {
		if batch != nil {
			batch.detach(t)
			return
		}
		t.promoted.Store(true)
		t.rctx.Detach()
		if t.lb != nil {
			t.lb.lift()
		}
	}
	// finishPromote 占位结果 + pending-- + task_promoted 事件（每任务只写一次：
	// phOnce 保证手动信号与自动摘离、ctx 取消补写并发安全，不双重占位/不重复事件）。
	finishPromote := func(t *toolTask) {
		t.phOnce.Do(func() {
			results[t.index] = placeholderResult(t)
			pending--
			if handler != nil {
				handler(ctx, &events.TaskPromoted{
					TaskId:    t.taskID,
					Kind:      "tool",
					Name:      t.call.Name,
					Timestamp: time.Now(),
					EventType: events.TaskPromotedType,
				})
			}
		})
		// 竞态补发：promote 与完成同一瞬间（goroutine 的 promoted 检查先读到 false 而
		// 未交付）→ 此处已见 done → 补发交付（deliverOnce 保证只一次）。任务未完成则
		// 默认分支跳过，由 goroutine 完成路径交付。
		select {
		case <-t.done:
			if batch != nil {
				t.deliver(batch)
			}
		default:
		}
	}

	// 自动摘离定时器：武装到所有未摘离任务里最早的有效到期点；无 → 关闭（autoC nil）。
	var autoTimer *time.Timer
	autoC := func() <-chan time.Time {
		if autoTimer != nil {
			autoTimer.Stop()
			autoTimer = nil
		}
		if !autoAny {
			return nil
		}
		var next time.Time
		now := time.Now()
		for i, t := range tasks {
			if t == nil || t.promoted.Load() || autoAfter[i] <= 0 {
				continue
			}
			base := now
			if s := t.started.Load(); s > 0 {
				base = time.Unix(0, s)
			}
			// 未开始（串行批排队中）也武装：假设现在开始 → 定时器到 now+autoAfter 触发，
			// 触发时仍未开始则重武装（轮询直到任务真正开始，按实际开始时间计）。
			dl := base.Add(autoAfter[i])
			if !dl.After(now) {
				dl = now // 已到期：立即触发
			}
			if next.IsZero() || dl.Before(next) {
				next = dl
			}
		}
		if next.IsZero() {
			return nil
		}
		autoTimer = time.NewTimer(time.Until(next))
		return autoTimer.C
	}

	// 等待循环：收集已完成 / promote 摘离 / ctx 取消 / 自动摘离阈值
	for pending > 0 {
		select {
		case <-ctx.Done():
			// run abort / 批超时：未摘离任务取消；摘离任务不管（已 Detach，存活）
			for _, t := range tasks {
				if t != nil && !t.promoted.Load() {
					t.rctx.Cancel()
				}
			}
			// 硬上限等待（V2 P1-RUNTIME-03）：工具可能忽略 ctx 取消（第三方/插件工具
			// 不响应）——每个任务最多等 toolStuckTimeout（从取消起算），超时返回
			// tool_stuck 错误结果，绝不让 RunBatch 无限阻塞拖住 Session Shutdown / 进程退出。
			for i, t := range tasks {
				if t == nil {
					continue
				}
				if t.promoted.Load() {
					finishPromote(t) // 占位补写（promote 信号可能未及处理；phOnce 幂等）
					continue
				}
				select {
				case <-t.done:
					results[i] = t.result // 取消结果文本（execute 产出）
				case <-time.After(toolStuckTimeout):
					results[i] = core.ToolResult{
						Id:      t.call.Id,
						IsError: true,
						Result:  "TOOL_ERROR\ncode: tool_stuck\nnext_action: review\ndetail: tool did not respond to cancellation within timeout\nfix: avoid this tool or run it in background",
					}
					// 超时后不再等（goroutine 仍会结束，结果丢弃；进程不被拖住）
				}
			}
			if batch != nil {
				batch.release() // 批已返回：无在途摘离任务 → 立即回收（注销精确中断路由）
			}
			return results

		case id := <-promoteCh:
			batch.mu.Lock()
			t := batch.tasks[id]
			batch.mu.Unlock()
			if t != nil {
				finishPromote(t) // 标记 + Detach 已在 BatchController.Promote 同步完成
			}

		case i := <-doneCh:
			if tasks[i].promoted.Load() {
				continue // 已摘离：结果已走 onDone，占位已写入，不影响 pending
			}
			results[i] = tasks[i].result
			pending--

		case <-autoC():
			now := time.Now()
			for i, t := range tasks {
				if t == nil || t.promoted.Load() || autoAfter[i] <= 0 {
					continue
				}
				s := t.started.Load()
				if s > 0 && time.Unix(0, s).Add(autoAfter[i]).Before(now) {
					markPromoted(t)
					finishPromote(t)
				}
			}
		}
	}
	if batch != nil {
		batch.release() // 批已返回：无在途摘离任务 → 立即回收（注销精确中断路由）
	}
	return results
}

// DisableAutoPromote 可选接口（决策 #5）：工具声明永不被自动摘离（用户主动 promote 仍可用）。
type DisableAutoPromote interface {
	DisableAutoPromote() bool
}

// AsyncPromoteAfter 可选接口（决策 #5）：工具声明自动摘离阈值覆盖。
// >0 = 覆盖批级阈值；<0 = 显式禁用（同 DisableAutoPromote）；0 = 用批级阈值。
type AsyncPromoteAfter interface {
	AsyncPromoteAfter() time.Duration
}

// preApprove 工具轮执行前统一审批：收集需要确认的调用（ApprovalRequired 声明），
// 逐个发 ToolApprovalRequested 事件并等待用户决策。
// 返回 Id → 批准 的决策表（nil = 本轮无审批）；拒绝/超时 = false（调用将 Block）。
// approvalDecision 批前审批结论：approved + 「窗口用尽、无人决策」（后者必须与「拒绝」
// 分开措辞 —— 见 events.ErrApprovalTimeout）。
type approvalDecision struct {
	approved bool
	timedOut bool
}

func (e *Engine) preApprove(ctx context.Context, calls []core.ToolCall, handler events.EventHandler) (map[string]approvalDecision, error) {
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Approver == nil {
		return nil, nil // 未注入审批（现状：不审批）
	}
	var pending []core.ToolCall
	for _, c := range calls {
		if tool, err := e.GetTool(ctx, c.Name); err == nil {
			needs := false
			if ar, ok := tool.(ApprovalRequired); ok && ar.RequiresApproval(ctx, c) {
				needs = true // 工具自声明需审批（如 bash）——现状路径
			}
			if tc != nil {
				if ap, ok := tc.Approver.(events.ApprovalPolicy); ok {
					// 模式策略覆盖：Auto → 不发起审批；Block/Required → 纳入审批通道
					// （能让 manual 模式强制审批不自声明的 write/edit）
					needs = ap.NeedsApproval(c)
				}
			}
			if needs {
				pending = append(pending, c)
			}
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	decisions := make(map[string]approvalDecision, len(pending))
	for _, c := range pending {
		// 只有 ContextualApprover 能在注册阶段感知当前运行 ctx；Session 级
		// approvalWaiter 会在这里等待 FIFO 队首，因此非队首请求不会提前发事件。
		var (
			wait func(context.Context) (bool, error)
			err  error
		)
		if ca, ok := tc.Approver.(events.ContextualApprover); ok {
			wait, err = ca.BeginApprovalWithContext(ctx, c)
		} else {
			// 兼容旧 Approver：保持原有「注册后发事件」调用协议。
			wait = tc.Approver.BeginApproval(c)
		}
		if err != nil {
			return nil, err
		}
		if wait == nil {
			return nil, fmt.Errorf("approver returned nil wait function for call %q", c.Id)
		}
		if handler != nil {
			handler(ctx, &events.ToolApprovalRequested{
				RunId:     tc.RunId,
				RequestId: c.RequestId,
				Id:        c.Id,
				Index:     c.Index,
				Name:      c.Name,
				Arguments: c.Arguments,
				Timestamp: time.Now(),
				EventType: events.ToolApprovalType,
			})
		}
		ok, err := wait(ctx)
		if errors.Is(err, events.ErrApprovalTimeout) {
			// 窗口用尽、用户没做决定：记「超时未决」并继续处理本批其余调用
			//（不是本轮审批失败 —— abort 等真错误才中断）。
			decisions[c.Id] = approvalDecision{timedOut: true}
			continue
		}
		if err != nil {
			return nil, err // 打断（ctx 取消）等：本轮审批失败
		}
		decisions[c.Id] = approvalDecision{approved: ok}
	}
	return decisions, nil
}

// timeoutFor 单次调用的时限：工具级 ToolTimeout/ToolTimeoutProvider 声明 → PerCallTimeout
// （可读本次参数，如 bash timeout）覆盖。返回 0 = 不限时。时限由 runBatch 落到
// liftableTimeout 上（promote 可解除），execute 不再自行 WithTimeout（防双层定时器
// 快照——lift 只对最外层生效）。
func (e *Engine) timeoutFor(ctx context.Context, c core.ToolCall) time.Duration {
	timeout := e.toolTimeout
	tool, err := e.GetTool(ctx, c.Name)
	if err != nil {
		return timeout
	}
	if tp, ok := tool.(ToolTimeoutProvider); ok {
		switch t := tp.ToolTimeout(); {
		case t > 0:
			timeout = t // 工具声明自身时限（信任工具作者）
		case t < 0:
			timeout = 0 // 显式不受限（如子 agent：生命周期由 abort/ctx 级联管理）
		}
	}
	if pt, ok := tool.(PerCallTimeout); ok {
		if v := pt.TimeoutForArgs(c.Arguments); v != 0 {
			timeout = v
		}
	}
	return timeout
}

// execute 单次工具调用全流程：参数校验 → 拦截 → 执行 → 截断，全程事件上报。
// 实现 ToolUsageProvider 的工具（如子 agent）用量随结果与事件回传。
// 超时兜底：runBatch 已按本调用时限包了 liftableTimeout（见 timeoutFor 注释）；
// 防单个失控工具挂死整个 agent（LLM 调用有超时+重试，工具调用同样必须有兜底）。
// taskID 为该调用在批内的工具任务 id（tooltask-<seq>；ToolStart 携带，前端关联后台任务）。
func (e *Engine) execute(ctx context.Context, call core.ToolCall, handler events.EventHandler, decisions map[string]approvalDecision, taskID string) core.ToolResult {
	// 审批决策：拒绝/超时未决 = Block 回传（模型可见后可自行调整）。
	// 两者措辞必须不同：超时是「没人决策」，不是「用户否决」——否则模型会以为用户否掉了它
	//（用户报障同一原则：「超时之后要让 LLM 在上下文里知道用户未填写，否则模型会很疑惑」）。
	if decisions != nil {
		if d, exists := decisions[call.Id]; exists && !d.approved {
			msg := "user rejected the tool call (approval denied)"
			if d.timedOut {
				msg = "approval timed out with no decision from the user: nobody approved or rejected this call in time, so it did NOT run. " +
					"Tell the user it is still pending (they may have been away or still reading), then either ask again or propose an alternative — do not treat this as a rejection."
			}
			return e.finish(ctx, handler, call, finishInput{result: msg, isErr: true})
		}
	}

	tool, err := e.GetTool(ctx, call.Name)
	if err != nil {
		return e.finish(ctx, handler, call, finishInput{result: err.Error(), isErr: true})
	}

	if handler != nil {
		tc := events.ToolContextFrom(ctx)
		handler(ctx, &events.ToolStart{
			RequestId: call.RequestId,
			Index:     call.Index,
			Id:        call.Id,
			Name:      call.Name,
			Arguments: call.Arguments, // 实际执行的参数 JSON（UI 展示「传了什么」）
			RunId:     runOf(tc),
			TaskId:    taskID,
			Timestamp: time.Now(),
			EventType: events.ToolRunStartType,
		})
	}

	// 参数语法修复（jsonpair）：LLM 的 tool arguments 常有流式截断/单引号/尾逗号等
	// 语法问题，先做语法级安全修复（只动语法不动语义，修不出合法 JSON 则原样回退），
	// 再用修复后的参数校验+执行。历史 ToolCall 保留原参（审计），ToolResponse.Arguments
	// 带实际执行参数（与 PreToolUse 改写同语义）。
	call.Arguments = RepairArguments(call.Arguments)

	if err := tool.ValidParams(ctx, call.Name, call.Arguments); err != nil {
		// 明确回传失败原因：合法 JSON 但缺 schema 必填字段 → 中央层列出缺失字段
		//（全部工具 Parameters() 都声明 required，无需改各工具手写校验器）；
		// 非合法 JSON → 提示语法仍非法（修复失败）。
		msg := "invalid params: " + err.Error()
		if hint := missingFieldsHint(tool, call.Arguments); hint != "" {
			msg += "; " + hint
		} else if !json.Valid([]byte(call.Arguments)) {
			msg += "; arguments is not valid JSON (auto-repair failed)"
		}
		return e.finish(ctx, handler, call, finishInput{result: msg, isErr: true})
	}

	// 拦截点：BeforeCall 返回 Block 时跳过执行（如用户确认场景）
	if resp := tool.BeforeCall(ctx, call); resp.Block {
		return e.finish(ctx, handler, call, finishInput{result: "blocked: " + resp.Reasoning, isErr: true})
	}

	defer tool.AfterCall(ctx, call)

	// 图片收集槽：每次调用独立（放 ctx）——并行批不得共享暂存，否则结果会张冠李戴
	//（见 tools.ImageSink 注释）。工具经 ImageSinkFrom(ctx).Add(...) 报告图片。
	sink := &ImageSink{}
	// 命令执行结局收集槽：同为「每次调用独立放 ctx」范式（ExecSink）——命令类工具
	// （bash 等）在 Call 内上报退出码/超时，引擎据此把「失败/超时」升级为结构化信号，
	// 而 **Call 仍返回 nil error**（否则下面 err != nil 分支会丢弃整个 out，非目标 1）。
	execSink := &ExecSink{}
	out, err := callTool(tool, WithExecSink(WithImageSink(ctx, sink), execSink), call)
	if err != nil {
		return e.finish(ctx, handler, call, finishInput{result: err.Error(), isErr: true})
	}

	usage := core.Usage{}
	if up, ok := tool.(ToolUsageProvider); ok {
		usage = up.Usage() // 子 agent 类工具回传本次执行用量
	}
	var diff *core.FileDiff
	if dp, ok := tool.(ToolDiffProvider); ok {
		diff = dp.Diff() // write/edit 类工具回传本次调用对文件的变更（事件层，宿主渲染）
	}
	// 执行结局（命令类工具经 ExecSink 上报；非命令类 ok=false → 结构体 nil，行为不变）。
	// 判定规则：超时/取消（输出为部分）必判失败；非零退出且非「良性非零退出」命令判失败
	// （grep 无匹配/diff 有差异的 exit 1 是正常结果，见 benignNonZeroExit）。
	// 注意：isErr 只影响 IsError/事件 code，**不影响 r.ToolError** —— 工具执行类失败不在此处
	// 写结构化 ToolError（只有工具自己产出的 TOOL_ERROR 信封才写）。
	// 2026-09-18（C5）更新：去重守卫（repeated_failed_call）**不再依赖 ToolError** —— 改为
	// per-run 失败账本（记录**所有** IsError 结果）+ **收窄的拦截条件**（「紧邻的上一轮工具轮」
	// 且「其间无状态变更」）。合法重跑（改完代码再 go build/go test）由「状态变更即清账」保护，
	// 见 agents/fileledger.go 的 failureLedger 与 FIX_PLAN_BATCH1.md 非目标 2。
	var execStatus *core.ExecStatus
	isErr := false
	if cmdText, code, timedOut, canceled, ok := execSink.Exit(); ok {
		execStatus = &core.ExecStatus{ExitCode: code, TimedOut: timedOut, Canceled: canceled}
		switch {
		case timedOut || canceled:
			isErr = true // 超时/取消：输出为部分，必须判失败
		case code != 0 && !benignNonZeroExit(cmdText, code):
			isErr = true // 真实失败（go build/test/lint 等）
		}
	}
	// 图片类工具（read_file 读图 / 截图 / MCP image）回传图片内容块：文本照常截断，
	// 图片按 maxImageBytes 逐张过闸（超限丢弃 + 文本说明，模型知道有图但拿不到，
	// 比静默塞爆上下文好——对齐 provider.DowngradeImages 的「明示降级」语义）。
	// 失败路径同样过 truncate（超时的部分输出可能超长）。
	text, images := e.limitImages(e.truncate(out), sink)
	return e.finish(ctx, handler, call, finishInput{
		result: text,
		isErr:  isErr,
		usage:  usage,
		diff:   diff,
		images: images,
		exec:   execStatus,
	})
}

// limitImages 取本次调用收集的图片并做体积/数量闸门（tools.ImageSink）。
// 返回 (结果文本, 图片块)：无图片 → 文本原样、图片 nil。
// 被丢弃的图片在文本追加说明——模型明确知道「有图但我看不到」，可自行调整
// （降分辨率 / 换文本路径），而不是完全无从察觉。
func (e *Engine) limitImages(text string, sink *ImageSink) (string, []core.Content) {
	imgs := sink.Images()
	if len(imgs) == 0 {
		return text, nil
	}
	kept := make([]core.Content, 0, len(imgs))
	dropped := 0
	for _, c := range imgs {
		if c.Type != core.ContentTypeImage || c.Content == "" {
			continue
		}
		if e.maxImages > 0 && len(kept) >= e.maxImages {
			dropped++
			continue
		}
		if e.maxImageBytes > 0 && len(c.Content) > e.maxImageBytes {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	if dropped == 0 {
		return text, kept
	}
	note := fmt.Sprintf("\n\n[%d image(s) omitted: exceeds the per-image or per-result size/count limit (max %s/image, %d images/result)]",
		dropped, humanBytes(e.maxImageBytes), e.maxImages)
	if len(kept) == 0 {
		return text + note, nil
	}
	return text + note, kept
}

// humanBytes 人类可读字节数（错误提示用）。
func humanBytes(n int) string {
	switch {
	case n <= 0:
		return "unlimited"
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n>>20)
	default:
		return fmt.Sprintf("%dKB", n>>10)
	}
}

// callTool 执行工具 Call 并把 panic 转为错误（工具崩溃不炸 agent 主流程，
// 错误进结果文本回传 LLM —— 模型可见后可自行调整）。
func callTool(tool Tool, ctx context.Context, call core.ToolCall) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool %s panicked: %v", call.Name, r)
		}
	}()
	return tool.Call(ctx, call.Name, call.Arguments)
}

// finishInput finish 的入参（字段多，用具名结构避免位置参数拼错）。
type finishInput struct {
	result string
	isErr  bool
	usage  core.Usage
	diff   *core.FileDiff
	images []core.Content   // 图片类工具产出的内容块（ToolImageProvider）
	exec   *core.ExecStatus // 命令类工具的执行结局（ExecSink 旁路；nil = 不适用/未采集）
}

// finish 构造结果并发出 ToolResponse 事件。
// Arguments 携带实际执行的参数（PreToolUse 改写后；历史 ToolCall 保留原参——审计用）。
// diff 为本次执行对文件的变更（ToolDiffProvider 产出；事件层携带，不进 LLM 上下文）。
func (e *Engine) finish(ctx context.Context, handler events.EventHandler, call core.ToolCall, in finishInput) core.ToolResult {
	r := core.ToolResult{Id: call.Id, Result: in.result, IsError: in.isErr, Usage: in.usage, Blocks: buildResultBlocks(in.result, in.images), Exec: in.exec}
	if in.isErr {
		r.ToolError = parseToolError(in.result)
	}
	if handler != nil {
		tc := events.ToolContextFrom(ctx)
		// 事件 code：信封优先，否则兜底分类（兜底只进事件/遥测，绝不写 r.ToolError ——
		// 见 fallbackErrorCode 注释与 FIX_PLAN_BATCH1.md 非目标 2）。
		code := errorCode(r.ToolError)
		if code == "" && in.isErr {
			// 取消判定**先于**文本兜底：取消是结构化事实（Exec.Canceled = 父 ctx 被取消，
			// 见 B5），而取消的输出文本往往同时含 "timed out"/"context canceled"，交给
			// fallbackErrorCode 只会落 unclassified —— 指标/看板就无法按 code 统计取消
			//（替代口径 Exec.Canceled 只在事件里，跨工具聚合要特判）。
			// 与 timeout 严格区分：Exec.TimedOut 仍走文本兜底（"deadline exceeded" → timeout），
			// 二者对模型是不同事实（取消不是命令的问题，也不该建议调大 timeout）。
			if in.exec != nil && in.exec.Canceled {
				code = errorCodeCanceled
			} else {
				code = fallbackErrorCode(in.result)
			}
		}
		handler(ctx, &events.ToolResponse{
			RequestId:  call.RequestId,
			Index:      call.Index,
			Id:         call.Id,
			Name:       call.Name,
			RunId:      runOf(tc),
			Arguments:  call.Arguments,
			Result:     r.Result,
			IsError:    r.IsError,
			ErrorCode:  code,
			Changed:    errorChanged(r.ToolError),
			Retryable:  errorRetryable(r.ToolError),
			NextAction: errorNextAction(r.ToolError),
			Recovery:   errorRecovery(r.ToolError),
			Diff:       in.diff,
			Images:     in.images,
			Usage:      &r.Usage,
			Exec:       in.exec,
			Timestamp:  time.Now(),
			EventType:  events.ToolRunEndType,
		})
	}
	return r
}

// buildResultBlocks 组装结果的完整内容块：文本块（结果文本）+ 图片块。
// 无图片 → nil（保持 Result 单文本语义，ToolResult.Blocks 空 = 纯文本，向后兼容）。
func buildResultBlocks(text string, images []core.Content) []core.Content {
	if len(images) == 0 {
		return nil
	}
	blocks := make([]core.Content, 0, len(images)+1)
	if text != "" {
		blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: text})
	}
	return append(blocks, images...)
}

// runOf 取工具事件所属 run（事件自包含：主/子 agent 并发工具归属）。
// run 上下文经 WithToolContext 注入（agents.runTools）；无上下文（独立引擎调用）→ 空。
func runOf(tc *events.ToolContext) string {
	if tc == nil {
		return ""
	}
	return tc.RunId
}

// truncate 头尾保留 + 中段省略量显式量化。
// 为什么不是纯 s[:max]：对 grep/find，前 N 字节只是按路径字母序最早的命中（与相关性无关）；
// 对编译/测试输出，诊断信息总在尾部（FAIL/堆栈）——纯头部截断会把最关键的一段整段丢掉。
// 模型必须同时知道三件事：总量、看到了多少、省略了多少（用户 2026-09-18 明确要求）。
func (e *Engine) truncate(s string) string {
	if e.maxResponse <= 0 || len(s) <= e.maxResponse {
		return s
	}
	head := e.maxResponse / 2
	tail := e.maxResponse - head
	headEnd := cutPrefixUTF8(s, head) // 不劈裂多字节字符
	tailStart := cutSuffixUTF8(s, tail)
	if tailStart <= headEnd {
		return s[:headEnd] + fmt.Sprintf("\n\n[... %d bytes omitted ...]", len(s)-headEnd)
	}
	omitted := tailStart - headEnd
	return s[:headEnd] + fmt.Sprintf(
		"\n\n[... %d bytes omitted from the middle (%.2f%% of %d bytes total); showing the first %d and the last %d bytes — both ends are present, the middle is not. Narrow the output to see it all (e.g. `head`/`-l`/`wc`, a smaller path or pattern) ...]\n\n",
		omitted, 100*float64(omitted)/float64(len(s)), len(s), headEnd, len(s)-tailStart) +
		s[tailStart:]
}

// cutPrefixUTF8 返回 <= n 且落在 rune 边界上的长度。
func cutPrefixUTF8(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// cutSuffixUTF8 返回起始下标 i，使 len(s)-i <= n 且 i 落在 rune 边界上。
func cutSuffixUTF8(s string, n int) int {
	if n <= 0 {
		return len(s)
	}
	i := len(s) - n
	if i < 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// parseToolError extracts the stable metadata emitted by builtin tools.
// 精简格式缺省值：Changed="false"、Retryable=true、RetryWithSameArguments=false
// ——与现网所有 recoveryError 调用点取值一致；重复失败调用拦截
// （agents：!RetryWithSameArguments 才记 hash）依赖该缺省，勿改。
func parseToolError(result string) *core.ToolError {
	// 参数校验失败路径会把信封前置 "invalid params: "（见 ValidParams 分支）——
	// 先剥掉再判定，否则整类「参数非法」错误的 code/next_action/fix 全丢
	//（实测 2026-09-18：92 次，invalid_line_range 88 + missing_required_argument 4）。
	result = strings.TrimPrefix(result, "invalid params: ")
	if !strings.HasPrefix(result, "TOOL_ERROR\n") {
		return nil
	}
	e := &core.ToolError{Changed: "false", Retryable: true}
	for _, line := range strings.Split(result, "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "code":
			e.Code = parts[1]
		case "changed":
			e.Changed = parts[1]
		case "retryable":
			e.Retryable = parts[1] == "true"
		case "retry_with_same_arguments":
			e.RetryWithSameArguments = parts[1] == "true"
		case "next_action":
			e.NextAction = parts[1]
		case "fix":
			e.Recovery = parts[1]
		case "recovery": // 旧格式兼容（历史快照重放场景）
			e.Recovery = parts[1]
		}
	}
	return e
}

func errorCode(e *core.ToolError) string {
	if e == nil {
		return ""
	}
	return e.Code
}
func errorChanged(e *core.ToolError) string {
	if e == nil {
		return ""
	}
	return e.Changed
}
func errorRetryable(e *core.ToolError) bool { return e != nil && e.Retryable }
func errorNextAction(e *core.ToolError) string {
	if e == nil {
		return ""
	}
	return e.NextAction
}
func errorRecovery(e *core.ToolError) string {
	if e == nil {
		return ""
	}
	return e.Recovery
}

// 事件 code（ToolResponse.ErrorCode）只有两个来源：工具信封显式声明的 code（parseToolError）
// 与引擎推导（下面这个常量 + fallbackErrorCode 的文本分类）。两者都**只进事件/遥测**，
// 绝不写 ToolResult.ToolError（见 fallbackErrorCode 注释）。
const (
	// errorCodeCanceled 父 ctx 被取消（用户中断 / 会话关闭 / 后台任务中断）——结构化事实
	// Exec.Canceled，不是命令自身的问题，也不该建议调大 timeout（B5 刚把取消与超时分开）。
	errorCodeCanceled = "canceled"
)

// fallbackErrorCode 为没有 TOOL_ERROR 信封的错误推导一个稳定 code，仅用于事件/遥测
// （ErrorCode/Retryable 字段），**不写入 ToolResult.ToolError** —— 后者是
// repeated_failed_call 守卫的判定依据，必须保持「只有工具显式声明才纳入」的保守范围
// （见 FIX_PLAN_BATCH1.md 非目标 2）。
// 事件 code 全集（本函数 + 结构化先验 errorCodeCanceled + 工具信封）：
// canceled / file_not_found / path_not_allowed / not_a_file / transport_error / timeout /
// unknown_tool / unclassified（以及各工具信封自报的 code，如 invalid_line_range）。
func fallbackErrorCode(result string) string {
	r := strings.ToLower(result)
	switch {
	case strings.Contains(r, "file not found"), strings.Contains(r, "文件不存在"),
		strings.Contains(r, "no such file or directory"):
		return "file_not_found"
	case strings.Contains(r, "escapes workspace root"), strings.Contains(r, "is protected"):
		return "path_not_allowed"
	case strings.Contains(r, "is a directory"):
		return "not_a_file"
	case strings.Contains(r, "transport error"), strings.Contains(r, "transport closed"):
		return "transport_error"
	case strings.Contains(r, "deadline exceeded"), strings.Contains(r, "timed out"), strings.Contains(r, "timeout"):
		return "timeout"
	case strings.Contains(r, "is not registered"):
		return "unknown_tool"
	default:
		return "unclassified"
	}
}
