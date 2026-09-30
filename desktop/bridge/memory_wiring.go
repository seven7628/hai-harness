package main

// Project Memory 的宿主接线（Step 3 启动注入 + Step 4 modified 钩子）。
//
// 装配位置与理由
//
// Step 3 的注入文本在**会话建立时算一次**（buildLoop），不在 agents 层算：
// composeSystemPrompt 每轮循环都会重跑，而 memory.Bootstrap 要跑 git 解析仓库身份
// 再读索引，放进去等于每轮多一次子进程 + N 次读盘。
//
// Step 4 的钩子挂在 write_file / edit_file 的**写成功**路径上（tools/builtin 的
// WithPostWrite），因为官方规定 topic 文件被写入时补 `modified`，而记忆文件是模型
// 用**标准文件工具**写的 —— 没有别的入口。
//
// 开关语义（关闭 = 完全不注入 + 完全不写盘）全部在 memory 包内闭环：宿主不做二次
// 判定，也不注入"请勿使用记忆"这类占 context 又未必被遵守的段落。
//
// 范围：只接主会话。子 agent 不注入、不补戳（官方未规定 subagent 记忆继承，
// 本轮明确不实现）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/memory"
)

// memoryHost 持有一次会话装配期解析出的记忆目录，供 Step 3 与 Step 4 共用。
//
// 为什么要缓存 Dir：Step 4 的钩子挂在每一次 write_file / edit_file 成功路径上，
// 而解析目录要跑 `git` 子进程。不缓存 = 每次写文件都 fork 一次 git。
//
// 失败**不缓存**：git 身份解析可能因为工作区还没初始化完、或用户正在切分支而暂时
// 失败；缓存失败会让这次会话后续所有写入永远不补戳，而用户重开会话就好了 ——
// 那是"看起来是 bug 且无法自愈"的故障形态。成功才缓存（sync.Once 语义）。
type memoryHost struct {
	cfg memory.Config

	once sync.Once
	dir  *memory.Dir
}

// newMemoryHost 装配期构造。**不**在这里解析身份：解析要跑 git，而装配期的
// context 不总是可用的（会话 ctx 在 buildLoop 阶段尚未就绪，session 变量还是 nil）。
// 身份改为惰性解析 —— 第一次真的需要目录时才跑 git。
func newMemoryHost(cfg memory.Config) *memoryHost {
	return &memoryHost{cfg: cfg}
}

// resolve 取记忆目录（成功则缓存，失败则每次重试）。workdir 为空时不做任何事。
func (h *memoryHost) resolve(ctx context.Context, workdir string) (*memory.Dir, error) {
	if h.cfg.Disabled() || workdir == "" {
		return nil, nil
	}
	h.once.Do(func() {
		res := memory.Bootstrap(ctx, workdir, h.cfg)
		if res.Resolved && res.Dir != nil {
			h.dir = res.Dir
		}
	})
	if h.dir != nil {
		return h.dir, nil
	}
	// 未缓存成功 → 重试一次解析（每条写入一次 git，代价可接受：只在身份解析失败
	// 的会话里发生，而那种会话通常不写文件）
	res := memory.Bootstrap(ctx, workdir, h.cfg)
	if res.Resolved && res.Dir != nil {
		return res.Dir, nil
	}
	return nil, res.Err
}

// injection 返回 Step 3 要注入系统提示词的段落（主会话）。
//
// 四种结果：
//
//	开关关闭        → 空（reason=disabled）
//	取不到目录       → 空（reason=no-identity / 取不到 home）
//	有目录但无索引   → **只有目录说明**（reason=no-index / empty-index）
//	有索引          → 标题 + 索引正文（+ 截断提示）+ 目录说明
//
// 前两种返回空串 = 不改动系统提示词的任何字节；后两种一定会注入一段文本。
// 这不是"多花了点 context"，而是冷启动的唯一入口：没有 MEMORY.md 就没有第一条
// 记忆，而第一条只可能靠这段说明写出来（2026-09-30 起，见 memory/guidance.go）。
func (h *memoryHost) injection(ctx context.Context, workspace string) string {
	// 走 resolve 而不是自己再 Bootstrap 一次：解析成功的结果在装配期已缓存，
	// 装配点（Step 3 注入 / Step 4 钩子）因此只付一次 git 的代价。
	dir, err := h.resolve(ctx, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[memory] 启动注入跳过：%v\n", err)
		return ""
	}
	if dir == nil {
		return "" // 开关关闭 / 取不到目录
	}
	return h.injectionFor(*dir)
}

