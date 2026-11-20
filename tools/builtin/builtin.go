// Package builtin 内置基础工具集：文件读写、目录浏览、Shell、HTTP。
//
// 安全模型（workspace 隔离）：
//   - 文件类工具构造时绑定工作区根目录，所有路径解析后必须落在根内（拒绝逃逸）；
//   - Shell 随工具集默认注册（初始 cwd = 工作区根），经进程隔离/沙箱执行 —— 注册即信任；
//   - HTTP 带超时，响应体截断。
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FileTools 绑定工作区根目录的文件工具集。
type FileTools struct {
	root string
	sbx  sandbox.Sandbox // bash 执行后端（nil = NoSandbox 进程隔离直通）

	// wd 会话级 cwd 状态（nil = **没有**会话级状态，相对路径恒定锚工作区根）。
	//
	// 为什么需要它：改造前文件工具的相对路径恒定锚在工作区根，而 bash 记着自己的 cd
	// ⇒ 模型 `cd sub` 之后 read_file("a.txt") 仍在根目录找 ⇒ file not found。
	// 接上 workdir.State 后，相对路径锚到**调用方分区的当前目录**，与 bash / run_python
	// 同一份真相（见 workdir 包的包注释）。
	//
	// nil 保留（NewFileTools）：库默认行为一字不变 —— 不接状态就没有"会话级 cwd"
	// 这件事，既有调用方（测试、独立使用）行为完全一致。
	wd *workdir.State

	// binaryHint 可选钩子：read_file 判出「二进制、不可解码为文本/图片」时，产出错误
	// 提示末尾的「下一步怎么办」句（参数 = 文件路径，宿主可按扩展名细分格式）。宿主按
	// **自身能力面**注入 —— 不注册 bash 的产品形态（桌面 Work persona）照默认句提示
	// bash，等于让模型去调不存在的工具、白耗轮次；nil = 默认提示 bash（库默认，既有
	// 用户不受影响）。
	binaryHint func(path string) string

	// postWrite 写/改成功后的宿主钩子，参数是**绝对路径**。nil = 不挂钩（库默认，
	// 既有用户不受影响）。
	//
	// 用途：Project Memory 的 modified 戳（memory 包 StampModified）。官方规定
	// topic 文件被写入时补 `modified`，而记忆文件由模型用**标准 write_file /
	// edit_file** 写入——所以戳时间戳只能挂在这两个工具的写成功路径上，没有别的
	// 入口。工具本身不知道"记忆"是什么概念，只负责在写成功后报一次绝对路径；
	// 是不是记忆文件、要不要补戳、开关是否打开，全部由宿主的钩子决定。
	//
	// 为什么用钩子而不是直接 import memory：工具层不应依赖任何产品概念
	// （同 binaryHint 的分层理由）。宿主不注入钩子 = 行为与今天完全一致。
	//
	// **带 ctx**（2026-09-30 追加）：Project Memory 的 "Saved/Recalled N memories"
	// 需要把事件挂到**本次运行**上（RunId + Handler 都在工具上下文里）。不带 ctx 的
	// 钩子只能看到路径，发不出可关联的事件——第一版就踩了这个：单测直接调宿主方法
	// 全绿，端到端那条（真的走 read_file）却是 0 条事件。
	//
	// 返回 (notice, err) 而不是单一 error，是 2026-09-30 的修正。宿主有两类
	// **不同性质**的话要在写成功后告诉模型，而它们不能走同一条通道：
	//
	//	err     真失败：写入本身没成，或官方明说要报错的情形（如 MEMORY.md 超限，
	//	         官方 "the write still succeeds, but Claude Code returns an error
	//	         telling Claude to rewrite the index"）。回错误是忠实的。
	//	notice  写**已经成功**，但顺带提醒一句（如索引接近限额）。把它塞进 err
	//	         会让模型以为写失败了——它很可能重试，而重试只会让索引更长，
	//	         正是这条提醒要避免的结果。
	postWrite func(ctx context.Context, absPath string) (notice string, err error)
	// postRead 读成功后的对称钩子，签名与 postWrite 一致。默认 nil（零开销）。
	//
	// 为什么需要它（2026-09-30）：Project Memory 的用户可见面里有一条
	// "Recalled N memories"（官方 memory#how-it-works）。要诚实地报这个数，就得知道
	// 模型**读了哪些记忆文件**——而读只有这一个入口，postWrite 只能看见写。
	//
	// 返回值用途与 postWrite 相同，但**目前没有宿主用它**：读取失败要照常报错，
	// 读取成功没有要附加给模型的提示（官方没规定读记忆要提醒什么）。签名先对齐，
	// 是为了不必在两个钩子不一致时再改一次。
	postRead func(ctx context.Context, absPath string) (notice string, err error)
}

// WithPostRead 注入"读成功后"钩子（nil = 保持默认不挂钩）。
//
// 与 WithPostWrite 同型：notice 拼在结果后、err 回给模型。见 WithPostWrite 的注释
// 为什么那两条通道必须分开。
func (f *FileTools) WithPostRead(fn func(context.Context, string) (string, error)) *FileTools {
	f.postRead = fn
	return f
}

// WithPostWrite 注入"写/改成功后"钩子（nil = 保持默认不挂钩）。
//
// notice 会拼在成功结果**后面**返回给模型（非错误）；err 会**一并回给模型**，不吞：
// Step 4 补戳失败若被静默吞掉，记忆文件会带着过期或缺失的 modified 继续留在索引里，
// 而模型毫不知情。
func (f *FileTools) WithPostWrite(fn func(context.Context, string) (string, error)) *FileTools {
	f.postWrite = fn
	return f
}

// WithBashSandbox 注入 bash 工具的执行后端（如 macOS Seatbelt / Linux bwrap）。
// 工具本身不知道自己在沙箱里；nil 保持 NoSandbox 进程隔离（库默认，端口保持现状）。
func (f *FileTools) WithBashSandbox(sbx sandbox.Sandbox) *FileTools {
	f.sbx = sbx
	return f
}

// WithBinaryHint 注入二进制拒绝提示的尾句构造器（nil = 保持默认提示 bash）。
// 钩子风格同 BashTool.BeforeExec / LoadSkillTool.WithOnLoad：工具层只负责给出「这个
// 文件读不了」的事实（文件名/形态/体积仍由本层拼），后续建议由宿主按自己注册了哪些
// 工具来组织——库不认识 persona 等产品概念。
func (f *FileTools) WithBinaryHint(fn func(path string) string) *FileTools {
	f.binaryHint = fn
	return f
}

// defaultBinaryHint 未注入钩子时的尾句：指向 bash 的现成工具链（库默认行为，勿改）。
const defaultBinaryHint = "Use bash for other formats (e.g. `file`, `ffmpeg`, `sips`, `unzip -l`)"

// NewFileTools 创建文件工具集；root 为工作区根目录（绝对路径，不存在不报错）。
//
// **不接会话级 cwd 状态**：相对路径恒定锚在工作区根（库默认行为，既有调用方不变）。
// 需要会话级 cwd（bash / 文件工具 / run_python 同一份目录真相）时用
// NewFileToolsWithWorkdir。
func NewFileTools(root string) *FileTools {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &FileTools{root: filepath.Clean(abs)}
}

// NewFileToolsWithWorkdir 创建**接会话级 cwd 状态**的文件工具集。
//
// wd = 会话级 cwd 状态（workdir.State）：工作区根取 wd.Root()，相对路径锚到
// **调用方分区**的当前目录（bash 的 cd 会同步反映到这里，因为两者共用同一个 State）。
//
// 为什么走"同一个 State"而不是"把 cwd 传进来"：cwd 是**运行期状态**（模型随时 cd），
// 装配期冻结的常量立刻过期 ⇒ 必须是同一个可变的、被两个工具共享的对象。
//
// wd 为 nil ⇒ 退化成 NewFileTools(wd.Root()) 的行为（恒定锚根目录），不 panic：
// 装配期传 nil 不该让整条链路炸掉。
func NewFileToolsWithWorkdir(wd *workdir.State) *FileTools {
	if wd == nil {
		return NewFileTools("")
	}
	return &FileTools{root: wd.Root(), wd: wd}
}

