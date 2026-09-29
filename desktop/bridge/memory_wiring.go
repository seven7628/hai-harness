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

// injection 返回 Step 3 要注入系统提示词的索引段。
//
// 三种空结果都返回空串（= 不注入该层），不返回占位文本：
//
//	开关关闭        → 空（reason=disabled）
//	无 git 身份     → 空（reason=no-identity，这是绝大多数会话）
//	有身份但无索引   → 空（repo 还没有任何记忆）
//
// 只有"有索引"才注入。索引为空时注入一段"你还没有记忆"是纯浪费：官方对缺失的规定
// 是不注入不报错，而 §5.2 的硬要求是「无记忆时 prompt 逐字节不变」。
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

// postWrite 是 Step 4 的钩子实现：写入路径落在记忆目录内时补/更新 `modified`。
//
// 三道闸，任何一道不满足就静默返回 nil（这不是"出错"，是"这次写入与记忆无关"）：
//
//	① 开关关闭      → 连 .lock 侧车文件都不建（否则变成"不读但写入"）
//	② 路径不是记忆 → IsMemoryPath 是纯词法比较，不需要先解析仓库身份，
//	                   工作区内的绝大多数写入在这里就被排除，零 git 开销
//	③ 目录解析失败  → 无 git 身份就没有记忆目录，无从判断
//
// 真正的错误（补戳写失败）**不吞**，原样回给模型：静默吞掉会让记忆文件带着过期或
// 缺失的 modified 继续留在索引里，而模型毫不知情。
func (h *memoryHost) postWrite(ctx context.Context, workspace, absPath string) error {
	if h.cfg.Disabled() {
		return nil
	}
	if !filepath.IsAbs(absPath) || !h.cfg.IsMemoryPath(absPath) {
		return nil
	}
	dir, err := h.resolve(ctx, workspace)
	if err != nil || dir == nil {
		return nil
	}
	if !dir.IsTopic(absPath) {
		return nil
	}
	ws := h.cfg.StampModified(*dir, absPath)
	return ws.Err
}

// memoryResolveTimeout 身份解析（跑 git）的上限。
//
// 装配期的注入在会话创建的关键路径上：无界的 git 一旦挂住（网络文件系统、
// 凭据助手等待、锁等待）会把整个会话创建卡死，而用户看到的只是"应用没反应"。
// 超时后按"没有记忆"处理 —— 官方对缺失的规定就是不注入不报错。
const memoryResolveTimeout = 10 * time.Second