// injectionFor 对**给定**记忆目录产出注入段。与 injection 的区别只有"目录从哪来"，
// 因为主会话与「启用 memory 的子 agent」走的是同一段文本生成逻辑——官方说的是
// 子 agent 有自己的目录，不是另一套规则。
func (h *memoryHost) injectionFor(dir memory.Dir) string {
	if h.cfg.Disabled() {
		return ""
	}
	inj, err := h.cfg.BuildInjection(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[memory] 读索引失败：%v\n", err)
		return ""
	}
	return inj.Text
}

// subagentInjection 返回某个子 agent 自己的记忆目录的注入段。
//
// 官方 memory#how-it-works："The main conversation's auto memory isn't loaded into
// subagents … A subagent's own auto memory, enabled with the subagent `memory` field,
// is a separate directory." —— 所以这里**不给**主目录，只给子 agent 自己那份；
// 子 agent 的记忆与主会话互不可见（这正是官方那句 "isn't loaded into subagents" 的
// 含义，也是我们一直以来的实际行为，现在由 TestSubagentDoesNotInheritMainMemory 钉住）。
func (h *memoryHost) subagentInjection(ctx context.Context, workspace, name string) string {
	if h.cfg.Disabled() || name == "" {
		return ""
	}
	repo, err := h.resolve(ctx, workspace)
	if err != nil || repo == nil {
		return ""
	}
	sub, ok := h.cfg.SubagentDir(*repo, name)
	if !ok {
		fmt.Fprintf(os.Stderr, "[memory] 子 agent %q 的记忆目录派生失败（名字非法？）：不注入\n", name)
		return ""
	}
	return h.injectionFor(sub)
}

// postWrite 是 Step 4 的钩子实现：记忆目录内的写入，补/更新 `modified`；
// 写到 MEMORY.md 时，另按官方口径测量索引限额。
//
// 两条通道分开（2026-09-30，签名从 error 改成 (notice, error)）：
//
//	err     真失败。补戳写失败、或索引超限——官方明说超限时
//	         "the write still succeeds, but Claude Code returns an error telling
//	         Claude to rewrite the index"，回错误是忠实的。
//	notice  写已成功、只是顺带提醒。索引接近限额时官方说的是
//	         "reminds Claude to shorten it"——是**提醒**不是报错。混进 err 会让
//	         模型以为写失败并重试，而重试只会让索引更长。
//
// 四道闸，任何一道不满足就静默返回（这不是"出错"，是"这次写入与记忆无关"）：
//
//	① 开关关闭      → 连 .lock 侧车文件都不建（否则变成"不读但写入"）
//	② 路径不是记忆 → IsMemoryPath 是纯词法比较，不需要先解析仓库身份，
//	                   工作区内的绝大多数写入在这里就被排除，零 git 开销
//	③ 目录解析失败  → 取不到记忆目录，无从判断
//
// 真正的错误（补戳写失败、索引超限）**不吞**，原样回给模型：静默吞掉会让记忆文件
// 带着过期或缺失的 modified 继续留在索引里，而模型毫不知情。
//
// # 归属按**路径**判定，不按调用者（2026-09-30）
//
// 主 agent 与子 agent 共用同一个 ToolEngine 实例，钩子拿不到"谁在写"。但这不需要
// 解决：主目录（`<root>/repos/<id>`）与子 agent 目录（`<root>/agents/<id>/<name>`）
// **互不相交**，路径本身就唯一确定归属。这也正是把子 agent 记忆放在 `agents/` 下
// 而不是 `repos/<id>/agents/` 的原因（见 memory/subagent.go）。
func (h *memoryHost) postWrite(ctx context.Context, workspace, absPath string) (string, error) {
	if h.cfg.Disabled() {
		return "", nil
	}
	if !filepath.IsAbs(absPath) || !h.cfg.IsMemoryPath(absPath) {
		return "", nil
	}
	dir, err := h.resolve(ctx, workspace)
	if err != nil || dir == nil {
		return "", nil
	}
	// 子 agent 的独立目录优先：它与主目录不相交，两者不可能同时命中同一条路径。
	target := *dir
	if sub, ok := h.cfg.SubagentDirFromPath(*dir, absPath); ok {
		target = sub
	}
	// 官方（memory#how-it-works）把两条规则都挂在 MEMORY.md 上：
	//
	//	"After Claude writes to `MEMORY.md`, Claude Code measures the file
	//	 against the 200-line and 25KB read limits."
	//
	// 所以 topic 文件走 modified、索引走限额测量，两者在同一个钩子里分流。
	if target.IsIndex(absPath) {
		if err := h.cfg.CheckIndex(target); err != nil {
			return "", err
		}
		return h.cfg.NearIndexNotice(target), nil
	}
	if !target.IsTopic(absPath) {
		return "", nil
	}
	scope := events.MemoryScopeMain
	if target.Path != dir.Path {
		scope = events.MemoryScopeSubagent
	}
	ws := h.cfg.StampModified(target, absPath)
	if ws.Err == nil {
		h.emitMemoryActivity(ctx, scope, events.MemoryActionSaved, filepath.Base(absPath))
	}
	return "", ws.Err
}