// Tools 返回工作区工具集（read_file / write_file / edit_file / grep / glob / bash）。
// bash 默认注册（命令经进程隔离执行），初始 cwd = 工作区根：bash 相对路径与文件
// 工具同锚（模型声明的 Current Workspace 即命令真实起点）。绑定仅决定「起点」，
// 不锁访问面 —— 命令内可随时 `cd 任意目录 && 执行`、绝对路径照常可用（已装包 /
// 桌面应用不受影响）；Seatbelt 策略按路径放行，与 cwd 无关。—— 危险防护从
// 「不注册」转向「注册 + 进程隔离」，模型无需显式要求即可使用。
func (f *FileTools) Tools() []tools.Tool {
	return []tools.Tool{
		newReadFileTool(f),
		newWriteFileTool(f),
		newEditFileTool(f),
		newGrepTool(f),
		newGlobTool(f),
		// cwd 传 f.root：bash 初始目录 = 工作区根（每条命令独立进程、从工作区出发；
		// AI 可在单条命令内 cd 切换，下条命令回到工作区起点——对齐 Claude Code 语义）。
		// appAllow 参数已弃用（2026-09：open 全放行），传 nil。
		//
		// 接了会话级 cwd 状态时：bash 与文件工具**共用同一个 workdir.State**
		// ⇒ bash 的 cd 立刻反映到文件工具（相对路径锚点），反之亦然。
		f.bashTool(),
	}
}

// bashTool 造本工具集里的 bash 工具：有会话级 cwd 状态时与文件工具共用它。
func (f *FileTools) bashTool() tools.Tool {
	if f.wd != nil {
		return NewBashToolWithWorkdir(f.wd, 0, f.sbx, nil)
	}
	return NewBashTool(f.root, 0, f.sbx, nil)
}

// Register 把文件工具集注册进引擎。
func (f *FileTools) Register(e *tools.Engine) {
	for _, t := range f.Tools() {
		e.RegisterTool(context.Background(), t)
	}
}

// pathParamDesc 是 read_file / write_file / edit_file 三个工具共用的 path 参数说明。
//
// # 为什么必须改（2026-09-30）
//
// 原文是 "File path (relative to workspace root)"，**与 resolve 的实际行为不符**，而
// 这条不符在 Project Memory 上直接造成写入失败：
//
//	resolve 接受绝对路径（工作区外也接受），只拒绝用 `..` 逃逸的**相对**路径；
//	而 schema 说"相对工作区根"，模型于是把宿主给它的**绝对**记忆目录路径
//	（如 ~/.go-code/memory/repos/<id>/）转成相对路径 —— 被拒：
//	    path "../009/mem/repos/010-f5ae/relay-backoff.md" escapes workspace root
//
// 即：宿主与工具**各自**都是对的，合起来把模型带进死路。真实 provider 跑批里
// 每 7 次写入尝试就有 1 次这样失败（2026-09-30 实测）。
//
// 官方没有这个矛盾：Claude Code 的 Write 参数写的是
// "Absolute path to file, or relative to the working directory" —— 两种都收。
// 我们不改 resolve 的行为（拒绝 `..` 逃逸是刻意的，见下），只把**实际规则**说准：
// 相对路径按工作区根解析且不得逃逸，要写工作区外的文件就用绝对路径。
//
// 这是工具契约的描述修正，不是新机制：行为一个字没改，schema 终于与实现一致。
//
// # 2026-11（会话级 cwd）：锚点从"工作区根"改成"当前工作目录"
//
// 接上 workdir.State 之后，相对路径锚到**调用方分区的当前目录**（模型 cd 之后
// 就是那个目录），不再是恒定的工作区根。文案跟着改，否则又是一条"说明与行为
// 不符"——模型按文案以为相对路径永远从根解析，cd 到子目录后就找不到文件。
//
// 边界语义**没变**：相对路径不得逃出**工作区根**（安全边界仍是工作区，不是锚点）。
// 不接会话级 cwd 状态时（NewFileTools）锚点恒为工作区根 ⇒ 与旧文案逐字同义。
const pathParamDesc = "File path. A relative path is resolved against the current working " +
	"directory (the workspace root until the session changes it) and may not escape the " +
	"workspace root; use an absolute path for files outside the workspace."

// resolve 校验并解析路径为绝对路径（问题十六：与沙箱/审批统一治理）。
//   - 相对路径 → 锚定 workspace root（既有行为；`..` 逃逸仍拒绝——相对语义下
//     工作区外无意义，出工作区请用绝对路径表达显式意图）；
//   - 绝对路径 → Clean 后原样采用：写盘合法性交由统一层治理（Seatbelt 管 bash
//     进程；mode.Approve 审批管所有工具调用），工具层不再自行锚死 workspace；
//   - go-code 自身配置/密钥目录（~/.go-code）显式拒绝——与 Seatbelt 的
//     defaultSensitivePaths 同一保护对象，防止模型篡改 harness 自身配置。
//
// **denyHarnessConfigPath 必须对两条分支都生效**（2026-10-09 修复，此前只在绝对
// 路径分支上调）。攻击面不是理论上的：
//
//	模型: bash "ln -s ~/.go-code /workspace/cfg"
//	模型: read_file path="cfg/settings.json"
//	  → 相对分支只做 HasPrefix(workspace) 词法判定 → 放行
//	  → os.ReadFile 跟随软链接 → 读到明文 api_key
//
// 而绝对路径那条路（`/workspace/cfg/settings.json`）是被拦的——同一个文件只因写法
// 不同而结论相反。根因是"路径从哪来"被当成了"路径到哪去"的判据，而 deny 的本职是
// 后者：Join 之后两条分支产出的都是绝对路径，就该受同一套判定。
//
// 无 ctx 版本（锚点 = 工作区根）：给**没有会话级 cwd 状态**的调用方与测试用；
// 生产路径一律走 resolveAt（锚点 = 调用方分区的当前目录）。
func (f *FileTools) resolve(raw string) (string, error) {
	return f.resolveFor(context.Background(), raw, PathRead)
}

// resolveAt 按**调用方**解析路径（会话级 cwd 口径）。
//
// 相对路径锚到该调用方分区的当前目录（f.wd 为 nil 时 = 工作区根，与 resolve 同）。
func (f *FileTools) resolveAt(ctx context.Context, raw string) (string, error) {
	return f.resolveFor(ctx, raw, PathRead)
}

// resolveWrite 写意图的路径解析（write_file / edit_file）：与 resolve 逐字同源，
// 只多一条「只读子树拒绝」。**必须共用同一份实现**，否则读写两条路的拒绝理由会漂。
func (f *FileTools) resolveWrite(raw string) (string, error) {
	return f.resolveFor(context.Background(), raw, PathWrite)
}

// resolveWriteAt 写意图的路径解析（按调用方锚点，见 resolveAt）。
func (f *FileTools) resolveWriteAt(ctx context.Context, raw string) (string, error) {
	return f.resolveFor(ctx, raw, PathWrite)
}

