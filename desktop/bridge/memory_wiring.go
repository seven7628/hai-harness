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

// injection 返回 Step 3 要注入系统提示词的段落。
//
// 四种结果：
//
//	开关关闭        → 空（reason=disabled）
//	无 git 身份     → 空（reason=no-identity，这是绝大多数会话）
//	有身份但无索引   → **只有目录说明**（reason=no-index / empty-index）
//	有索引          → 标题 + 索引正文（+ 截断提示）+ 目录说明
//
// 前两种返回空串 = 不改动系统提示词的任何字节；后两种一定会注入一段文本。
// 这不是"多花了点 context"，而是冷启动的唯一入口：没有 MEMORY.md 就没有第一条
// 记忆，而第一条只可能靠这段说明写出来（2026-09-30 起，见 memory/guidance.go）。
func (h *memoryHost) injection(ctx context.Context, workspace string) string {
	if h.cfg.Disabled() {
		return ""
	}
	// 走 resolve 而不是自己再 Bootstrap 一次：解析成功的结果在装配期已缓存，
	// 两次装配点（Step 3 注入 / Step 4 钩子）因此只付一次 git 的代价。
	dir, err := h.resolve(ctx, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[memory] 启动注入跳过：%v\n", err)
		return ""
	}
	if dir == nil {
		return "" // 开关关闭 / 无 git 身份（后者是绝大多数会话）
	}
	inj, err := h.cfg.BuildInjection(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[memory] 读索引失败：%v\n", err)
		return ""
	}
	return inj.Text
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
//	③ 目录解析失败  → 无 git 身份就没有记忆目录，无从判断
//
// 真正的错误（补戳写失败、索引超限）**不吞**，原样回给模型：静默吞掉会让记忆文件
// 带着过期或缺失的 modified 继续留在索引里，而模型毫不知情。
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
	// 官方（memory#how-it-works）把两条规则都挂在 MEMORY.md 上：
	//
	//	"After Claude writes to `MEMORY.md`, Claude Code measures the file
	//	 against the 200-line and 25KB read limits."
	//
	// 所以 topic 文件走 modified、索引走限额测量，两者在同一个钩子里分流。
	if dir.IsIndex(absPath) {
		if err := h.cfg.CheckIndex(*dir); err != nil {
			return "", err
		}
		return h.cfg.NearIndexNotice(*dir), nil
	}
	if !dir.IsTopic(absPath) {
		return "", nil
	}
	ws := h.cfg.StampModified(*dir, absPath)
	return "", ws.Err
}

// memoryResolveTimeout 身份解析（跑 git）的上限。
//
// 装配期的注入在会话创建的关键路径上：无界的 git 一旦挂住（网络文件系统、
// 凭据助手等待、锁等待）会把整个会话创建卡死，而用户看到的只是"应用没反应"。
// 超时后按"没有记忆"处理 —— 官方对缺失的规定就是不注入不报错。
const memoryResolveTimeout = 10 * time.Second
