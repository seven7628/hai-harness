// Package memory 是 HAI Phase A「Claude Code 兼容的仓库自动记忆」内核：
// 按 git 仓库身份把记忆落到 ~/.go-code/memory/repos/<id>/，索引（MEMORY.md）
// 有界，主题文件（topics/*.md）原子写。
//
// 与 CC auto memory 的对齐点（docs/ROADMAP.md §4「记忆写回」调研结论）：
//   - 记忆**不写 CLAUDE.md / AGENTS.md**，只写 harness 自己的 memory 目录；
//   - 索引文件有界（200 行 / 25KB，见 IndexMaxLines / IndexMaxBytes），
//     主题文件承载细节——索引是"查询地图"不是全量正文；
//   - 索引与主题都是**纯 Markdown + 有限结构**，人可读可手改；
//     渲染必须字节稳定（render(parse(x)) == x）以便人工编辑不被下轮写回冲掉。
//
// 边界（Phase A 明确不做，见任务约定）：
//   - 不实现 Candidate / Validator / Replay / Outcome / Lifecycle / Selector / Events；
//   - 不依赖 agents / session / bridge / skills —— 本包只依赖标准库，
//     仓库身份从 git 现场解析（见 identity.go），上下文由调用方以参数传入；
//   - 不做并发/多进程锁（原子写只保证单次写不撕裂，不保证跨进程读-改-写串行化）。
package memory

import (
	"context"
	"time"
)

// 目录布局常量（~/.go-code/memory/repos/<id>/）。文件名与 CC auto memory 对齐，
// 便于用户手工查看/编辑。
const (
	// IndexFileName 索引文件名（仓库记忆根下）。
	IndexFileName = "MEMORY.md"
	// TopicsDirName 主题文件目录名（仓库记忆根下）。
	TopicsDirName = "topics"
	// IndexMaxLines 索引读取的最大行数（CC 限额对齐）。
	IndexMaxLines = 200
	// IndexMaxBytes 索引读取的最大字节数（CC 限额对齐：25KB）。
	IndexMaxBytes = 25 * 1024
)

// Scope 主题的作用域标签（写进 frontmatter，用于后续 Selector 阶段过滤）。
// Phase A 只做**基础校验**（取值必须已知），不做任何选择/召回逻辑。
type Scope string

const (
	// ScopeRepo 仓库内通用知识（默认）。
	ScopeRepo Scope = "repo"
	// ScopeFile 绑定到仓库内某个文件路径。
	ScopeFile Scope = "file"
	// ScopeTask 一次性任务上下文。
	ScopeTask Scope = "task"
)

// validScopes 已知作用域集合；validateScope 是它的唯一出口。
var validScopes = map[Scope]bool{ScopeRepo: true, ScopeFile: true, ScopeTask: true}

var validKinds = map[string]bool{"project": true, "experience": true, "decision": true, "reference": true}

// Topic 一个主题记忆文件（topics/<name>.md 的内存表示）。
//
// 正文字段（Body）是人写的自由文本，解析/渲染不做任何语义改写；
// 头部字段（frontmatter）由 renderIndex/renderTopic 保证字节稳定。
type Topic struct {
	Name    string   // 主题短名（= 文件名去扩展名，规范化后）
	Title   string   // 一行标题
	Summary string   // frontmatter description / 索引摘要
	Kind    string   // 内容类别（project/experience/decision/reference）
	Scope   Scope    // 作用域标签；Phase A 固定为 project
	Tags    []string // 自由标签
	Body    string   // 主题正文
	Created time.Time
	Updated time.Time
}

// IndexEntry 索引里的一行（对应一个 Topic 的头部摘要）。
type IndexEntry struct {
	Name    string
	Title   string
	Summary string
	Scope   Scope
	Tags    []string
	Updated time.Time
}

// Ref 主题引用（"topics/foo" / "foo" / "topic:foo" 三种写法都归一到它）。
type Ref struct {
	Name string // 规范化后的主题名
	// IsIndex 为真表示引用的是索引本身（View("" / "index" / "MEMORY.md"）。
	IsIndex bool
}