func (f *FileTools) resolveFor(ctx context.Context, raw string, op PathOp) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	base := ""
	if !filepath.IsAbs(raw) {
		base = f.cwd(ctx)
	}
	p := filepath.Clean(filepath.Join(base, raw))
	// 边界：相对路径不得逃出**工作区根**。
	//
	// 注意判据是 root 而不是 base（锚点）：锚点现在可能是工作区里的子目录
	//（模型 cd 过），而"工作区"才是安全边界 —— 相对路径可以走出锚点（cd 到
	// sub/ 后读 ../a.txt 是工作区内的正常操作），但不能走出工作区。
	// 没接会话级 cwd 状态时 base == root ⇒ 与改造前逐字同义。
	if base != "" && f.root != "" && p != f.root && !strings.HasPrefix(p, f.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root %q", raw, f.root)
	}
	if err := denyHarnessConfigPath(p, op); err != nil {
		return "", err
	}
	return p, nil
}

// WorkdirState 暴露本工具集使用的会话级 cwd 状态（tools.WorkdirAware）。
//
// 装配层用它证明「bash / 文件工具 / run_python 共用同一份」—— 见 tools.WorkdirAware。
func (f *FileTools) WorkdirState() *workdir.State { return f.wd }

// cwd 当前工作目录（**会话级**）：有 workdir 状态时按调用方分区取；否则 = 工作区根。
func (f *FileTools) cwd(ctx context.Context) string {
	if f.wd != nil {
		return f.wd.Current(CwdKey(ctx))
	}
	return f.root
}

// denyHarnessConfigPath 拒绝 go-code 自身配置/密钥路径（~/.go-code）：settings.json
// 可能含 api_key。无法定位 home 时放弃该保护（与旧行为一致，不阻塞正常路径）。
//
// # 必须解析软链接（2026-09-29，Project Memory 放行 memory 子树后补上）
//
// 放行 memory 子树让模型获得了**在记忆目录里创建任意文件**的能力：bash 的 `ln -s`
// 不在危险命令名单里，而 memory 子树在沙箱放行子树内。于是出现一条完整攻击链：
//
//	模型: bash "ln -s ~/.go-code/settings.json ~/.go-code/memory/repos/<id>/steal.md"
//	模型: read_file ~/.go-code/memory/repos/<id>/steal.md
//	  → 词法判定落在 memory 子树 → 放行
//	  → os.ReadFile **跟随软链接** → 读到 settings.json 里的明文 api_key
//
// 关键：read_file 跑在主 Agent 进程里、**不进沙箱**，本函数是它唯一的防线。
// 所以不能只做词法比较 —— 必须把**真实目标**解析出来再判定，否则 memory 子树
// 就成了通往受保护目录的桥。
//
// 解析后若真实目标仍在 ~/.go-code 内，按同一份放行名单判定；若解析到
// ~/.go-code 之外，那与模型直接读那个绝对路径无区别，不是本函数新增的攻击面。
//
// # 硬链接是 EvalSymlinks 看不见的同型链（2026-10-09 补）
//
// 上面的链路全部围绕软链接，但 `ln`（不带 `-s`，建硬链接）与 `ln -s` 同样常用，
// 而 memory 子树在沙箱放行名单里：
//
//	模型: bash "ln ~/.go-code/settings.json ~/.go-code/memory/repos/<id>/note.md"
//	模型: read_file ~/.go-code/memory/repos/<id>/note.md
//	  → EvalSymlinks 对该路径无感（硬链没有"链接"可解，它自己就是真实名字）
//	  → 词法命中放行名单 → 放行
//	  → os.ReadFile  → 读到明文 api_key
//
// 硬链与源文件共享 inode，所以唯一的判据是 inode 比对。受保护侧文件数很少且不随
// 会话增长，反过来扫它们是廉价的；若扫放行子树（memory 可能有上千个文件）则不划算。
func denyHarnessConfigPath(abs string, op PathOp) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	real := resolveRealTarget(abs)
	// home 也要解析：macOS 上 /tmp 是 /private/tmp 的软链接，HOME=/tmp/xxx 时
	// 解析出的目标带 /private 前缀而 cfgDir 不带，两边基准不一致 → 判定整体失效。
	// 判据：软链接解析后的真实路径与配置根必须在**同一基准**上比较。
	if resolvedHome, rerr := filepath.EvalSymlinks(home); rerr == nil {
		home = resolvedHome
	}
	cfgDir := filepath.Clean(filepath.Join(home, ".go-code"))
	if real == cfgDir || strings.HasPrefix(real, cfgDir+string(filepath.Separator)) {
		// 受管子树放行：plugins（Browser Use 等受管组件的安装/调试需要文件工具可达）
		// 与 runtime（受管 Python 运行时 ~/.go-code/runtime/python：venv 解释器与
		// 依赖库需可读、__pycache__ 需可写）—— 与沙箱 carve-out
		// （defaultOpenSubpaths）同语义，两处必须同步，否则沙箱能跑而文件工具读写不了。
		// memory（Project Memory，官方 auto memory 目录）：官方形态就是"模型用**标准
		// read_file / write_file / edit_file** 读写记忆目录"，没有记忆专用工具
		// （技术方案 §1）。不放行它 = 模型既读不到注入的索引指向的 topic，也写不进
		// 新记忆，auto memory 整个不成立。该子树下只有记忆文件（0600），不含密钥。
		// previews（R08a 预览产物缓存）只读放行，理由见 readOnlyConfigSubtrees。
		// .go-code 其余（settings.json 明文密钥 / sessions / events）仍保护。
		for _, sub := range readOnlyConfigSubtrees {
			dir := filepath.Join(cfgDir, sub)
			if real == dir || strings.HasPrefix(real, dir+string(filepath.Separator)) {
				if isHardlinkToProtected(abs, cfgDir) {
					return fmt.Errorf("path %q is a hard link to a protected go-code config file", abs)
				}
				if op == PathWrite {
					return fmt.Errorf("path %q is inside go-code's preview cache (%s), which is read-only for file tools: "+
						"entries are generated from the source document and must not be edited", abs, sub)
				}
				return nil
			}
		}
		for _, sub := range allowedConfigSubtrees {
			dir := filepath.Join(cfgDir, sub)
			if real == dir || strings.HasPrefix(real, dir+string(filepath.Separator)) {
				// 硬链接桥：放行子树内的名字可以与受保护文件**共享 inode**，词法与
				// EvalSymlinks 都看不出来。放行之前先否掉它（见本函数顶部的链路说明）。
				if isHardlinkToProtected(abs, cfgDir) {
					return fmt.Errorf("path %q is a hard link to a protected go-code config file", abs)
				}
				return nil
			}
		}
		return fmt.Errorf("path %q resolves to %q, which is go-code's own config/secret directory and is protected", abs, real)
	}
	return nil
}

// DenyHarnessConfigPath 是 denyHarnessConfigPath 的导出门面。
//
// 存在的理由：desktop/bridge 另有**一份拷贝**（main_helpers.go）服务于预览通路
// （file_preview / xlsx_preview 等）。那份拷贝是纯词法的——不解析软链接、放行名单
// 还漏了 memory，于是同一条链路在预览通路里畅通无阻。把它改为委托本函数，让
// "~/.go-code 保护"只有一份实现（否则每修一次都要记得同步两处，而已经漂移过一次）。
func DenyHarnessConfigPath(abs string) error { return denyHarnessConfigPath(abs, PathRead) }

// DenyHarnessConfigPathWrite 写意图的门面（编辑器保存 / 任何写盘通路）：
// 与读门面同一份判定，只多拒绝只读子树（previews）。
func DenyHarnessConfigPathWrite(abs string) error { return denyHarnessConfigPath(abs, PathWrite) }

// protectedConfigFiles ~/.go-code 下**可能含明文密钥**的配置文件。
//
// 硬链接比对只扫这份名单（而不是整个 ~/.go-code）：这些是已知的密钥载体，
// 文件数固定、不随会话增长，逐个 stat 的成本可以忽略。新增密钥文件时加进来。
var protectedConfigFiles = []string{
	"settings.json", // provider api_key / base_url
	"keys.json",     // 模型密钥
	"im-config.json",
	"oauth.json",
}