// personaMemory 描述"给这个子 agent 注入哪一份记忆"。
//
// 它是**装配参数**而不是全局状态：memoryHost 与主会话共用（同一次 git 解析结果
// 只付一次代价），但子 agent 各自的目录由 Name 决定。
type personaMemory struct {
	// Host 与主会话同一个 memoryHost（共享已解析的主目录，避免重复跑 git）。
	Host *memoryHost
	// Name 子 agent 定义名；空 = 不注入。
	Name string
	// Ctx 取会话 ctx。装配期 session 变量还是 nil，不能直接取 bgCtx()，
	// 故由调用方（newSessionEngine，已持有 bgCtx）传进来。
	Ctx func() context.Context
}

// bgCtxOrBackground 取会话 ctx；取不到就用 Background。
//
// 为什么不直接 Fatal/panic：记忆是增强项，不是会话的前提——上下文不可用时降级成
// "这个子 agent 没有记忆"继续跑，远好过让整个 spawn 失败。
func (p personaMemory) bgCtxOrBackground() context.Context {
	if p.Ctx == nil {
		return context.Background()
	}
	if c := p.Ctx(); c != nil {
		return c
	}
	return context.Background()
}

// memoryResolveTimeout 身份解析（跑 git）的上限。
//
// 装配期的注入在会话创建的关键路径上：无界的 git 一旦挂住（网络文件系统、
// 凭据助手等待、锁等待）会把整个会话创建卡死，而用户看到的只是"应用没反应"。
// 超时后按"没有记忆"处理 —— 官方对缺失的规定就是不注入不报错。
const memoryResolveTimeout = 10 * time.Second

// memCfgValue 从可变参数 memCfg ...memory.Config 里取值（没传 = 零值 = 跟随环境/默认开）。
//
// 抽成函数是因为取值出现两次（host 装配 + 子 agent 记忆），而"没传"必须与
// `memory.Config{}` 完全同义——两处各写一遍 `if len(...)>0` 迟早会漂移。
func memCfgValue(memCfg []memory.Config) memory.Config {
	if len(memCfg) > 0 {
		return memCfg[0]
	}
	return memory.Config{}
}

// postRead 是 read_file 的"读成功后"钩子：读的是记忆文件时发一条
// MemoryActivity{Action: recalled}（官方 "Recalled N memories"）。
//
// 与 postWrite 的判定同构（三道闸：开关 / 是否记忆路径 / 目录能否解析），但**不**
// 需要归属到具体目录：读侧不补戳、不测限额，只需要知道"这是一条记忆、它属于谁
// 的记忆"，以便把事件标上 Scope。
func (h *memoryHost) postRead(ctx context.Context, workspace, absPath string) (string, error) {
	if h.cfg.Disabled() || !filepath.IsAbs(absPath) || !h.cfg.IsMemoryPath(absPath) {
		return "", nil
	}
	dir, err := h.resolve(ctx, workspace)
	if err != nil || dir == nil {
		return "", nil
	}
	scope := events.MemoryScopeMain
	owner := *dir
	if sub, ok := h.cfg.SubagentDirFromPath(*dir, absPath); ok {
		scope = events.MemoryScopeSubagent
		owner = sub
	}
	// MEMORY.md 是索引不是记忆条目：读索引是"读清单"，官方没把它算成一条记忆。
	// 计入会让 "Recalled N memories" 在刚启动、只加载了索引时就说 N>0。
	if !owner.IsTopic(absPath) {
		return "", nil
	}
	h.emitMemoryActivity(ctx, scope, events.MemoryActionRecalled, filepath.Base(absPath))
	return "", nil
}

// emitMemoryActivity 发一条记忆活动事件。
//
// 发不出去**不报错**：它是一条纯可观测性事件（官方那句话只是"你能看到"），
// 而读写的成功与否不该因为它失败而改变。拿不到工具上下文（直接调工具的根 agent
// 路径）就静默跳过——没有 RunId 就无法与 agent_start/agent_end 关联，发出去也
// 是孤儿事件。
func (h *memoryHost) emitMemoryActivity(ctx context.Context, scope events.MemoryScope,
	action events.MemoryAction, file string) {
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Handler == nil {
		return
	}
	tc.Handler(ctx, &events.MemoryActivity{
		RunId:     tc.RunId,
		Scope:     scope,
		Action:    action,
		File:      file,
		Count:     1,
		Timestamp: time.Now(),
		EventType: events.MemoryActivityType,
	})
}
