package memory

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Auto Memory 的用户可见开关（技术方案 §8 待定项 2）。
//
// 官方提供三个开关，本包的对应关系：
//
//	autoMemoryEnabled              → EnvAutoMemoryEnabled（用户开关，默认开）
//	autoMemoryDirectory            → EnvAutoMemoryDirectory（改存储位置）
//	CLAUDE_CODE_PROJECT_DIR_NAME   → EnvProjectDirName（强制 <project> 名，跨仓库共用一份记忆）
//
// **默认开启。** 官方 auto memory 默认开；HAI 不做「默认关闭」（那会让"记忆到底开没开"
// 变成用户猜谜，也让空状态下的评测与线上行为不一致）。
//
// ## 关闭的语义（这一条是本文件唯一有争议的地方，写清楚以免被实现错）
//
// 关闭 = **完全不注入任何 Memory 相关系统提示词**，不是"注入一段告诉模型别用记忆"。
//
// 后者仍占 context，且模型未必遵守——恰恰是《技术方案》§2 判定的上一版失败根因
// （把行为规则写进提示词，结果 12%~62% 采纳率）。开关的价值全部来自"真的不注入"。
// 因此 Enabled 为 false 时 Step 3 直接返回空 Injection，**且不产生任何读盘副作用**
// （不 stat、不 open、不读索引），见 Bootstrap / BuildInjection 的开关前置判定。
//
// 关闭时 Step 4 的 `modified` 钩子**同样停**：否则变成"不读但写入"——用户关了记忆，
// 应用还在背后生成记忆文件。这比不实现更糟（官方没有这个行为）。
//
// 关闭时**不删除**已存在的记忆文件。官方语义是开关只控制加载
// （memory#storage-location：记忆文件"stay until you or Claude edits or deletes them"），
// 删数据必须是用户的显式操作。
//
// 优先级（后者覆盖前者）：Config 显式值 > 环境变量 > 默认开启。
// 设置面板走 Config（进程内持久化的用户偏好），环境变量供调试与 CI 使用。
const (
	// EnvAutoMemoryEnabled 对齐官方 autoMemoryEnabled。
	//
	// 接受 0/false/off/no（关）与 1/true/on/yes（开），大小写不敏感。
	// 显式设了它就**盖过**用户偏好：CI 要能在不开 UI 的情况下固定住空状态。
	EnvAutoMemoryEnabled = "autoMemoryEnabled"
	// EnvAutoMemoryDirectory 对齐官方 autoMemoryDirectory：改记忆存储位置。
	EnvAutoMemoryDirectory = "autoMemoryDirectory"
	// EnvProjectDirName 对齐官方 CLAUDE_CODE_PROJECT_DIR_NAME：强制 <project> 名，
	// 让多个仓库共用一份记忆（官方用途：给整条产品线记一份共享记忆）。
	EnvProjectDirName = "CLAUDE_CODE_PROJECT_DIR_NAME"
)