// isHardlinkToProtected 判定 abs 是否与某个受保护配置文件**共享 inode**。
//
// 判据是 (Dev, Ino) 而不是只看 Ino：Ino 只在同一文件系统内唯一，而 ~/.go-code 理论上
// 可能跨卷（用户把部分内容放到另一块盘上）。 Dev+Ino 同时相等才是同一个文件。
//
// 取不到 stat（文件不存在 / 不是常规文件 / 平台没有 Stat_t）时返回 false：
// 判不了就当没有硬链，其余判定（词法 + EvalSymlinks）仍然生效。
func isHardlinkToProtected(abs, cfgDir string) bool {
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	for _, name := range protectedConfigFiles {
		ti, err := os.Stat(filepath.Join(cfgDir, name))
		if err != nil || !ti.Mode().IsRegular() {
			continue
		}
		ts, ok := ti.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if ts.Dev == st.Dev && ts.Ino == st.Ino {
			return true
		}
	}
	return false
}

// PathOp 文件工具的访问意图。**同一个路径，读与写的结论可以不同**：
// previews（预览产物缓存）对读放行、对写拒绝 —— 前端要读回产物，但任何人都
// 不该改写它（改了就是给用户看「带 app 外壳的伪造文档」）。
type PathOp int

const (
	// PathRead 读（read_file / file_preview / @引用 / 列目录）。
	PathRead PathOp = iota
	// PathWrite 写（write_file / edit_file / 编辑器保存）。
	PathWrite
)

// allowedConfigSubtrees ~/.go-code 下对文件工具放行的受管子树（**读+写**）。
//
// 与 sandbox.defaultOpenSubpaths 是**同一份语义的两次列举**（沙箱放行 shell 侧、
// 本名单放行文件工具侧）。两处不同步就会出现"沙箱能跑而文件工具读写不了"或反向。
// 由 TestAllowedSubtreesMatchSandboxOpenSubpaths 钉住同步。
var allowedConfigSubtrees = []string{"plugins", "runtime", "memory"}

// readOnlyConfigSubtrees 放行**读**、拒绝**写**的受管子树。
//
// 与 sandbox.defaultReadOnlyOpenSubpaths 同语义（沙箱侧用 SBPL 的 deny-write 收尾，
// 见 seatbelt buildPolicy）；两处必须同步，否则「工具写不了但 bash 能写」只是把洞挪了个位置。
//
// previews（R08a 预览产物缓存 ~/.go-code/previews）为什么必须只读：
//   - 必须要读：预览面板把产物 HTML 读回前端（file_preview 读 html_path），不放行就是
//     「转换成功但读不回」；
//   - 必须不能写：缓存条目 = 「这份文档的预览」。能写就意味着模型可以把 index.html 换成
//     任意 HTML，用户在面板里看到的是**看起来属于这份文档**的伪造内容（带 app 外壳的
//     钓鱼面）。缓存键是内容哈希，模型也能自己算出来 —— 所以「难猜」不是防线。
//     只读才是。
var readOnlyConfigSubtrees = []string{"previews"}

// resolveRealTarget 解析路径的**真实目标**：对最深的已存在祖先做 EvalSymlinks，
// 再把剩余（可能尚不存在）的部分原样接回去。
//
// 为什么不能直接 filepath.EvalSymlinks(abs)：write_file 写新文件时目标不存在，
// EvalSymlinks 报 ENOENT。而那正是最需要判定的场景之一（模型新建软链接时，
// 链接本身存在；它指向的 settings.json 通常也存在，但新记忆文件确实不存在）。
func resolveRealTarget(abs string) string {
	p := filepath.Clean(abs)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			if rest == "" {
				return resolved
			}
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs // 理论上到不了这里（根必然存在）
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// ---------- read_file ----------

const defaultReadMaxLines = 200

// maxFileToolReadBytes write_file/edit_file 的**整文件读**上限（字节）。
//
// 改造前 io.ReadAll 无上限 ⇒ 一个 100 MB 文件：读 100 MB + string() 拷一份
// + strings.Replace 拷一份 + diff 的 splitLines ×2 ⇒ 峰值 **4–6× 文件大小**
// （100 MB ⇒ 400–600 MB RSS）。模型能改的文件都是文本文件，16 MiB 足够；
// 更大的文件请走 bash/python（错误文案会指路）。
const maxFileToolReadBytes = 16 << 20

const (
	// readFileLineCap 单行保留上限（字节）。超长行（minified bundle / base64 blob /
	// 单行大 JSON）只保留前 64 KB —— 引擎的响应闸门是 20 KB，64 KB 已远超模型能看到的量。
	readFileLineCap = 64 << 10
	// readFileBytesCap 单次返回的内容总上限（字节）。
	// 行数有上限（200）但**每行没有** ⇒ 200 行 × 4 MB 的超长行 = 800 MB。
	readFileBytesCap = 256 << 10
	// readFileCtxCheckEvery read_file 扫描循环里查 ctx 的间隔（行数）。
	readFileCtxCheckEvery = 64
	// maxImageRawBytes 图片**原始文件**的体积上限（字节）。
	// 编码体积闸（3 MiB base64）是在读完之后才判的 ⇒ 读的时候需要另一道闸。
	// 64 MiB：真实照片/截图远小于此（17 MB 的 PNG 有测试覆盖）。
	maxImageRawBytes = 64 << 20
)

type readFileTool struct {
	tools.BaseTool
	*FileTools
	maxLines int // 未指定 end_line 时的默认区间行数（默认 200）

	// maxImageBytes 单张内嵌图片 data URL 上限（0 = 用引擎默认闸门 defaultMaxImageBytes）。
	// 与 tools 引擎的图片闸门同口径：此处先降采样到达标，引擎那层才不会丢弃。
	// 只读（构造后不变）→ 并行调用安全。
	maxImageBytes int
}

func newReadFileTool(f *FileTools) *readFileTool {
	return &readFileTool{
		BaseTool: tools.BaseTool{
			Name_:        "read_file",
			Description_: "Read a text file and return numbered lines. path is required and relative to the current working directory (the workspace root until the session changes it). start_line and end_line are optional 1-based inclusive integers. Omit both for the first 200 lines; if only start_line is given, read up to 200 lines from there. If both are given in reverse order, the tool normalizes them to the ascending range and reports the normalization. Reading an image file (png/jpeg/gif) returns the image itself for direct viewing (do not pass start_line/end_line for images). Use glob to locate files and grep to search content.",
			Params_: tools.Obj(map[string]any{
				"path":       tools.Str(pathParamDesc),
				"start_line": tools.Map{"type": "integer", "minimum": 1, "default": 1, "description": "First line, 1-based; omit for line 1"},
				"end_line":   tools.Map{"type": "integer", "minimum": 1, "description": "Last line, inclusive; omit to read the default 200-line window. If end_line is smaller than start_line, the tool normalizes the range to ascending order and reports it."},
			}, "path"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		FileTools: f,
		maxLines:  defaultReadMaxLines,
	}
}

type readArgs struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line"`
	EndLine   *int   `json:"end_line"`
}

// readHead 读文件前 n 字节（格式判别用：魔数只在前几十字节，大图不必整读）。
// 文件短于 n 时返回实际读到的部分（无错）。
func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, n)
	read, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return head[:read], nil
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明 read_file 的工具级时限。
//
// 改造前 read_file **不看 ctx** ⇒ 引擎的 5 分钟工具超时对它完全无效
// （超时是靠取消 ctx 实现的，工具不看就收不到）；用户 abort 后 goroutine
// 还在后台把文件读完。现在扫描循环按行抽样查 ctx.Err()（见 readLinesRange）。
func (t *readFileTool) ToolTimeout() time.Duration { return fileToolTimeout }

func (t *readFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a readArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("read_file", err.Error())
	}
	if a.Path == "" {
		return requiredArgument("read_file", "path")
	}
	if (a.StartLine != nil && *a.StartLine < 1) || (a.EndLine != nil && *a.EndLine < 1) {
		start, end := "omitted", "omitted"
		if a.StartLine != nil {
			start = fmt.Sprintf("%d", *a.StartLine)
		}
		if a.EndLine != nil {
			end = fmt.Sprintf("%d", *a.EndLine)
		}
		return recoveryError("invalid_line_range", "correct_arguments", fmt.Sprintf("start_line=%s, end_line=%s; line numbers must be >= 1 when provided", start, end), "Use positive 1-based line numbers, or omit start_line/end_line for the default range.")
	}
	// A reversed range is recoverable without asking the model to repeat a tool
	// call. Call normalizes it below, preserving the requested interval while
	// avoiding the common end_line < start_line failure loop.
	return nil
}

