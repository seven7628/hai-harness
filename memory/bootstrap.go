package memory

import (
	"context"
	"path/filepath"
)

// Bootstrap 解析记忆目录并组装启动注入。**这是 Step 3 的接线入口。**
//
// workdir 是会话的工作目录（官方 <project> 由 git 仓库派生，§3.2）；cfg 是用户可见开关。
// 开关关闭时**立刻返回**，不解析身份、不读盘——这是「关闭 = 完全不注入」的实现点。
//
// # 装配层要做的事只有三件
//
//	// ① 会话启动：把注入文本拼进系统提示词
//	res := memory.Bootstrap(ctx, workdir, cfg)
//	prompt := base + res.Injection.Text   // Text 为空则一个字节都不拼
//
//	// ② 模型 write_file 写完一条记忆后：补 modified（Step 4）
//	if res.Dir != nil && cfg.IsMemoryPath(path) {
//	    if w := cfg.StampModified(*res.Dir, path); w.Err != nil {
//	        // 回给模型，不吞
//	    }
//	}
//
//	// ③ 用户在设置面板关掉开关：cfg.Enabled = Bool(false)，无需重启应用
//
// **本函数不写会话、不改配置、不碰 prompt 组装。** 它只回答一个问题：
// "这段文字要不要拼进系统提示词"。拼到哪一层、存不存盘、错误怎么呈现都由调用方决定——
// 《技术方案》§5.2 要求「追加位置与 AGENTS.md 同级或其后」，那是装配层的结构知识，
// 本包不该反过来规定它。
//
// # 装配点（2026-09-29 接上，此前本包零 importer）
//
//	Step 3  desktop/bridge/main.go buildLoop：newMemoryHost(...).injection(...)
//	        非空则 agents.WithMemoryInjection，composeSystemPrompt 在工作记忆之后拼接
//	Step 4  desktop/bridge/main.go newSessionEngine：builtin.NewFileTools(ws)
//	        .WithPostWrite(→ memoryHost.postWrite)
//
// 身份解析在**装配期做一次**并缓存，composeSystemPrompt 每轮循环都重跑，
// 放那里等于每轮多一次 git 子进程 + N 次读盘。
//
// 装配期**不能**用 bgCtx()：buildLoop 阶段它捕获的 session 变量还是 nil，调用即崩。
// 故用 context.WithTimeout(context.Background(), memoryResolveTimeout)（10s）——
// 顺带防住"挂住的 git 阻塞会话创建"。
//
// 空状态自查（**现已不成立**，接线后 desktop/bridge 是 importer）：
//
//	go list -deps=false -f "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}" ./... | grep hai-harness/memory
//	# 期望含 github.com/seven7628/hai-harness/desktop/bridge（生产装配点）
//
// Step 0 三臂基线装置因此在产品树上跳过（见 desktop/bridge/memory_baseline_test.go
// 的 baselineEnforceEmptyMemoryState：仅 desktop/bridge 时跳过，出现**其他** importer
// 仍 t.Fatal —— 护栏防的是"不知道自己在测什么"，该能力完整保留）。

func Bootstrap(ctx context.Context, workdir string, cfg Config) BootstrapResult {
	// ↓ 开关前置短路：关闭时不解析身份（不跑 git）、不读盘、不注入。
	// Step 4 的开关判定在 StampModified 内部独立做，两者互不依赖——所以
	// 装配层即使只接了其中一半，另一半也是干净的（不会半开半关）。
	if !cfg.enabled() {
		return BootstrapResult{
			Injection: Injection{Enabled: false, Reason: "disabled"},
		}
	}
	dir, err := resolveDir(ctx, workdir, cfg)
	if err != nil {
		// 无 git 身份 / 取不到 home：按"没有记忆"处理，err 带回给调用方记日志。
		// 注意这里**不是**把 err 藏起来：宿主需要知道"为什么这次没注入"。
		return BootstrapResult{
			Injection: Injection{Enabled: true, Reason: "no-identity"},
			Err:       err,
		}
	}
	inj, err := cfg.withDefaults().BuildInjection(dir)
	if err != nil {
		return BootstrapResult{Injection: inj, Dir: &dir, Resolved: true, Err: err}
	}
	return BootstrapResult{Injection: inj, Dir: &dir, Resolved: true}
}