// Index 仓库记忆索引（MEMORY.md 的有界读取结果）。
type Index struct {
	RepoID     string       // 仓库身份 id（= 目录名）
	RemoteURL  string       // 规范化后的远端 URL（无远端为空）
	RepoRoot   string       // git toplevel（无 git 时为工作目录）
	Generated  time.Time    // 索引里的生成时间戳
	Entries    []IndexEntry // 解析出的条目（**文件内出现顺序**，不重排）
	Context    string       // `## Context` 分节里的自由文本（人写区）
	Truncated  bool         // 是否被 200 行 / 25KB 限额截断
	Lines      int          // 实际参与解析的行数
	Bytes      int          // 实际参与解析的字节数
	Raw        string       // 参与解析的原始文本（有界）
	Bootstrap  bool         // 由 BootstrapIndex 新建（非从磁盘读到）
	Path       string       // 索引文件绝对路径
	TopicCount int          // 索引条目数
}

// Store 记忆存储（Phase A 只有一个实现：FileStore，目录布局见文件头注释）。
//
// 接口保持最小：读/写索引 + 读/写/列主题。所有实现**必须**满足：
//   - 目录 0700、文件 0600；
//   - 写为原子写（临时文件 + rename），读者不会看到半截内容；
//   - 索引读取受 IndexMaxLines / IndexMaxBytes 限制并如实标记 Truncated；
//   - 主题名先过 validateTopicName（路径边界），再拼接路径。
type Store interface {
	// RepoID 仓库身份 id（= 存储目录名）。
	RepoID() string
	// Dir 该仓库的存储根目录（绝对路径）。
	Dir() string
	// ReadIndex 读取有界索引；不存在时返回 os.ErrNotExist 包裹的错误。
	ReadIndex() (*Index, error)
	// WriteIndex 原子写索引（内容由 format.go 渲染）。
	WriteIndex(idx *Index) error
	// ReadTopic 读一个主题；不存在时返回 os.ErrNotExist 包裹的错误。
	ReadTopic(name string) (*Topic, error)
	// WriteTopic 原子写一个主题（含 topics/ 目录按需创建）。
	WriteTopic(t *Topic) error
	// ListTopics 列出主题名（字典序，跳过非 .md 与不可读项）。
	ListTopics() ([]string, error)
}

// IdentityResolver 仓库身份解析器（可注入：测试用假实现，产品可换实现）。
type IdentityResolver interface {
	// Resolve 从工作目录解析仓库身份；无 git 仓库时返回包裹 ErrNoGit 的错误。
	Resolve(ctx context.Context, workdir string) (Identity, error)
}

// MemoryService 记忆服务：解析身份 → 打开仓库记忆存储 → 引导索引 → 读/写主题。
//
// 零值不可用，必须经 NewService 构造。Service 本身无状态（身份解析结果不缓存），
// 因此可跨会话复用；Bound 是一次 Open 绑定的句柄。
type MemoryService struct {
	cfg Config
	// root 记忆根目录（Open 后生效；未 Open 时为 cfg.Root）。
	root string
	// store 当前绑定的存储（未 Open 为 nil）。
	store Store
	// ident 当前绑定的身份（未 Open 为 nil）。
	ident *Identity
}

// Bound 一次 Open 的结果：仓库身份 + 存储。
type Bound struct {
	Identity Identity
	Store    Store
}

// Config 服务配置。
type Config struct {
	// Root 记忆根目录（空 = DefaultRoot()，即 ~/.go-code/memory）。
	Root string
	// Resolver 身份解析器（空 = 默认 GitIdentityResolver）。
	Resolver IdentityResolver
	// FallbackID 无 git 仓库时的 fallback 目录名（空 = 直接返回 ErrNoGit）。
	//
	// 边界：fallback id 与真实 git id 落在**同一命名空间**（root/repos/<id>），
	// 因此只适合"确定单仓库"的场景；多仓库混用会共用一份记忆。
	FallbackID string
	// Now 时钟（空 = time.Now）。测试注入固定时钟以获得稳定字节输出。
	Now func() time.Time
	// Timeout 单次 git 调用的超时（空 = DefaultGitTimeout）。
	Timeout time.Duration
}

// DefaultRoot 默认记忆根目录（~/.go-code/memory）。
// 取不到 home 时返回空串（由调用点决定报错，不静默落到别处）。
func DefaultRoot() string {
	home, err := userHomeDir()
	if err != nil {
		return ""
	}
	return joinPath(home, ".go-code", "memory")
}