func (t *readFileTool) Call(ctx context.Context, _, arguments string) (string, error) {
	// ctx 已取消/已超时 ⇒ 立刻返回，不做任何 I/O（改造前工具完全不看 ctx）。
	if err := ctx.Err(); err != nil {
		return "", toolCancelledError(ctx, "read_file")
	}
	var a readArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolveAt(ctx, a.Path)
	if err != nil {
		return "", err
	}
	start := 1
	if a.StartLine != nil {
		start = *a.StartLine
	}
	end := 0
	if a.EndLine != nil {
		end = *a.EndLine
	}
	normalizedRange := false
	if end > 0 && end < start {
		// Models occasionally swap the two line numbers after reading a grep
		// result. Treat the pair as an unordered selection rather than failing
		// the whole tool round; this is deterministic and has no side effect.
		start, end = end, start
		normalizedRange = true
	}
	// 窗口硬钳（2026-11-17，第 2 步）：**模型显式给了 end_line 也只给 maxLines 行**。
	//
	// 改造前只在 end_line **缺失**时兜底成 maxLines 行；模型同时给 start_line/end_line
	// 时**没有任何上限** ⇒ 一次调用把整个文件收进内存（100 MB 文件 ⇒ 峰值 200 MB
	// —— lines 切片 + 行号前缀的 Builder 拷贝），而模型侧真正能看到的只有引擎
	// 截断后的 20 KB（99.9% 的产出被扔掉，模型拿到的是**残缺**上下文）。
	// 现在：超出的部分用 start_line 续读（表头明说下一段从哪一行开始）。
	clamped := false
	if end <= 0 {
		end = start + t.maxLines - 1
	} else if end > start+t.maxLines-1 {
		end = start + t.maxLines - 1
		clamped = true
	}

	// 一次打开，全流程复用（2026-11-17，第 2 步）：
	// 改造前是**3 次文件系统往返** —— statPathHint(Stat) + readHead(Open+read 512B)
	// + readLines(Open+read 全文)。网络盘（NFS/sshfs）上每次触碰一个 RTT。
	// 现在：Open 一次 → 读头 512 B（魔数判别）→ 需要时 seek(0) 复用同一个 fd。
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", notFoundError(p, "read_file", similarPathHint(p))
		}
		return "", err
	}
	defer f.Close()

	// 头 512 B（魔数判别）：同一个 fd，不另开一次。
	head := make([]byte, 512)
	hn, herr := io.ReadFull(f, head)
	if herr != nil && herr != io.ErrUnexpectedEOF && herr != io.EOF {
		return "", herr
	}
	head = head[:hn]

	// 图片分支：魔数识别 → 内嵌为 image 内容块（模型直接看图）。行区间参数对
	// 图片无意义（不按行切图），故只要指定了行区间即明确拒绝而不是静默忽略——
	// 让模型知道「这里读到了图，range 没生效」，避免它以为读的是文本片段。
	if format, isImg := detectImageFormat(head); isImg {
		if a.StartLine != nil || a.EndLine != nil {
			return "", fmt.Errorf("read_file: %s is an image (%s); line ranges do not apply to images — call read_file with just the path to view it", a.Path, format.mime)
		}
		// 原始体积上限（2026-11-17，第 4 步）：改造前 `os.ReadFile` **无上限** ——
		// 一个 2 GB 的 PNG ⇒ 2 GB 常驻 + 之后 image.Decode 再 3.6 GB。
		// 编码体积闸（3 MiB）在**读完之后**才判 ⇒ 读的时候没有任何保护。
		if st, serr := f.Stat(); serr == nil && st.Size() > maxImageRawBytes {
			return "", fmt.Errorf("read_file: %s is %s — too large to read inline (limit %s); shrink or convert it first (e.g. `sips -Z 4000 %s`)",
				a.Path, humanSize(int(st.Size())), humanSize(maxImageRawBytes), a.Path)
		}
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return "", serr
		}
		data, rerr := io.ReadAll(f)
		if rerr != nil {
			return "", rerr
		}
		res, ierr := loadImageForModel(ctx, p, data, t.maxImageBytes)
		if ierr != nil {
			return "", ierr
		}
		tools.ImageSinkFrom(ctx).Add(res.blocks...)
		return res.text, nil
	}
	// 非图片的二进制（可执行文件/压缩包/媒体容器）：明确拒绝而不是倒乱码。
	if looksBinary(head) {
		kind := "binary"
		if imageExtHint(p) {
			kind = "binary (image-like extension, but the content is not a decodable image)"
		}
		size := ""
		if st, serr := f.Stat(); serr == nil {
			size = ", " + humanSize(int(st.Size()))
		}
		// 事实部分（文件名/形态/体积 + read_file 的能力边界）由本层拼；尾句「下一步
		// 怎么办」交给宿主注入的 hint —— 默认指 bash 的现成工具链，未注册 bash 的产品
		// 形态（桌面 Work persona）换成本会话真有的路径。空串 = 宿主明确不给建议（不回退
		// 默认：注入方已表明自己能力面里没有 bash，再提示它只会让模型调不存在的工具）。
		hint := defaultBinaryHint
		if t.binaryHint != nil {
			hint = t.binaryHint(a.Path)
		}
		tail := "" // 连前导空格一起省，空提示不留尾空格
		if hint != "" {
			tail = " " + hint
		}
		return "", fmt.Errorf("read_file: %s looks like a %s file%s; read_file only returns text or decodable images (png/jpeg/gif).%s", a.Path, kind, size, tail)
	}

	// 文本：回到文件头扫（同一个 fd ⇒ 只多一次 seek，不再开第二次）。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	lines, more, byteCut, err := readLinesRange(ctx, f, start, end)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	last := start + len(lines) - 1
	switch {
	case len(lines) == 0:
		fmt.Fprintf(&b, "[read_file: %s, no lines in range %d-%d (end of file)]\n", a.Path, start, end)
	case byteCut:
		fmt.Fprintf(&b, "[read_file: %s, lines %d-%d (content truncated at %d bytes; use start_line=%d to continue)]\n",
			a.Path, start, last, readFileBytesCap, last+1)
	case len(lines) < end-start+1:
		// 文件在请求区间结束前就没了（模型要么越界，要么文件就这么长）。
		fmt.Fprintf(&b, "[read_file: %s, lines %d-%d (end of file)]\n", a.Path, start, last)
	case clamped:
		// 窗口被硬钳 ⇒ 明说下一段从哪开始（模型能续读，而不是拿到残缺上下文）。
		fmt.Fprintf(&b, "[read_file: %s, lines %d-%d (at most %d lines per call; use start_line=%d to continue)]\n",
			a.Path, start, last, t.maxLines, last+1)
	case more:
		fmt.Fprintf(&b, "[read_file: %s, lines %d-%d (more lines; use start_line=%d to continue)]\n",
			a.Path, start, last, last+1)
	default:
		fmt.Fprintf(&b, "[read_file: %s, lines %d-%d]\n", a.Path, start, last)
	}
	if normalizedRange {
		fmt.Fprintf(&b, "[line range normalized from %d-%d to %d-%d]\n", *a.StartLine, *a.EndLine, start, end)
	}
	for i, l := range lines {
		fmt.Fprintf(&b, "%d: %s\n", start+i, l)
	}
	notice, err := t.notifyPostRead(ctx, p)
	if err != nil {
		return "", err
	}
	return withNotice(b.String(), notice), nil
}

