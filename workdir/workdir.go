// Package workdir 承载**会话级**的当前工作目录（cwd）状态。
//
// # 为什么要有这个包（改造前的问题）
//
// 改造前"我现在在哪个目录"这件事散落在三处，各记各的：
//
//   - bash 工具：实例字段 curCwd（按 TaskId 分区，cd 成功后更新）；
//   - 文件工具（read_file / write_file / edit_file / grep / glob）：恒定锚在**工作区根**；
//   - run_python / codemode 脚本：恒定锚在**工作区根**。
//
// 于是模型 `cd sub` 之后，三个工具对"我现在在哪"给出三个答案：
//
//	bash      → 记住了 sub/（下一条命令从 sub/ 出发）
//	read_file → 仍在**工作区根**找 "a.txt" ⇒ file not found
//	run_python→ 脚本 cwd 仍是工作区根 ⇒ 产物落在根而不是 sub/
//
// 模型只能靠"每条命令都写 cd X && ..."硬扛，而一旦忘了 cd（或顺手写了相对路径）
// 就直接踩空。这是本包要消灭的那个"三份 cwd"。
//
// # 口径：一份状态，多个分区
//
// 一个会话（desktop/bridge 的 bridgeSession）持有**一个** State；
// State 内部按**分区**（partition）记录各自的 cwd：
//
//   - 主 agent 的分区键 = workdir.MainKey；
//   - 子 agent（spawn / explore）的分区键 = 它的 task id。
//
// 分区存在的唯一理由：主 agent 与子 agent **共用同一个工具引擎**
// （desktop/bridge 只装一套 tools.Engine），工具实例也是共享的 ⇒ cwd 若只有一份，
// 子 agent 的 `cd` 会改掉主 agent 的目录（两个 agent 互相踩，同一份工作树里
// 尤其难受：主 agent 以为自己在根，其实在子 agent cd 到的目录里）。
// 分区之后各走各的：主 agent 的 cwd 不受任何子 agent 影响。
//
// # 状态挂在会话上，不挂在引擎上
//
// 引擎（连同工具集）会随 **rebuildLoop** 重建（切模型 / 切人格 / 切 provider）。
// 状态若存在引擎里，切一次模型就丢 cwd（模型于是又开始每次 `cd X && ...`）。
// 所以 State 由**会话**持有（bridgeSession.wd），引擎只持有它的引用 ⇒ 跨重建存活。
//
// # 为什么本包不 import 仓库内任何包
//
// 工具层（tools/builtin）依赖它、宿主层（desktop/bridge）依赖它；它若反过来依赖
// 工具层或宿主层就会成环。分区键的**最终口径**（ctx key 优先、回退到
// events.ToolContext.TaskId）放在 tools/builtin.CwdKey —— 只有那一层才认识
// events.ToolContext。本包只认"一个字符串键"，不关心这个键是谁。
package workdir

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// MainKey 主分区键：主 agent（以及任何没有子 agent 身份的调用方）。
//
// 为什么是一个**显式常量**而不是空字符串：分区键要能进 ctx（WithKey/KeyFrom），
// 空字符串在 ctx 里等于"没设置"，与"设置成主分区"分不开。用一个显式值，
// KeyFrom 才能在"没设置"时稳定地回落到主分区。
const MainKey = "main"

// State 会话级 cwd 状态：按分区记录各自的当前工作目录。
//
// 并发安全：主 agent 与多个后台子 agent 会**并发**调用工具（共享同一个引擎），
// 而 cd 与"读当前目录"必须看到同一份状态 ⇒ 用 RWMutex 保护 map。
//
// 零值不可用：用 New 构造（root 需要归一化、map 需要初始化）。
type State struct {
	mu   sync.RWMutex
	root string            // 工作区根（绝对路径，Clean 过）：所有分区的初始 cwd
	cur  map[string]string // 分区键 → 当前目录（绝对路径）
}