// resolveDir 定位记忆目录。三条来源，优先级从高到低：
//
//  1. CLAUDE_CODE_PROJECT_DIR_NAME / Config.ProjectDirName —— 强制 <project> 名，
//     落到 <root>/repos/<name>。官方用途：让多个仓库共用一份记忆。
//  2. <root> 由 Config.Directory / autoMemoryDirectory 覆盖。
//  3. <root>/repos/<git 派生的 ID> —— 常规路径（identity.go）。
//
// autoMemoryDirectory 只改**根**，不改单个仓库目录名，这一点是有意的：官方
// autoMemoryDirectory 的语义是"换存储位置"，换完仍然按仓库分层。
func resolveDir(ctx context.Context, workdir string, cfg Config) (Dir, error) {
	c := cfg.withDefaults()
	root, err := c.root()
	if err != nil {
		return Dir{}, err
	}
	if name := c.projectDirName(); name != "" {
		id := sanitizeSegment(name)
		if id == "" {
			return Dir{}, ErrNoIdentity
		}
		return Dir{ID: id, Path: filepath.Join(root, reposDirName, id)}, nil
	}
	d, err := c.Resolve.Resolve(ctx, workdir)
	if err != nil {
		return Dir{}, err
	}
	// 身份解析内部会再算一次默认根（ReposRoot），这里按覆盖后的根重算路径。
	// ID 不变，所以换根不会改变"哪份记忆属于哪个仓库"，只改变落在哪。
	d.Path = filepath.Join(root, reposDirName, d.ID)
	return d, nil
}

// BootstrapResult 是 Bootstrap 的返回值。
//
// 之所以是结构体而不是 (Injection, error) 两段式：调用点几乎总是要**同时**拿
// "注入文本"和"记忆目录"（后者给 Step 4 的写钩子判路径），而"这个仓库还没有记忆"
// 是常态而不是错误。
type BootstrapResult struct {
	// Injection 启动注入（见 Injection）。关闭开关时 Text 为空且 Reason=="disabled"。
	Injection Injection
	// Dir 记忆目录（Enabled=true 且身份解析成功时非 nil），供 Step 4 使用。
	Dir *Dir
	// Resolved 身份解析是否成功。ErrNoHome 等**不算失败**：
	// 绝大多数会话没有记忆，官方对缺失的规定是"不注入不报错"。
	Resolved bool
	// Err 真实错误（取不到 home、读盘 IO 失败）。非 nil 时注入文本必为空。
	//
	// 注意「工作目录不在 git 工作树内」**不在此列**：官方回落到项目根目录，
	// 那条路径仍然解析成功（见 identity.go Resolve）。
	Err error
}

// Disabled 判定这次配置下记忆是否整体关闭（不注入、也不补时间戳）。
//
// 装配层在关掉开关的分支里可以直接用它做短路，不必先调 Bootstrap 再看 Reason。
func (c Config) Disabled() bool { return !c.enabled() }

// IsMemoryPath 判断 path 是否属于**某个**仓库的记忆目录（跨仓库，Step 4 的前置筛）。
//
// 装配位置：write_file / edit_file 钩子的最前面。它比 Dir.IsTopic 便宜——不需要先
// 解析当前仓库身份，一次路径比较就能把绝大多数写入排除掉（工作区内的文件占了绝大多数）。
func (c Config) IsMemoryPath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	root, err := c.root()
	if err != nil {
		return false
	}
	return IsInMemoryDir(root, path)
}