// notifyPostRead 触发宿主"读成功后"钩子；未注入钩子时是零成本空操作。
// 与 notifyPostWrite 同理：只在**读成功之后**调用——读失败时什么也没发生，
// 报"回忆了记忆"是假的。
func (t *readFileTool) notifyPostRead(ctx context.Context, absPath string) (string, error) {
	if t.FileTools == nil || t.FileTools.postRead == nil {
		return "", nil
	}
	return t.FileTools.postRead(ctx, absPath)
}

// statPath 操作前预检查目标存在性（给 LLM 可操作提示，而非裸错误）。
func statPath(p, tool string) error {
	_, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return notFoundError(p, tool, "")
	}
	return err
}

// notFoundError 组装「目标不存在」错误文案（statPath 与 statPathHint 共用，两处不漂移）。
func notFoundError(p, tool, hint string) error {
	return fmt.Errorf("%s: file not found: %s%s (use glob to locate the file first, or inspect the workspace)", tool, p, hint)
}

// statPathHint 同 statPath，但「文件不存在」时附一条同目录近似条目建议（read_file 专用：
// 记错文件名是它最常见的失败，时间序列显示「文件不存在率」6-9% 不升不降）。
// 仅失败路径读目录 —— 成功路径零额外开销（read_file 是高频工具）。
// 其余情况（文件在、权限错等）与 statPath 逐字一致。
func statPathHint(p, tool string) error {
	_, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return notFoundError(p, tool, similarPathHint(p))
	}
	return err
}

// maxPathSuggestions 「did you mean」最多列几条同目录候选：再多就变成噪声清单，
// 模型反而要在候选里做二次选择（建议的用途只是「你是不是记错了这个名字」）。
const maxPathSuggestions = 3

// similarPathHint 为「文件不存在」的路径挑一条可操作建议：在**同目录**（目录本身也不存在时
// 逐级上溯到最近的已存在目录）里找最像的条目名，返回 `; did you mean "x.go" …?`；
// 目录不可读 / 条目不相似时返回 ""（调用方退化到原文案，行为与新增前一致）。
// 复用 B6 的相似度纯函数（tools.SuggestSimilarFileNames：公共前缀 + 最长公共子串、
// 大小写不敏感、先剥扩展名），与工具名建议同一取舍：不够像就不猜。
// 只读目录：不写盘、**不替换目标路径** —— 建议是错误文案而非自动纠错（猜错文件比报错更糟，
// 同 GetTool 的 did-you-mean 取舍）；模型仍须自己发起一次调用。
func similarPathHint(p string) string {
	parent := filepath.Dir(p)
	dir, ok := nearestExistingDir(parent)
	if !ok {
		return ""
	}
	entries, err := os.ReadDir(dir) // 已按名字排序 → 建议文本确定、可断言（同 GetTool 的清单排序）
	if err != nil {
		return "" // 目录不可读（权限等）：不在一个错误里再叠一个错误
	}
	names := make([]string, 0, len(entries))
	isDir := make(map[string]bool, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		isDir[e.Name()] = e.IsDir()
	}
	cands := tools.SuggestSimilarFileNames(missingComponent(p, dir), names, maxPathSuggestions)
	if len(cands) == 0 {
		return ""
	}
	// 目录候选带尾斜杠：告诉模型「这是目录、不是你要读的文件」（read_file 读目录只会得到
	// not_a_file，它该去 glob/列目录找目录里的文件）。
	for i, n := range cands {
		if isDir[n] {
			cands[i] = n + "/"
		}
	}
	return "; did you mean " + quoteCandidates(cands) + "?"
}

// missingComponent 取「该拿去比对的名字」：模型写错的到底是文件名还是目录名，取决于路径在
// 哪一段开始不存在。
//   - dir 就是 p 的父目录 → 比 p 的文件名（"repot.md" vs 同目录的 "report.md"）；
//   - 上溯过（父目录不存在）→ 比**第一个缺失的路径分量**（"componets/x.ts" 该比 "componets"，
//     而不是 x.ts：目录名写错时拿文件名去比只会得到无关候选）。
func missingComponent(p, dir string) string {
	if filepath.Clean(dir) == filepath.Clean(filepath.Dir(p)) {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.Base(p)
	}
	if first, _, _ := strings.Cut(rel, string(os.PathSeparator)); first != "" && first != "." && first != ".." {
		return first
	}
	return filepath.Base(p)
}

// nearestExistingDir 从 dir 起逐级上溯，返回最近的已存在目录（目标路径的父目录常常也不存在：
// 模型写错目录名时，能在最近的真实目录里找到它想读的文件）。
// 一路到根都不存在 → ok=false（调用方不给建议，不 panic）。
func nearestExistingDir(dir string) (string, bool) {
	for {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // 已到根（filepath.Dir 对根返回自身）
		}
		dir = parent
	}
}

// quoteCandidates 把候选名拼成好读的短语：`"a"`、`"a" or "b"`、`"a", "b" or "c"`。
func quoteCandidates(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// readLinesRange 读 [start, end] 闭区间的行；返回（行内容, 文件在 end 之后是否还有行）。
//
// **扫描到 end 就停**（再多读一行只为判断「还有更多」）——
// 改造前为了在表头报一句「N lines total」把**整个文件**读完：读 200 行 = O(N) 磁盘读
// （100 MB 日志 ⇒ 每次 read_file 读 100 MB 只回 8 KB，**12500× 放大**）。
// 「总行数」不是模型能用的信息（它要的是内容），而「还有更多、从第几行续读」是。
//
// 用 grepScanner（bufio.Reader）而不是 bufio.Scanner，两条都解决：
//   - Scanner 遇到 > 4 MB 的单行报 ErrTooLong ⇒ **整个 read_file 失败**
//     （minified bundle / base64 blob / 单行大 JSON 一律读不了，连前 200 行都看不到）；
//   - Scanner 没有 Reset ⇒ 每文件一份 64 KB 缓冲；Reader 有且能复用。
func readLinesRange(ctx context.Context, f *os.File, start, end int) ([]string, bool, bool, error) {
	sc := newGrepScannerWithCap(readFileLineCap)
	sc.reset(f)
	var lines []string
	ln := 0
	total := 0 // 已收集的内容字节数
	for {
		line, ok := sc.nextLine()
		if !ok {
			break // EOF
		}
		ln++
		// 取消/超时检查（按行数抽样：ctx.Err() 不是免费的 —— 引擎的
		// liftableTimeout.Err() 要加锁）。read_file 的窗口 ≤ 200 行 ⇒ 间隔取
		// searchCtxCheckEvery/64（每 64 行查一次）—— 够密又不至于把开销变成大头。
		if ln%readFileCtxCheckEvery == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return nil, false, false, cerr
			}
		}
		if ln >= start && ln <= end {
			// 字节闸：行数有上限（200）但**每行没有** ⇒ 200 行 × 4 MB 的超长行
			// = 800 MB。引擎的响应闸门只有 20 KB ⇒ 256 KB 已远超模型能看到的量。
			if total+len(line) > readFileBytesCap {
				return lines, true, true, nil // 内容已到上限：后面还有
			}
			total += len(line)
			text := string(line) // 必须拷贝：nextLine 的切片下次调用即失效
			if sc.cut {
				text += fmt.Sprintf(" …[truncated at %d bytes]", readFileLineCap)
			}
			lines = append(lines, text)
		}
		if ln >= end {
			// 再读一行只为判断「文件还有更多」（不保留内容）。
			_, more := sc.nextLine()
			return lines, more, false, nil
		}
	}
	return lines, false, false, nil
}