// New 创建会话级 cwd 状态。root = 工作区根目录（绝对路径；相对路径按进程 cwd 展开）。
//
// root 不存在**不报错**：工作区可能还没建好（新建会话时目录刚 mkdir），
// 而"目录不存在"这件事应该由真正要用它的工具（bash / 文件工具）去报，
// 不该在装配期就把整条链路炸掉。
func New(root string) *State {
	abs, err := filepath.Abs(root)
	if err != nil {
		// Abs 失败只可能是 cwd 不可用（进程被 chroot 之类）⇒ 退化成 Clean，
		// 后续用到相对路径时再说。
		abs = filepath.Clean(root)
	}
	return &State{root: filepath.Clean(abs), cur: map[string]string{}}
}

// Root 返回工作区根目录（所有分区的初始 cwd）。
func (s *State) Root() string { return s.root }

// Current 返回该分区的当前工作目录。
//
// 没有记录过 cd ⇒ 返回工作区根（分区的初始状态）。
//
// 记录过的目录**已不存在**（模型把它 rm -rf 了）⇒ 回落到工作区根，而不是让后续
// 每条命令都以"目录不存在"失败：cwd 失效时"回到起点"是最可预期的降级，
// 而让 bash 以 ENOENT 失败会把模型卡在一个它无法自己察觉的状态里。
func (s *State) Current(key string) string {
	k := normKey(key)
	s.mu.RLock()
	dir, ok := s.cur[k]
	s.mu.RUnlock()
	if !ok || dir == "" {
		return s.root
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	// 目录没了 ⇒ 顺手把这条记录删掉（否则每次调用都要 stat 一次死路径）。
	s.mu.Lock()
	delete(s.cur, k)
	s.mu.Unlock()
	return s.root
}

// Set 记录该分区的当前目录（不做存在性校验 —— 调用方通常已经校验过，
// 例如 bash 的 cd 只在目标确实是目录时才更新记忆）。
//
// dir 为空 ⇒ 忽略（"回到工作区根"用 Reset，别用空串表达）。
func (s *State) Set(key, dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	s.mu.Lock()
	s.cur[normKey(key)] = filepath.Clean(abs)
	s.mu.Unlock()
}

// Reset 把该分区复位回工作区根（删掉它的 cd 记录）。
func (s *State) Reset(key string) {
	s.mu.Lock()
	delete(s.cur, normKey(key))
	s.mu.Unlock()
}

// Resolve 把一条路径锚到该分区的当前目录：
// 相对路径 → 相对当前目录展开；绝对路径 → 原样 Clean。
//
// **不做工作区边界检查**：边界是工具层（tools/builtin）的职责（那里才知道
// 哪个根是"工作区"、哪条策略允许越界）。本函数只管锚点。
func (s *State) Resolve(key, raw string) string {
	if raw == "" {
		return ""
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(s.Current(key), raw))
}

// ctxKey 是本包在 context 里存分区键的私有键类型。
//
// 为什么用私有结构体而不是字符串：context 的键必须是**包私有**的类型，
// 否则别的包可能用同一个字符串撞进来（Go 的官方建议，也是本仓库其它包的写法）。
type ctxKey struct{}

// WithKey 把分区键写进 ctx。
//
// 用途：**拼工具描述的那条路**。工具描述（schema）在装配期算好，那时还没有
// ToolContext（不知道调用方是谁），所以没法按 TaskId 分区；调用方在装配期
// 知道自己是谁（子 agent 的 task id），用 WithKey 标出来，描述侧就能算出
// 与运行期一致的工作目录。
//
// 空键 ⇒ 视为主分区（MainKey）。
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKey{}, normKey(key))
}

// KeyFrom 取出 ctx 里的分区键；没设置 ⇒ MainKey。
//
// 返回值**永远非空**（调用方不必判空）：主分区是合法且最常见的取值。
func KeyFrom(ctx context.Context) string {
	if ctx == nil {
		return MainKey
	}
	if k, ok := ctx.Value(ctxKey{}).(string); ok && k != "" {
		return k
	}
	return MainKey
}

// normKey 归一化分区键：空串 ⇒ 主分区。
func normKey(key string) string {
	if key == "" {
		return MainKey
	}
	return key
}