// Config 是 Bootstrap 的输入（用户可见开关 + 供测试注入的 I/O 出口）。
//
// 零值即可用：Enabled 零值 true 表示「跟随环境变量，再跟随默认开启」。
// 三个 io/fs 字段只在测试里注入，生产装配时留空即走真实实现。
type Config struct {
	// Enabled *bool 用户偏好（三态）：
	//
	//	nil    → 跟随 EnvAutoMemoryEnabled，未设则开启
	//	非 nil → 显式开/关；若同时设了 EnvAutoMemoryEnabled，环境变量覆盖它
	//
	// 用 *bool 而不是 bool，是因为「用户没设置过」与「用户设成关」必须能区分。
	Enabled *bool
	// Directory 覆盖记忆存储根目录（~/.go-code/memory）。nil/空 = 默认位置。
	//
	// 取的是**记忆根目录**（等价官方 autoMemoryDirectory 指向 memory/ 那一层），
	// 不是单个仓库的目录——单个仓库目录仍由 <root>/repos/<id> 派生。
	Directory string
	// ProjectDirName 覆盖 <project> 段（等价 CLAUDE_CODE_PROJECT_DIR_NAME）：
	// 直接当作仓库身份，落在 <root>/repos/<ProjectDirName>。
	ProjectDirName string

	// Resolve 身份解析器（nil = memory.New()，即真实 git）。
	Resolve *Resolver
	// Now 时间来源（nil = time.Now）。补 `modified` 用它，保证可测。
	Now func() time.Time
	// ReadDir 目录列举出口（nil = os.ReadDir）。与 ReadFile 同一理由抽出来：
	// List 要能跑在注入的根目录上，而不是去列用户真实的 ~/.go-code/memory
	//（此前 list.go 直接 os.ReadDir，测试里 ReadFile 指向 tmp 而列举列真实 home）。
	ReadDir func(string) ([]fs.DirEntry, error)
	// ReadFile / WriteFile / Rename / Remove 是 Step 3/4 全部 I/O 的出口。
	//
	// 单独抽出不是为了抽象洁癖，而是为了让「关闭开关不产生任何读盘副作用」成为
	// 一个**可断言的事实**：测试注入计数版 ReadFile，关闭开关后计数必须为 0，
	// 而不需要去 stat/strace 证明"没读"。
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte, os.FileMode) error
	Rename    func(string, string) error
	Remove    func(string) error
}

// enabled 是开关判定本体（Config.Enabled_ 的实现，避免方法名与字段混淆）。
//
// 三态 + 环境变量覆盖，见 Config.Enabled 的注释。
func (c Config) enabled() bool {
	if v, ok := lookupBool(EnvAutoMemoryEnabled); ok {
		return v
	}
	if c.Enabled != nil {
		return *c.Enabled
	}
	// 官方默认开。
	return true
}

// root 返回记忆根目录：Config.Directory > autoMemoryDirectory > ~/.go-code/memory。
//
// 环境变量在 Config.Directory 之后取值：Config 来自用户设置面板，环境变量只作
// 调试/CI 覆盖——与 autoMemoryEnabled 的方向一致（都是更外层覆盖更内层）。
func (c Config) root() (string, error) {
	dir := strings.TrimSpace(c.Directory)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv(EnvAutoMemoryDirectory))
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return filepath.Clean(abs), nil
	}
	return MemoryRoot()
}

// projectDirName 返回被强制指定的 <project> 名（空 = 按 git 仓库派生）。
//
// Config.ProjectDirName 优先于 CLAUDE_CODE_PROJECT_DIR_NAME，方向与开关一致
// （设置面板 > 环境变量 > 默认）。
func (c Config) projectDirName() string {
	if n := strings.TrimSpace(c.ProjectDirName); n != "" {
		return n
	}
	return strings.TrimSpace(os.Getenv(EnvProjectDirName))
}

// withDefaults 复制出填好默认出口的 Config，供 Bootstrap / BuildInjection 内部使用。
func (c Config) withDefaults() Config {
	out := c
	if out.Resolve == nil {
		out.Resolve = New()
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	if out.ReadDir == nil {
		out.ReadDir = os.ReadDir
	}
	if out.ReadFile == nil {
		out.ReadFile = os.ReadFile
	}
	if out.WriteFile == nil {
		out.WriteFile = os.WriteFile
	}
	if out.Rename == nil {
		out.Rename = os.Rename
	}
	if out.Remove == nil {
		out.Remove = os.Remove
	}
	return out
}

// lookupBool 解析布尔型环境变量；未设或值不认识时返回 ok=false（**不报错**）。
//
// 值不认识按「未设」处理而不是硬失败：官方这些变量是可选覆盖，一个拼错的值不该
// 让整个应用起不来。要强制 CI 走空状态请显式设成 0/false。
func lookupBool(key string) (bool, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true, true
	case "0", "false", "no", "off", "disable", "disabled":
		return false, true
	}
	return false, false
}

// Bool 是 *bool 构造助手，供设置面板把「开/关/跟随」映射成三态。
func Bool(v bool) *bool { return &v }