// ---------- write_file ----------

type writeFileTool struct {
	tools.BaseTool
	*FileTools
	// diff 镜像（**直调/旧宿主兼容**）：引擎读的是 ctx 槽（tools.DiffSink），
	// 因为工具实例在注册表里共享 —— 主 agent 与后台子 agent 并发调用同一实例时，
	// 实例字段会互相清空/串味（产出卡挂到别的文件上）。
	dmu  sync.Mutex
	diff *core.FileDiff
}

// Diff 实现 tools.ToolDiffProvider：返回最近一次成功调用的变更 diff。
// ⚠️ 引擎**不再**读它（读 ctx 槽，见 tools.DiffSink 的注释）；它只留给直调与旧宿主。
func (t *writeFileTool) Diff() *core.FileDiff {
	t.dmu.Lock()
	defer t.dmu.Unlock()
	return t.diff
}

// setDiff 同时写镜像与 ctx 槽（引擎读后者 ⇒ 并发下不会串味）。
func (t *writeFileTool) setDiff(ctx context.Context, d *core.FileDiff) {
	t.dmu.Lock()
	t.diff = d
	t.dmu.Unlock()
	tools.SetFileDiff(ctx, d)
}

func newWriteFileTool(f *FileTools) *writeFileTool {
	return &writeFileTool{
		BaseTool: tools.BaseTool{
			Name_: "write_file",
			Description_: "Write a file (overwrites by default; append=true appends). To modify an existing file, prefer edit_file (sends only the changed fragment, far fewer tokens); use this tool to create new files or rewrite a whole file. " +
				"Overwrite is atomic (temp file + rename; concurrent readers never see partial content); concurrent write conflicts return an error (retryable). " +
				"Creates missing files and parent directories automatically; if you need to keep the old content before overwriting, back it up with read_file first.",
			Params_: tools.Obj(map[string]any{
				"path":    tools.Str(pathParamDesc),
				"content": tools.Str("Text content to write (full new file content)"),
				"append":  map[string]any{"type": "boolean", "default": false, "description": "Append mode (default false = overwrite)"},
			}, "path", "content"),
			CanParallel_: false, // 写文件不并行（避免同文件竞争）
		},
		FileTools: f,
	}
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明 write_file 的工具级时限。
// 语义与 read_file 一致（引擎的 5 分钟兜底对不看 ctx 的工具无效）。
func (t *writeFileTool) ToolTimeout() time.Duration { return fileToolTimeout }

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func (t *writeFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a writeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if a.Path == "" || a.Content == "" {
		return errors.New("write_file: path and content are required")
	}
	return nil
}

func (t *writeFileTool) Call(ctx context.Context, _, arguments string) (string, error) {
	// ctx 已取消/已超时 ⇒ 立刻返回（改造前工具完全不看 ctx）。
	if err := ctx.Err(); err != nil {
		return "", toolCancelledError(ctx, "write_file")
	}
	// 清空本次调用的 diff（ctx 槽 + 镜像）：上次调用的变更不得泄漏到本次。
	t.setDiff(ctx, nil)
	var a writeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolveWriteAt(ctx, a.Path)
	if err != nil {
		return "", err
	}
	if a.Append {
		// 追加（产物汇总修复，2026-09-15）：此前 append 分支不产 diff，导致
		// 追加写永远不进产出列表（AgentEnd.Artifacts）。现在产出 Added + 指纹。
		//
		// 注意语义边界：content 是**增量**而非文件全文——
		//   - Added  = 增量行数（正确）
		//   - SHA256 = **增量**指纹（用于变更判定：同样内容重复 append 可得同一值）
		//   - Size/Lines = **留空**：全量值需读整个文件，与「工具侧零额外 IO」冲突；
		//     宿主对空值不展示该维度（诚实优于错数）。
		// Created 由 Stat 判定（appendTo 用 O_CREATE，文件可能此前存在）。
		_, statErr := os.Stat(p)
		if err := appendTo(p, a.Content); err != nil {
			return "", err
		}
		created := errors.Is(statErr, os.ErrNotExist)
		var d *core.FileDiff
		if created {
			// 新建文件：追加内容即全文 → size/lines/指纹均为精确事实
			d = finalizeDiff(&core.FileDiff{Path: a.Path, Added: strings.Count(a.Content, "\n")}, a.Content)
		} else {
			// 既有文件：content 是纯增量 → Added 是增量语义；
			// 全量 size/lines 需读整个文件（与「工具侧零额外 IO」冲突）→ 留空，
			// 宿主对空值不展示该维度（诚实优于错数）。指纹为增量指纹（变更判定有效）。
			d = &core.FileDiff{
				Path:   a.Path,
				Added:  strings.Count(a.Content, "\n"),
				SHA256: contentFingerprint(a.Content),
			}
		}
		d.Created = created
		t.setDiff(ctx, d)
		added := d.Added
		notice, err := t.notifyPostWrite(ctx, p)
		if err != nil {
			return "", err
		}
		return withNotice(fmt.Sprintf("appended (+%d)", added), notice), nil
	}
	// 覆盖：锁内读旧内容（若存在）→ 替换 → 算 diff。旧内容须在替换前读取，
	// 与 edit_file 同款锁内读改写全程无竞态；替换后旧内容不可再得（信息源头在工具）。
	lock, err := lockPath(p)
	if err != nil {
		return "", err
	}
	locked := lock != nil
	if locked {
		defer func() {
			if locked {
				lock.Close()
			}
		}()
	}
	var old string
	if lock != nil {
		if old, err = readAllLocked(ctx, lock, p); err != nil {
			return "", err
		}
	}
	if err := replaceLocked(lock, p, a.Content); err != nil {
		return "", err
	}
	// 替换完成 ⇒ **先释放锁再算 diff**：diff 是 O(N²) 量级（50k 行 ≈ 300 ms，
	// 改造前全程持锁 ⇒ 别的写者被挡这么久）。write 的 rename 已换 inode，
	// 这把锁此后也不再守护新路径 ⇒ 提前释放不改变语义。
	if locked {
		lock.Close()
		locked = false
	}
	// Created：lock==nil ⇔ 文件此前不存在（lockPath 对不存在的文件返回 nil 锁）。
	// 不能用 Removed==0 推断（纯插入内容同样是 +N -0）。
	d := finalizeDiff(computeFileDiff(a.Path, old, a.Content), a.Content)
	d.Created = lock == nil
	t.setDiff(ctx, d)
	added, removed := d.Added, d.Removed
	notice, err := t.notifyPostWrite(ctx, p)
	if err != nil {
		return "", err
	}
	return withNotice(fmt.Sprintf("written (+%d -%d)", added, removed), notice), nil
}

// withNotice 把宿主钩子给的提醒拼在成功结果之后（空提醒 = 结果逐字节不变）。
func withNotice(result, notice string) string {
	if strings.TrimSpace(notice) == "" {
		return result
	}
	return result + "\n\n" + strings.TrimSpace(notice)
}

// notifyPostWrite 触发宿主"写/改成功后"钩子；未注入钩子时是零成本空操作。
//
// 只在**写成功之后**调用：写失败时文件没变，补戳没有意义，且会把一次已经失败
// 的操作报成"记忆写入出错"，掩盖真正的错误。
func (t *writeFileTool) notifyPostWrite(ctx context.Context, absPath string) (string, error) {
	if t.FileTools == nil || t.FileTools.postWrite == nil {
		return "", nil
	}
	return t.FileTools.postWrite(ctx, absPath)
}

// ---------- edit_file ----------

type editFileTool struct {
	tools.BaseTool
	*FileTools
	// diff 镜像（**直调/旧宿主兼容**）—— 引擎读的是 ctx 槽，见 writeFileTool 的注释。
	dmu  sync.Mutex
	diff *core.FileDiff
}

// Diff 实现 tools.ToolDiffProvider：返回最近一次成功调用的变更 diff（直调/旧宿主用）。
func (t *editFileTool) Diff() *core.FileDiff {
	t.dmu.Lock()
	defer t.dmu.Unlock()
	return t.diff
}

// setDiff 同时写镜像与 ctx 槽（引擎读后者 ⇒ 并发下不会串味）。
func (t *editFileTool) setDiff(ctx context.Context, d *core.FileDiff) {
	t.dmu.Lock()
	t.diff = d
	t.dmu.Unlock()
	tools.SetFileDiff(ctx, d)
}

func newEditFileTool(f *FileTools) *editFileTool {
	return &editFileTool{
		BaseTool: tools.BaseTool{
			Name_:        "edit_file",
			Description_: "Replace exactly one occurrence in an existing text file. Read the file first and copy old_string exactly, including whitespace and line breaks. Use the shortest old_string that is still unique; uniqueness is more important than minimizing context. Keep the change minimal: new_string must contain only the necessary edits (ideally 1-3 lines) and must not rewrite unrelated content. If it matches zero or multiple times, no change is made. After an error never repeat the same arguments: read_file and refresh the edit. Use write_file only for intentional full-file replacement.",
			Params_: tools.Obj(map[string]any{
				"path":       tools.Str(pathParamDesc),
				"old_string": tools.Str("Original text to replace (must appear exactly once; ideally 1-3 lines, just enough to be unique)"),
				"new_string": tools.Str("Replacement text"),
			}, "path", "old_string", "new_string"),
			CanParallel_: false,
		},
		FileTools: f,
	}
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明 edit_file 的工具级时限。
func (t *editFileTool) ToolTimeout() time.Duration { return fileToolTimeout }

type editArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (t *editFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a editArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	if a.Path == "" || a.OldString == "" {
		return recoveryError("missing_required_argument", "correct_arguments", "path and old_string are required", "Provide both path and old_string; read the file first if needed.")
	}
	return nil
}

func (t *editFileTool) Call(ctx context.Context, _, arguments string) (string, error) {
	// ctx 已取消/已超时 ⇒ 立刻返回（改造前工具完全不看 ctx）。
	if err := ctx.Err(); err != nil {
		return "", toolCancelledError(ctx, "edit_file")
	}
	// 清空本次调用的 diff（ctx 槽 + 镜像）：上次调用的变更不得泄漏到本次。
	t.setDiff(ctx, nil)
	var a editArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolveWriteAt(ctx, a.Path)
	if err != nil {
		return "", err
	}
	if err := statPath(p, "edit_file"); err != nil {
		return "", err
	}
	// 读-改-写全程持锁：读的是锁定版本，替换不会覆盖并发写者的新内容
	lock, err := lockPath(p)
	if err != nil {
		return "", err
	}
	locked := lock != nil
	if locked {
		defer func() {
			if locked {
				lock.Close() // Close 即释放 flock
			}
		}()
	}
	s, err := readAllLocked(ctx, lock, p)
	if err != nil {
		return "", err
	}
	if n := strings.Count(s, a.OldString); n == 0 {
		return "", recoveryError("old_string_not_found", "read_file", fmt.Sprintf("old_string was not found in %s", a.Path), "Re-read the file and copy the exact current text (incl. whitespace); use the shortest unique snippet.")
	} else if n > 1 {
		return "", recoveryError("old_string_not_unique", "read_file", fmt.Sprintf("old_string appears %d times in %s", n, a.Path), "Add a bit of surrounding context to make it unique. Do not repeat the same arguments.")
	}
	replaced := strings.Replace(s, a.OldString, a.NewString, 1)
	if err := replaceLocked(lock, p, replaced); err != nil {
		return "", err
	}
	// 改写完成 ⇒ **先释放锁再算 diff**：diff 是 O(N²) 量级（改造前全程持锁 ⇒
	// 大文件时同一文件的其它写者被挡几百毫秒到几秒）。
	if locked {
		lock.Close()
		locked = false
	}
	// 旧全文 s 与 replaced 均在手，免费产 diff（信息源头在工具）。
	// edit_file 入口已 statPath 要求文件存在 → Created 恒 false（modified）。
	d := finalizeDiff(computeFileDiff(a.Path, s, replaced), replaced)
	t.setDiff(ctx, d)
	added, removed := d.Added, d.Removed
	notice, err := t.notifyPostWrite(ctx, p)
	if err != nil {
		return "", err
	}
	return withNotice(fmt.Sprintf("[edit_file: replaced 1 occurrence in %s (+%d -%d)]", a.Path, added, removed), notice), nil
}

// notifyPostWrite 触发宿主"写/改成功后"钩子；未注入钩子时是零成本空操作。
// 理由与 write_file 同名方法一致：只在写成功后调用，错误不吞，提醒不混进错误。
func (t *editFileTool) notifyPostWrite(ctx context.Context, absPath string) (string, error) {
	if t.FileTools == nil || t.FileTools.postWrite == nil {
		return "", nil
	}
	return t.FileTools.postWrite(ctx, absPath)
}

// readAllLocked 读取目标文件全文（持锁时直接复用锁定 fd，避免再次 open 竞态）。
//
// 带**上限**与**可中断**（2026-11-17，第 3 步）：
//   - 上限 16 MiB：改造前 io.ReadAll 无上限 ⇒ 100 MB 文件 ⇒ 峰值 4–6× 文件大小；
//   - 可中断：分块读，每块查一次 ctx.Err()（io.ReadAll 不可中断 —— 用户 abort
//     后它还会把整个文件读进内存）。
func readAllLocked(ctx context.Context, lock *os.File, p string) (string, error) {
	var r io.Reader
	var size int64 = -1
	if lock != nil {
		r = lock
		if st, err := lock.Stat(); err == nil {
			size = st.Size()
		}
	} else {
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
	}
	if size > maxFileToolReadBytes {
		return "", fileTooLargeError(p, size)
	}
	// 预分配：大小已知（Stat）⇒ 不靠 append 扩容（扩容会把已读内容整体拷贝一遍）。
	capacity := int(size)
	if capacity < 0 {
		capacity = 64 << 10
	}
	if capacity > maxFileToolReadBytes {
		capacity = maxFileToolReadBytes
	}
	buf := make([]byte, 0, capacity)
	tmp := make([]byte, 64<<10)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		n, err := r.Read(tmp)
		if n > 0 {
			if len(buf)+n > maxFileToolReadBytes {
				return "", fileTooLargeError(p, int64(len(buf)+n))
			}
			buf = append(buf, tmp[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return string(buf), nil
}

// fileTooLargeError 构造「文件太大，整文件读改写不做」的可恢复错误（指路 bash/python）。
func fileTooLargeError(p string, size int64) error {
	return recoveryError("file_too_large", "use_shell_or_split",
		fmt.Sprintf("%s is %s, larger than the %s limit for whole-file read-modify-write",
			p, humanSize(int(size)), humanSize(maxFileToolReadBytes)),
		"Edit big files with bash (sed/python) or split them into parts; do not repeat the same call.")
}
