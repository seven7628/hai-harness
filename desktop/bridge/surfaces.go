// surfaces.go —— `~/.go-code/surfaces/` 持久层（PR-13 A：安装 / 卸载 / 回滚点 / 审计）。
//
// 布局（01-design §5.4）：
//
//	surfaces/index.json       {"version":1,"items":[{id,version,hash,source,enabled,installedAt,disabledReason?}]}
//	surfaces/index.json.bak   每次变更**前**的内容（只留 1 份 = 手动回滚点）
//	surfaces/<id>/surface.json + panel.js (+ icon.svg)
//	surfaces/.trash/<id>-<ts>/  卸载去向（不直接删：给手滑一次后悔的机会）
//	surfaces/audit.jsonl      一行一条 {id, action, ts, ok, reason?}
//
// 六条不变式（每条都有 surfaces_test.go 的钉子）：
//
//  1. **失败一个字节都不改 index**：校验在加锁之前做完，拷贝落在 `.install-<rand>/`
//     暂存目录，校验通过才 rename 成 `<id>`。半装状态比装不上更糟 —— 用户会以为装上了。
//  2. **index 写入必须原子**（internal/atomicfile：temp + fsync + rename），且读-改-写
//     全程持 flock —— 两个会话同时安装不许丢条。
//  3. **`.bak` 是变更前的内容**（逐字节）。写成变更后 = 回滚点指向坏状态，比没有更糟。
//  4. **卸载只挪不删**：目录进 `.trash/<id>-<ts>`，index 才删条目。
//  5. **index 损坏按空列表启动**，原文件原样改名为 `index.json.corrupt-<ts>` 备份 ——
//     不静默覆盖（用户的插件清单可能是唯一副本）。
//  6. **audit 写失败不影响主流程**（记日志即可）：审计是观测面，不是事务的一部分。
//
// 本轮**不加载、不执行** panel.js（渲染层 loader 是 PR-14）：`applied` 的语义是
// 「已写进 index、下次启动可见」，不是「面板已经在屏幕上」。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/internal/atomicfile"
)

const (
	surfaceIndexVersion = 1
	surfaceIndexName    = "index.json"
	surfaceBakName      = "index.json.bak"
	surfaceAuditName    = "audit.jsonl"
	surfaceTrashDir     = ".trash"
	surfaceStagingPref  = ".install-"
	surfaceDefaultEntry = "panel.js"

	// maxSurfaceEntryBytes 单个 ESM 入口的上限（256 KiB）：无构建步骤的单文件面板，
	// 再大只可能是把依赖打包进来了 —— 那该走 PR-14 之后的构建路径，不是这一轮的语义。
	maxSurfaceEntryBytes = 256 << 10
	// maxSurfaceDirBytes 整目录上限（1 MiB）：manifest + panel.js + icon 的合理量级。
	maxSurfaceDirBytes = 1 << 20

	// maxSurfaceEntries manifest.entries[] 的条数上限（D-2 / R16）。
	// 20 是「一个插件能一眼列完入口」的量级：再多就不是菜单，是目录页了。
	maxSurfaceEntries = 20
	// maxSurfaceEntryTitleChars 单个入口标题的**字符**（rune）上限，不是字节：
	// 一个 10 字的中文标题是 30 字节，用字节数判会把正常标题判死。
	maxSurfaceEntryTitleChars = 64

	// maxSurfaceTools manifest.tools[] 的条数上限（09-agent-bridge §5.3 / §7.3）。
	// 16 = 「一个插件能一眼列完的工具」的量级；再多就不是一面板插件，是一套工具箱。
	maxSurfaceTools = 16
	// maxSurfaceToolDescriptionChars 单个工具描述的**字符**（rune）上限，与 title 同口径。
	// 这句话是审批卡片上唯一的人读说明（§6.4「卡片显示插件名 · 工具名」），
	// 太长会被权限卡片截断成「看不出在干什么」。
	maxSurfaceToolDescriptionChars = 300
	// maxSurfaceToolInputBytes 单个工具 input schema 的上限（§8.3：schema ≤ 16 KiB）。
	maxSurfaceToolInputBytes = 16 << 10
	// 工具超时边界（§6.4「默认 10s（manifest ≤60s）」）。
	surfaceToolDefaultTimeoutMS = 10000
	surfaceToolMinTimeoutMS     = 100
	surfaceToolMaxTimeoutMS     = 60000

	surfaceDirMode  = 0o700 // 目录 0700
	surfaceFileMode = 0o600 // 文件 0600（index / manifest / panel.js / audit 一律）
)

// surfaceStatus* 回执状态（01-design §5.2 / 03-code-plan §3.4 的封闭集合）。
//
// `rejected` = 校验失败（带可读 reason，模型据此改），`error` = 磁盘/IO 失败
// （重试可能有用）。两者分开是为了让调用方知道「改输入」还是「重试」。
const (
	surfaceApplied          = "applied"
	surfaceAlreadyInstalled = "already_installed"
	surfaceCleaned          = "cleaned"
	surfaceRejected         = "rejected"
	surfaceStatusError      = "error"
)

// surfaceIDPattern 插件 id 的唯一判据（manifest 校验、目录名、卸载参数共用）。
var surfaceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)

// surfaceToolNamePattern 工具名判据（§5.3）。
//
// 只允许小写字母开头 + 小写字母/数字/下划线：模型侧名是 `plugin__<id>__<name>`，
// 工具名里再出现大写或连字符，反解（「这条工具属于谁」）就得靠猜。
var surfaceToolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// surfaceToolCallEvent 工具调用下发的事件名（§5.2 / §6.6）。
//
// tools[] 非空时 manifest.data 必须声明它 —— 不声明，装上去的工具每次调用都只能
// 等超时（插件收不到调用）。回执那条（tool.result）复用 surface_agent.go 的
// surfaceAgentToolResult：同一个协议名不该有两个常量各写一遍字面量。
const surfaceToolCallEvent = "tool.call"

// surfaceEntry index.json 里的一条（字段名即磁盘协议，PR-14 的插件页直接读它）。
type surfaceEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Hash    string `json:"hash"`   // sha256(入口文件) 的十六进制
	Source  string `json:"source"` // 安装来源目录的绝对路径（追溯「从哪装的」）
	Enabled bool   `json:"enabled"`
	// InstalledAt 这一版装上的时刻（RFC3339，UTC）。升级会刷新 —— 它描述的是
	// 「当前这版何时落盘」，不是「这个 id 第一次出现」。
	InstalledAt    string `json:"installedAt"`
	DisabledReason string `json:"disabledReason,omitempty"`
}

// surfaceIndex 磁盘协议（version 不匹配一律按损坏处理，不做版本迁移：V0.1 只有 1）。
type surfaceIndex struct {
	Version int            `json:"version"`
	Items   []surfaceEntry `json:"items"`
}

// surfaceManifest 只解出本轮真正要用的字段。
//
// 刻意**不**给 Name/Kind/Group 之类的字段加约束：它们属于渲染层（PR-14），这一轮
// 校验多了只会把「装得上」变成「装不上」，而没有任何消费者受益。
type surfaceManifest struct {
	ID      string            `json:"id"`
	Name    map[string]string `json:"name"`
	Version string            `json:"version"`
	API     int               `json:"api"`
	Entry   string            `json:"entry"`
	Icon    string            `json:"icon"`
	// Kind 渲染层的 provider kind（PR-14）：必须等于 `plugin:<id>`，否则 loader 拒绝加载。
	// bridge 不消费它（注册表在渲染层），解出来是为了让「索引/诊断/内容守卫」读到同一份
	// 声明 —— 而不是各自再解析一次 surface.json。
	Kind string `json:"kind"`
	// Commands 插件**声明**要用到的宿主命令（PR-14）。它是白名单的一半：
	// 真正放行 = 在这里 **且** 在宿主只读命令表里（见 surfaces_command.go）。
	// 刻意只解出名字，不解析参数形态 —— 参数由宿主命令自己的 handler 校验。
	Commands []string `json:"commands"`
	// Data 插件**声明**要订阅的 `ui` 通道 op（PR-14，渲染层 host.events.on 用它做闸门）。
	// bridge 不消费它（ui 通道没有回执），读它只是为了让「同一份 surface.json 的
	// 声明」在一处可查：渲染层从索引 IPC 读，Go 从这份结构读。
	Data []string `json:"data"`
	// Entries 插件声明的**多个入口**（D-2 / R16）：每个 entry 是同一 kind 的一个标签实例
	//（渲染层用 `params.entry = <id>` 区分页面）。bridge 只校验形状与路径纪律 ——
	// 入口渲染在渲染层，这一侧不存 index（index.json 的 schema 不动：entries 是 manifest 字段）。
	Entries []surfaceEntryDecl `json:"entries"`
	// Tools 插件声明的**模型可见工具**（09-agent-bridge §5.3 / §7.3，P2）。
	// 声明即注册：模型侧名 `plugin__<id>__<name>`，调用经 tool.call 下发插件。
	// 与 entries 同一条纪律：形状/重复/超限在**安装期**判死（写盘前），
	// 运行期只读这份声明（注册点见 plugin_tools.go 的 buildPluginEngine）。
	Tools []surfaceToolDecl `json:"tools"`
}

// surfaceToolDecl manifest.tools[] 的一项（§5.3）。
//
// 字段名即协议（渲染层安装页、审批卡片、模型侧 schema 都读同一份）：
//
//	name/description/input  模型看到的东西
//	read_only               只读 = 免审批（§6.4「read_only:false 走现有工具审批」）
//	requires_panel          调用需要插件面板在场（P2 只透传给插件，不做托管）
//	timeout_ms              本次调用的等待上限（0 = 默认 10s，见 timeout）
type surfaceToolDecl struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Input 工具参数的 JSON Schema（必须是 object）。原样透传给模型 —— 这一层
	// 不加任何关键字：安装期只判「能不能被 provider 当参数描述吃掉」。
	Input         json.RawMessage `json:"input"`
	ReadOnly      bool            `json:"read_only"`
	RequiresPanel bool            `json:"requires_panel"`
	TimeoutMS     int             `json:"timeout_ms"`
}

// timeout 生效时限：0 = 默认 10s（§6.4）。安装期已把非法的非零值判死，
// 这里只剩「0 → 默认」一条规则，运行期不再二次判断。
func (d surfaceToolDecl) timeout() time.Duration {
	ms := d.TimeoutMS
	if ms <= 0 {
		ms = surfaceToolDefaultTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond
}

// surfaceEntryDecl manifest.entries[] 的一项（D-2 的形状：id/title/icon?/order?）。
//
// 刻意**不**给 entry 加 kind / capabilities：一个插件仍只注册一个 kind（D-2 的第一条规则），
// 每个 entry 只是它的一面；多给一层能力就等于让一个 manifest 声明出两个插件。
type surfaceEntryDecl struct {
	ID string `json:"id"`
	// Title 两种写法都合法（R16b）：`"总览"`（老）或 `{"zh":"总览","en":"Overview"}`。
	// 单字符串**必须继续可用** —— 改之前先读 07-plugin-entries.md 的「默认入口标题」裁决。
	Title surfaceEntryTitle `json:"title"`
	Icon  string            `json:"icon"`
	Order int               `json:"order"`
}

// surfaceEntryTitle entries[].title 的两种写法（R16b）。
//
// 为什么是结构体而不是 `any` / `json.RawMessage`：
//   - 校验要能分别判「空」「每个语言超 64 字符」「对象里一个非空都没有」——三种失败给三种
//     reason（模型据此改输入），`any` 会把它们糊成一个「形状不对」；
//   - 三个字符串字段 = **可比较**，既有 `TestSurfaceEntriesRoundTrip` 的 `got != want` 语义
//     （「原样拿回」）继续成立，不必为了类型改造把断言降级成「字段差不多」；
//   - MarshalJSON 让 `Manifest()` 读出去的形状与磁盘上那份逐字同类（单字符串仍是字符串），
//     渲染层与诊断看到的是声明本身，不是我们的中间表示。
type surfaceEntryTitle struct {
	// Text 单字符串写法的原文（对象写法恒为空）。
	Text string
	// Zh / En 对象写法的两种语言（未声明的语言为空串）。
	Zh string
	En string
}

// IsZero 两种写法都没给（缺字段 / null）：校验要判它为空。
func (t surfaceEntryTitle) IsZero() bool { return t.Text == "" && t.Zh == "" && t.En == "" }

// Primary 回执/日志用的一句人读标题：单字符串 → 它自己；对象 → zh → en。
func (t surfaceEntryTitle) Primary() string {
	if t.Text != "" {
		return t.Text
	}
	if t.Zh != "" {
		return t.Zh
	}
	return t.En
}

// UnmarshalJSON 接受两种形状，其它一律报错（非法形状 = 安装 rejected，D-3）。
//
// 对象里**只认 zh/en**：宿主渲染层按当前语言二选一，多出来的键存了也没人读 —— 但一个
// 只写了 `{"ja":"..."}` 的 title 会被判成「一个非空都没有」，而不是静默变成空标题。
func (t *surfaceEntryTitle) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*t = surfaceEntryTitle{}
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*t = surfaceEntryTitle{Text: s}
		return nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return err
		}
		out := surfaceEntryTitle{}
		for _, k := range []string{"zh", "en"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				// 语言值必须是字符串：数字/null 是「形状不对」，不是「空标题」。
				return fmt.Errorf("title.%s 必须是字符串", k)
			}
			if k == "zh" {
				out.Zh = s
			} else {
				out.En = s
			}
		}
		*t = out
		return nil
	default:
		return errors.New(`title 必须是字符串或 {"zh":"…","en":"…"} 对象`)
	}
}

// MarshalJSON 按声明时的形状写回（对象写法里空语言省略，不写 `""`）。
func (t surfaceEntryTitle) MarshalJSON() ([]byte, error) {
	if t.Zh == "" && t.En == "" {
		return json.Marshal(t.Text)
	}
	m := map[string]string{}
	if t.Zh != "" {
		m["zh"] = t.Zh
	}
	if t.En != "" {
		m["en"] = t.En
	}
	return json.Marshal(m)
}

// validate 标题的长度与「至少一种写法有内容」（R16b）。
//
// 长度按 **rune** 判（与 version 的既有校验同口径）：一个 10 字的中文标题是 30 字节，
// 按字节判会把正常标题判死。对象写法逐语言各判一次 —— 只判最长的那个也行，但
// reason 里要能指出是哪一种语言超了，模型改起来才不用猜。
func (t surfaceEntryTitle) validate(limit int) error {
	if t.Text != "" {
		if n := len([]rune(strings.TrimSpace(t.Text))); n > limit {
			return fmt.Errorf("title 过长（%d 字符，上限 %d）", n, limit)
		}
		return nil
	}
	if t.Zh == "" && t.En == "" {
		// 缺字段 / null / `{}` / 只写了别的语言：都属于「没有可渲染的标题」。
		return errors.New(`title 不能为空（字符串，或 {"zh":"…","en":"…"} 里至少一个非空）`)
	}
	for _, lang := range []struct{ key, val string }{{"zh", t.Zh}, {"en", t.En}} {
		if lang.val == "" {
			continue
		}
		if n := len([]rune(strings.TrimSpace(lang.val))); n > limit {
			return fmt.Errorf("title.%s 过长（%d 字符，上限 %d）", lang.key, n, limit)
		}
	}
	return nil
}

// surfaceReceipt 安装 / 卸载的回执（工具层直接序列化给模型与脚本）。
type surfaceReceipt struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	Hash    string `json:"hash,omitempty"`
	// Entries 这次安装的入口条数（R16b）：0 = manifest 没声明 → 宿主合成一个默认入口。
	// 指针而不是 int：`entries: 0` 与「没有这个字段」是两件事，前者是「一个入口都没声明，
	// 但有一个默认入口」，后者属于别的回执（uninstall / rejected）。
	Entries *int `json:"entries,omitempty"`
	// EntryHint 入口在界面上的位置（D-2 的「安装回执写明入口在哪」那一半）。
	// 固定一句话而不是 i18n key：回执是给**模型**读的（模型据此告诉用户去哪点），
	// 它不在渲染层的语言订阅里，也不该为了一句话把 i18n 表引进 bridge。
	EntryHint string `json:"entryHint,omitempty"`
}

// surfaceEntryHint 安装成功回执里的入口提示（与 D-2 的第 1 处渲染点逐字对应）。
const surfaceEntryHint = "right panel ＋ → Panel plugins"

// withEntries 给回执补上入口条数 + 位置提示（只用于「装上了」的两种状态）。
func (r surfaceReceipt) withEntries(n int) surfaceReceipt {
	r.Entries = &n
	r.EntryHint = surfaceEntryHint
	return r
}

// surfaceListResult 列表回执。与 surfaceReceipt 分开的**唯一**原因：`items` 必须
// 恒出现（空态是 `[]`，不是字段消失 —— 脚本的 `s.items.filter(...)` 会因为
// undefined 直接抛 TypeError，而「空态」恰恰是最常被走到的分支）。
type surfaceListResult struct {
	Status string         `json:"status"`
	Reason string         `json:"reason,omitempty"`
	Items  []surfaceEntry `json:"items"`
}

// surfaceStore 持久层的唯一入口。
//
// 零值可用（root == "" → 调用时解析 appDataDir()）：manager 由多处字面量构造
// （含测试），要求显式初始化必然漏一处，而漏的那处表现为「装到别的目录去了」
// —— 是最难查的假绿形态（00-conventions §4.7 / F53）。与 uiState 同一条纪律。
type surfaceStore struct {
	// mu 进程内串行化：读-改-写 index 的临界区。跨进程由 atomicfile 的 flock 兜。
	// 两把锁都要：mutex 防同进程两个 goroutine 同时决定「装哪一个」，flock 防
	// 两个 bridge 实例（多窗口 / hub）互相覆盖。
	mu   sync.Mutex
	root string
}

func newSurfaceStore(root string) *surfaceStore { return &surfaceStore{root: root} }

// Dir store 根目录。root 为空时按生产口径解析（~/.go-code/surfaces）。
func (s *surfaceStore) Dir() string {
	if s != nil && s.root != "" {
		return s.root
	}
	return filepath.Join(appDataDir(), "surfaces")
}

// ---- 安装 ----

// Install 安装（或升级）target 目录里的插件，返回回执（**永不返回 error**：
// 失败是一种可编程状态，由调用方按 status 分支）。
func (s *surfaceStore) Install(target string, enable bool) surfaceReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.install(target, enable)
}

func (s *surfaceStore) install(target string, enable bool) surfaceReceipt {
	// ① 校验（不碰 index、不建目录）：任何一条不过都直接 rejected。
	src, verr := validateSurfaceSource(target)
	mf, hash := src.manifest, src.hash
	if verr != nil {
		s.audit(mf.ID, "install", false, verr.Error())
		return surfaceReceipt{Status: surfaceRejected, Reason: verr.Error(), ID: mf.ID}
	}

	root := s.Dir()
	if err := s.ensureStore(); err != nil {
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "创建 surfaces 目录失败：" + err.Error(), ID: mf.ID}
	}

	// ② 加锁并读出当前 index（损坏则在此隔离为 .corrupt-<ts>）。
	idx, prev, lock, lerr := s.lockIndex()
	if lerr != nil {
		s.audit(mf.ID, "install", false, lerr.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "读 index 失败：" + lerr.Error(), ID: mf.ID}
	}
	defer closeLock(lock)

	// ③ 幂等：同 id 同 hash = 什么都不做（脚本重试安全）。
	pos := idx.pos(mf.ID)
	if pos >= 0 && idx.Items[pos].Hash == hash {
		s.audit(mf.ID, "install", true, "already installed (same sha256)")
		it := idx.Items[pos]
		// 幂等路径也带入口条数：脚本重跑一次不该让「有几个入口」这个答案消失。
		return surfaceReceipt{Status: surfaceAlreadyInstalled, ID: it.ID, Version: it.Version, Hash: it.Hash}.
			withEntries(len(mf.Entries))
	}

	// ④ 拷到暂存目录：只有全部校验通过才会变成 <id>。
	stage, err := os.MkdirTemp(root, surfaceStagingPref)
	if err != nil {
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "建暂存目录失败：" + err.Error(), ID: mf.ID}
	}
	defer os.RemoveAll(stage) // 成功后已被 rename 走，这里是无害清理

	if err := copySurfaceTree(target, stage); err != nil {
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceRejected, Reason: err.Error(), ID: mf.ID}
	}
	// ⑤ 复制后复算 hash：拷贝中途源文件被改 / 写了一半，到这里为止都还能退。
	staged := filepath.Join(stage, src.entryRel)
	gotHash, err := sha256File(staged)
	if err != nil {
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "校验拷贝结果失败：" + err.Error(), ID: mf.ID}
	}
	if gotHash != hash {
		reason := fmt.Sprintf("拷贝后 sha256 不符（源 %s / 拷贝 %s）：源目录在安装过程中被改过？", shortHash(hash), shortHash(gotHash))
		s.audit(mf.ID, "install", false, reason)
		return surfaceReceipt{Status: surfaceRejected, Reason: reason, ID: mf.ID}
	}

	// ⑥ 就位：旧目录（升级/残留）先挪进 .trash，再把暂存目录改名成 <id>。
	//    目录就位后 index 才写；index 写失败则把目录回滚 —— 两边必须同进同退。
	prevTrash, err := s.displaceOld(root, mf.ID)
	if err != nil {
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "清旧目录失败：" + err.Error(), ID: mf.ID}
	}
	final := filepath.Join(root, mf.ID)
	if err := os.Rename(stage, final); err != nil {
		restoreOld(prevTrash, final)
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "落盘失败：" + err.Error(), ID: mf.ID}
	}

	// ⑦ 写 index（先 .bak）：失败则回滚目录，磁盘与本文件之外的世界保持一致。
	entry := surfaceEntry{
		ID:          mf.ID,
		Version:     strings.TrimSpace(mf.Version),
		Hash:        hash,
		Source:      filepath.Clean(target),
		Enabled:     enable,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if pos >= 0 {
		idx.Items[pos] = entry // 升级：位置不变（index 有序 = 用户看到的顺序）
	} else {
		idx.Items = append(idx.Items, entry)
	}
	if err := s.writeIndex(lock, idx, prev); err != nil {
		_ = os.RemoveAll(final)
		restoreOld(prevTrash, final)
		s.audit(mf.ID, "install", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "写 index 失败：" + err.Error(), ID: mf.ID}
	}

	s.audit(mf.ID, "install", true, "")
	return surfaceReceipt{Status: surfaceApplied, ID: entry.ID, Version: entry.Version, Hash: entry.Hash}.
		withEntries(len(mf.Entries))
}

// ---- 卸载 ----

// Remove 卸载 id：目录进 .trash、index 去条目。幂等（再删一次 = cleaned）。
func (s *surfaceStore) Remove(id string) surfaceReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remove(id)
}

func (s *surfaceStore) remove(id string) surfaceReceipt {
	id = strings.TrimSpace(id)
	if !surfaceIDPattern.MatchString(id) {
		// 非法 id 不写盘、不打日志到 audit（没有可信的 id 可记）。
		return surfaceReceipt{Status: surfaceRejected, Reason: fmt.Sprintf("id 不合法：%q（需匹配 ^[a-z][a-z0-9-]{1,40}$）", id), ID: id}
	}
	root := s.Dir()
	if err := s.ensureStore(); err != nil {
		s.audit(id, "remove", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "创建 surfaces 目录失败：" + err.Error(), ID: id}
	}
	idx, prev, lock, lerr := s.lockIndex()
	if lerr != nil {
		s.audit(id, "remove", false, lerr.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "读 index 失败：" + lerr.Error(), ID: id}
	}
	defer closeLock(lock)

	pos := idx.pos(id)
	dir := filepath.Join(root, id)
	_, statErr := os.Lstat(dir)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		// 权限之类：别把「看不见」当成「不存在」，否则会写出一份和磁盘不符的 index。
		s.audit(id, "remove", false, statErr.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "查看目录失败：" + statErr.Error(), ID: id}
	}
	dirExists := statErr == nil

	if pos < 0 && !dirExists {
		// 记录和目录都没有 = 已经是干净状态。不是错误（重试安全）。
		s.audit(id, "remove", true, "already clean")
		return surfaceReceipt{Status: surfaceCleaned, Reason: "index 里没有该 id，目录也不存在（已经干净）", ID: id}
	}

	// 先挪目录再写 index：目录挪失败就什么都不改（index 是唯一的真值）。
	trash := ""
	if dirExists {
		trashDir := filepath.Join(root, surfaceTrashDir)
		if err := os.MkdirAll(trashDir, surfaceDirMode); err != nil {
			s.audit(id, "remove", false, err.Error())
			return surfaceReceipt{Status: surfaceStatusError, Reason: "建 .trash 失败：" + err.Error(), ID: id}
		}
		dst := filepath.Join(trashDir, id+"-"+surfaceStamp())
		if err := os.Rename(dir, dst); err != nil {
			s.audit(id, "remove", false, err.Error())
			return surfaceReceipt{Status: surfaceStatusError, Reason: "移到 .trash 失败：" + err.Error(), ID: id}
		}
		trash = dst
	}

	if pos < 0 {
		// 只有残留目录、index 没记录：清掉目录（已进 .trash）即可。
		s.audit(id, "remove", true, "orphan dir moved to .trash")
		return surfaceReceipt{Status: surfaceCleaned, Reason: "index 里没有该 id，残留目录已移入 .trash", ID: id}
	}

	idx.Items = append(idx.Items[:pos], idx.Items[pos+1:]...)
	if err := s.writeIndex(lock, idx, prev); err != nil {
		restoreOld(trash, dir) // index 没写成 → 目录放回去，两边仍然一致
		s.audit(id, "remove", false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "写 index 失败：" + err.Error(), ID: id}
	}

	s.audit(id, "remove", true, "")
	if trash == "" {
		// index 有记录但目录早就没了（外部删除 / 手工清理）：清理记录，返回 cleaned。
		return surfaceReceipt{Status: surfaceCleaned, Reason: "目录不存在，已清理 index 记录", ID: id}
	}
	return surfaceReceipt{Status: surfaceApplied, ID: id}
}

// ---- 启用 / 禁用（PR-14b 插件页的写路径）----

// SetEnabled 翻转 index 里某条记录的 enabled（插件页的开关）。**永不返回 error**。
//
// 三条语义逐条定死（01-design §5.5「禁用 / 卸载 / 墓碑必须说同一件事」）：
//  1. **一个字节的插件文件都不动**：禁用不是卸载，也不改 hash / installedAt —— 那两个
//     字段描述「装的还是不是那一版」，与「现在让不让跑」无关；改它会让下次升级误判。
//  2. **先 .bak 再原子写 index**（走 writeIndex，与安装/卸载同一条路）：手改坏了还能回滚。
//  3. **找不到 id → rejected，index 一个字节不改**：把「未知 id」写成一次成功的 enable
//     会让用户以为开关生效了，而磁盘上根本没有这一条。
//
// 禁用后的**能力收走**由既有判据自动发生，不需要额外动作：surface_command 的 Enabled()
// 变成 false、`hai-surface://` 的协议侧 404、渲染层 loader 下一轮 sync 卸载 provider。
func (s *surfaceStore) SetEnabled(id string, enabled bool) surfaceReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setEnabled(id, enabled)
}

func (s *surfaceStore) setEnabled(id string, enabled bool) surfaceReceipt {
	id = strings.TrimSpace(id)
	if !surfaceIDPattern.MatchString(id) {
		// 非法 id 不写盘（也没有可信的 id 可以记进 audit）。
		return surfaceReceipt{Status: surfaceRejected, Reason: fmt.Sprintf("id 不合法：%q（需匹配 ^[a-z][a-z0-9-]{1,40}$）", id), ID: id}
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	if err := s.ensureStore(); err != nil {
		s.audit(id, action, false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "创建 surfaces 目录失败：" + err.Error(), ID: id}
	}
	idx, prev, lock, lerr := s.lockIndex()
	if lerr != nil {
		s.audit(id, action, false, lerr.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "读 index 失败：" + lerr.Error(), ID: id}
	}
	defer closeLock(lock)

	pos := idx.pos(id)
	if pos < 0 {
		s.audit(id, action, false, "index 中不存在该 id")
		return surfaceReceipt{Status: surfaceRejected, Reason: fmt.Sprintf("index 里没有 id=%s（先 surface_install）", id), ID: id}
	}
	// 只翻这一个字段：整条 entry 原样回写（version/hash/installedAt/source 逐字不变）。
	entry := idx.Items[pos]
	entry.Enabled = enabled
	idx.Items[pos] = entry
	if err := s.writeIndex(lock, idx, prev); err != nil {
		s.audit(id, action, false, err.Error())
		return surfaceReceipt{Status: surfaceStatusError, Reason: "写 index 失败：" + err.Error(), ID: id}
	}
	s.audit(id, action, true, "")
	return surfaceReceipt{Status: surfaceApplied, ID: entry.ID, Version: entry.Version, Hash: entry.Hash}
}

// ---- 审计（读侧：插件页的「最近调用记录」）----

// 审计读取的两个界：缺省 50 行（一屏表格），上限 200 行（再多也不该塞进设置页）。
const (
	surfaceAuditDefaultLimit = 50
	surfaceAuditMaxLimit     = 200
)

// auditLine audit.jsonl 的一行（只解出插件页要展示的字段；其余字段忽略）。
//
// 用**独立结构**而不是 map[string]any：页面按固定列渲染（id / action / cmd / ok / ts），
// 一份无字段约束的 map 会让「少一个 ts」变成渲染期的 undefined 而不是这里的零值。
type auditLine struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Cmd    string `json:"cmd,omitempty"`
	OK     bool   `json:"ok"`
	TS     string `json:"ts"`
	Reason string `json:"reason,omitempty"`
}

// ReadAudit 读 audit.jsonl **尾部**最多 limit 行，按时间正序（旧 → 新）返回。
//
// limit ≤ 0 = 缺省 50；超过 200 压到 200。文件不存在 = 空列表（不是错误：一次都没装过
// 插件的新用户走的正是这条路）。
//
// **坏行跳过，不报错**：审计是观测面（§5.4 第 6 条「写失败不影响主流程」的对称面）。
// 一次断电写坏的半行、或用户手工编辑过的那一行，不该让整个「调用记录」区域变成一条
// 错误 —— 那样用户既看不到坏行，也看不到它后面那 49 条好行。跳过 = 少一条记录，
// 而「整段失败」= 少一个功能。
func (s *surfaceStore) ReadAudit(limit int) ([]auditLine, error) {
	if limit <= 0 {
		limit = surfaceAuditDefaultLimit
	}
	if limit > surfaceAuditMaxLimit {
		limit = surfaceAuditMaxLimit
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(), surfaceAuditName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []auditLine{}, nil
		}
		return nil, err
	}
	rows := bytes.Split(raw, []byte("\n"))
	out := make([]auditLine, 0, limit)
	// 从尾部往前收：只解析最后 limit 条**非空**行（大文件不必整份解析）。
	for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
		line := bytes.TrimSpace(rows[i])
		if len(line) == 0 {
			continue
		}
		var rec auditLine
		if json.Unmarshal(line, &rec) != nil {
			continue // 坏行跳过（见上：不报错）
		}
		out = append(out, rec)
	}
	// 收的时候是倒序，翻回文件顺序（表格按「旧 → 新」渲染，最新一行在最下面）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ---- 列表 ----

// List 读 index。enabledOnly = 只返回启用的（注册表视角）。
func (s *surfaceStore) List(enabledOnly bool) surfaceListResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, _, lock, err := s.lockIndex()
	if err != nil {
		return surfaceListResult{Status: surfaceStatusError, Reason: "读 index 失败：" + err.Error(), Items: []surfaceEntry{}}
	}
	closeLock(lock)

	items := make([]surfaceEntry, 0, len(idx.Items))
	for _, it := range idx.Items {
		if enabledOnly && !it.Enabled {
			continue
		}
		items = append(items, it)
	}
	// status=applied 在这里的含义是「这一份来自已落盘的 index」，不是「刚装完」
	// （状态集合是封闭的，见 surfaceStatus* 注释）。
	return surfaceListResult{Status: surfaceApplied, Items: items}
}

// ---- 查询（PR-14 的两个消费者：宿主命令白名单 / 渲染层索引 IPC 的 Go 侧口径）----

// Enabled 该 id 在 index 里且 enabled=true。
//
// 这是**协议与命令白名单共用的唯一判据**：禁用/卸载后 `hai-surface://` 必须 404、
// `surface_command` 必须 denied。两处各写一遍「enabled 是什么意思」，迟早一处松一处紧，
// 而松的那一侧就是「禁用只是 UI 上灰了」—— 安全条目会变成装饰。
//
// index 读不出来（损坏/不存在）一律 false（fail-closed）。
func (s *surfaceStore) Enabled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, _, lock, err := s.lockIndex()
	if err != nil {
		return false
	}
	closeLock(lock)
	pos := idx.pos(id)
	return pos >= 0 && idx.Items[pos].Enabled
}

// Manifest 读 `<root>/<id>/surface.json`（id 先过 pattern，绝不拼接未校验的字符串）。
//
// 与 validateSurfaceSource 分开是刻意的：那一份是**安装期**校验（含拷贝后复算 hash），
// 这一份是**运行期**读声明（commands/data/icon），不该因为「目录里有别的大文件」而失败。
func (s *surfaceStore) Manifest(id string) (surfaceManifest, error) {
	mf := surfaceManifest{}
	if !surfaceIDPattern.MatchString(strings.TrimSpace(id)) {
		return mf, fmt.Errorf("id 不合法：%q", id)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(), strings.TrimSpace(id), "surface.json"))
	if err != nil {
		return mf, fmt.Errorf("读 surface.json 失败：%v", err)
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return mf, fmt.Errorf("surface.json 不是合法 JSON：%v", err)
	}
	return mf, nil
}

// ---- index：加锁 / 解析 / 隔离损坏 / 原子写 ----

// lockIndex 加 flock 并读出当前 index。文件不存在 = 空索引（lock 为 nil）。
//
// 损坏（非法 JSON / version 不匹配）时**不静默覆盖**：原文件改名成
// `index.json.corrupt-<ts>` 原样备份，本次按空列表继续。改名失败则报错 ——
// 那种情况下继续写就等于把用户唯一的一份清单抹了。
func (s *surfaceStore) lockIndex() (surfaceIndex, []byte, *os.File, error) {
	empty := surfaceIndex{Version: surfaceIndexVersion, Items: []surfaceEntry{}}
	root := s.Dir()
	if err := os.MkdirAll(root, surfaceDirMode); err != nil {
		return empty, nil, nil, err
	}
	idxPath := filepath.Join(root, surfaceIndexName)
	lock, err := atomicfile.LockPath(idxPath)
	if err != nil {
		return empty, nil, nil, err
	}
	raw, err := os.ReadFile(idxPath)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil, lock, nil
	}
	if err != nil {
		closeLock(lock)
		return empty, nil, nil, err
	}
	var idx surfaceIndex
	if json.Unmarshal(raw, &idx) != nil || idx.Version != surfaceIndexVersion {
		backup := idxPath + ".corrupt-" + surfaceStamp()
		if rerr := os.Rename(idxPath, backup); rerr != nil {
			closeLock(lock)
			return empty, nil, nil, fmt.Errorf("index.json 损坏且无法备份（%v）：%s", rerr, backup)
		}
		log.Printf("surfaces: index.json 损坏（version=%d），已备份为 %s，本次按空列表启动", idx.Version, backup)
		return empty, nil, lock, nil
	}
	if idx.Items == nil {
		idx.Items = []surfaceEntry{}
	}
	return idx, raw, lock, nil
}

// writeIndex 原子写 index，并在覆盖前把**旧内容**存成 .bak。
//
// prev 为空 = 还没有前一份（首次安装）：不造空 .bak —— 一个空的回滚点比没有更危险。
func (s *surfaceStore) writeIndex(lock *os.File, idx surfaceIndex, prev []byte) error {
	root := s.Dir()
	idx.Version = surfaceIndexVersion
	if idx.Items == nil {
		idx.Items = []surfaceEntry{}
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	if len(prev) > 0 {
		// 逐字节复制旧文件：.bak 是回滚点，不是「重新序列化一遍」。
		if err := atomicfile.ReplaceReader(filepath.Join(root, surfaceBakName), bytes.NewReader(prev), surfaceFileMode); err != nil {
			return fmt.Errorf("写 %s 失败：%w", surfaceBakName, err)
		}
	}
	// 已持 flock（lockIndex 拿的），走 ReplaceReaderLocked 而不是 Replace：
	// 再取一次同一把锁会自己挡住自己（非阻塞 flock）。
	return atomicfile.ReplaceReaderLocked(lock, filepath.Join(root, surfaceIndexName), bytes.NewReader(body), surfaceFileMode)
}

func closeLock(lock *os.File) {
	if lock != nil {
		_ = lock.Close()
	}
}

// pos 返回 id 在有序列表里的位置（-1 = 没有）。
func (idx surfaceIndex) pos(id string) int {
	for i := range idx.Items {
		if idx.Items[i].ID == id {
			return i
		}
	}
	return -1
}

// ---- 校验 ----

// surfaceSource 校验通过的源（entryRel 用于「拷贝后复算 hash」）。
type surfaceSource struct {
	manifest  surfaceManifest
	entryPath string // 源目录里的真实路径（已解析软链，用于算 hash）
	entryRel  string // 相对 target 的路径（用于找到暂存目录里的那一份）
	hash      string
}

// validateSurfaceSource 校验源目录并算出入口文件的 sha256。
//
// 失败时也返回已解出的 manifest（能解多少算多少）：回执与 audit 里带上 id
// 才能让人对上「哪个包被拒了」。
func validateSurfaceSource(target string) (surfaceSource, error) {
	mf := surfaceManifest{}
	if strings.TrimSpace(target) == "" {
		return surfaceSource{manifest: mf}, errors.New("target 不能为空")
	}
	if !filepath.IsAbs(target) {
		return surfaceSource{manifest: mf}, fmt.Errorf("target 必须是绝对路径：%q", target)
	}
	info, err := os.Stat(target)
	if err != nil {
		return surfaceSource{manifest: mf}, fmt.Errorf("target 不是可读目录：%v", err)
	}
	if !info.IsDir() {
		return surfaceSource{manifest: mf}, fmt.Errorf("target 不是目录：%q", target)
	}

	raw, err := os.ReadFile(filepath.Join(target, "surface.json"))
	if err != nil {
		return surfaceSource{manifest: mf}, fmt.Errorf("读 surface.json 失败：%v", err)
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return surfaceSource{manifest: mf}, fmt.Errorf("surface.json 不是合法 JSON：%v", err)
	}
	if !surfaceIDPattern.MatchString(mf.ID) {
		return surfaceSource{manifest: mf}, fmt.Errorf("id 不合法：%q（需匹配 ^[a-z][a-z0-9-]{1,40}$）", mf.ID)
	}
	if v := strings.TrimSpace(mf.Version); v == "" {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: version 不能为空", mf.ID)
	} else if len([]rune(v)) > 32 {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: version 过长（%d 字符，上限 32）", mf.ID, len([]rune(v)))
	}
	// commands 声明也在**写盘前**判死（09-agent-bridge D11）。运行期那四条旧命令早已
	// 恒拒（surfaceHostCommands 是空表），但只拒调用不够：装上去之后用户看到的是一个
	// 「面板打开、每次调用都 denied」的插件，而且 index 里从此多一条永远修不好的记录。
	// 安装期直接拒绝，作者拿到的是「哪条命令、该改用哪一组」的可行动理由。
	if err := validateSurfaceCommands(mf.ID, mf.Commands); err != nil {
		return surfaceSource{manifest: mf}, err
	}
	// tools[]（§5.3 / §7.3）同样在**写盘前**判死：装上去之后才发现「名字非法 / schema
	// 不是 object / 没声明 tool.call」只能卸载重装，而那时模型已经看到过一个永远
	// 调不通的工具。它紧跟 commands 校验：同属「声明即授权」这一层，且都很便宜
	// （不碰磁盘、不算 hash）。
	if err := validateSurfaceTools(mf); err != nil {
		return surfaceSource{manifest: mf}, err
	}

	entry := strings.TrimSpace(mf.Entry)
	if entry == "" {
		entry = surfaceDefaultEntry // 默认 panel.js（manifest 可以省略 entry）
	}
	entryPath, err := resolveSurfaceEntry(target, entry)
	if err != nil {
		return surfaceSource{manifest: mf}, err
	}
	st, err := os.Stat(entryPath)
	if err != nil || !st.Mode().IsRegular() {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: entry %q 不是常规文件（面板入口默认 %s）", mf.ID, entry, surfaceDefaultEntry)
	}
	if st.Size() > maxSurfaceEntryBytes {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: entry %q 有 %s，超过 256 KiB 上限", mf.ID, entry, humanKiB(st.Size()))
	}
	// entries[] 与 entry/icon 同属「manifest 指哪读哪」的声明：形状/重复/逃逸必须在**写盘前**
	// 判死（D-3）。它在体积校验之后、hash 之前 —— 便宜的先跑，且失败时还没算过任何东西。
	if err := validateSurfaceEntries(target, mf.ID, mf.Entries); err != nil {
		return surfaceSource{manifest: mf}, err
	}

	total, err := surfaceTreeSize(target)
	if err != nil {
		return surfaceSource{manifest: mf}, err
	}
	if total > maxSurfaceDirBytes {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: 目录共 %s，超过 1 MiB 上限", mf.ID, humanKiB(total))
	}

	hash, err := sha256File(entryPath)
	if err != nil {
		return surfaceSource{manifest: mf}, fmt.Errorf("%s: 计算 sha256 失败：%v", mf.ID, err)
	}
	return surfaceSource{manifest: mf, entryPath: entryPath, entryRel: filepath.Clean(entry), hash: hash}, nil
}

// surfaceRetiredCommands P1c/D11 下线、安装期一律拒绝的四条旧宿主命令。
//
// 与运行期那张空表（surfaceHostCommands）刻意分开：那张表是**授权**（谁能通过），
// 这里是**装得上还是装不上**（作者该不该继续写它）。两者唯一的共同点是要一起改 ——
// 哪天真的加回某条命令，这张表与那张表必须同一次提交里对齐。
var surfaceRetiredCommands = []string{"workspace_list", "session_list", "git_snapshot", "ui_inspect"}

// validateSurfaceCommands 安装期校验 manifest.commands：四条旧命令出现即拒绝。
//
// 只认**名字**：参数形态由各命令自己的 handler 校验（运行期），安装期多解析一层就等于
// 让 manifest 能通过参数去探测宿主实现。其它命令（agent.* / composer.quote / ui.*）
// 一律不受影响 —— 这条闸只关 D11 的那四条，不扩权也不收权。
func validateSurfaceCommands(id string, cmds []string) error {
	for _, raw := range cmds {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		for _, retired := range surfaceRetiredCommands {
			if name != retired {
				continue
			}
			return fmt.Errorf("%s: commands 声明了 %q —— D11 已下线，请改用 agent.*（%s / %s / %s）",
				id, name, surfaceAgentSessionName, surfaceAgentRunName, surfaceAgentCancelName)
		}
	}
	return nil
}

// resolveSurfaceEntry 把 manifest 的 entry 解析成 target 内的真实文件路径。
// 三道闸（对齐 01-design §5.5「装插件 = 信任它」的最低防线：至少要挡住「装 A 读 B」）：
//  1. 不许绝对路径（否则 manifest 就是一次任意文件读取）；
//  2. 不许 `..`（Clean 之后按分段判断，`a/../../b` 也算逃逸）；
//  3. EvalSymlinks 之后仍必须在 target 内（软链逃逸）。
//
// validateSurfaceTools 校验 manifest.tools[]（D-3：任何一条不过 = 安装 rejected）。
//
// 三条取舍，改之前先读：
//  1. **零条合法**（缺省 / 空数组）：老 manifest 一个 tools 字段都没写，不能因此装不上；
//  2. **通道声明是硬条件**（§5.2 / §5.1）：有工具却在 data 里没收 tool.call、或在
//     commands 里没收 tool.result 的插件，装上之后每次调用都只能等超时 —— 那比装不上
//     更糟（用户看到的是「插件在，工具永远失败」）。reason 点名缺哪一条，作者据此改；
//  3. **只判形状，不判语义**：required 里的字段是否真在 properties 里、schema 是否有
//     意义，交给插件自己与模型 —— 这一层多判一次只会把能跑的 schema 判死。
func validateSurfaceTools(mf surfaceManifest) error {
	if len(mf.Tools) == 0 {
		return nil
	}
	id := strings.TrimSpace(mf.ID)
	if len(mf.Tools) > maxSurfaceTools {
		return fmt.Errorf("%s: tools 有 %d 项，超过 %d 项上限", id, len(mf.Tools), maxSurfaceTools)
	}
	if !surfaceDeclares(mf.Data, surfaceToolCallEvent) {
		return fmt.Errorf("%s: tools 非空但 data 未声明 %q（插件收不到工具调用，每次调用都会超时）", id, surfaceToolCallEvent)
	}
	if !surfaceDeclares(mf.Commands, surfaceAgentToolResult) {
		return fmt.Errorf("%s: tools 非空但 commands 未声明 %q（插件无法回结果，每次调用都会超时）", id, surfaceAgentToolResult)
	}
	seen := make(map[string]struct{}, len(mf.Tools))
	for i, t := range mf.Tools {
		name := strings.TrimSpace(t.Name)
		if !surfaceToolNamePattern.MatchString(name) {
			return fmt.Errorf("%s: tools[%d].name 不合法：%q（需匹配 ^[a-z][a-z0-9_]{0,40}$）", id, i, name)
		}
		// `__` 是模型侧名 `plugin__<id>__<name>` 的分隔符：工具名里再出现就分不清
		// 「这条工具属于哪个插件、叫什么」，跨插件拒绝也就判不干净。
		if strings.Contains(name, "__") {
			return fmt.Errorf("%s: tools[%d].name 不许含 %q（%q 是模型侧名 plugin__<id>__<name> 的分隔符）", id, i, "__", name)
		}
		if _, dup := seen[name]; dup {
			// 重复名 = 同一插件里两个工具抢一个模型侧名：后注册的静默覆盖前一个，
			// 装完再发现只能卸载重装（与 entries 的 id 重复同一条理由）。
			return fmt.Errorf("%s: tools 的 name 重复：%q（同一插件内工具名必须唯一）", id, name)
		}
		seen[name] = struct{}{}
		// 描述是审批卡片上唯一的人读说明（§6.4）：空描述等于让用户在不知情的情况下授权。
		desc := strings.TrimSpace(t.Description)
		if desc == "" {
			return fmt.Errorf("%s: tools[%d] (%s) 缺 description（审批卡片要显示它）", id, i, name)
		}
		if n := len([]rune(desc)); n > maxSurfaceToolDescriptionChars {
			return fmt.Errorf("%s: tools[%d] (%s) 的 description 过长（%d 字符，上限 %d）",
				id, i, name, n, maxSurfaceToolDescriptionChars)
		}
		if err := validateSurfaceToolInput(id, i, name, t.Input); err != nil {
			return err
		}
		// 0 = 默认 10s；非零必须在 [100ms, 60s]（§6.4）。负数与 1ms 同样拒绝：
		// 它们不是「更快」，是「调用必然打不满一次插件往返」。
		if ms := t.TimeoutMS; ms != 0 && (ms < surfaceToolMinTimeoutMS || ms > surfaceToolMaxTimeoutMS) {
			return fmt.Errorf("%s: tools[%d] (%s) 的 timeout_ms = %d，必须在 [%d, %d]（0 = 默认 %d ms）",
				id, i, name, ms, surfaceToolMinTimeoutMS, surfaceToolMaxTimeoutMS, surfaceToolDefaultTimeoutMS)
		}
	}
	return nil
}

// validateSurfaceToolInput 校验单个工具的参数 schema（§5.3）。
//
// 「必须是 JSON object 且 type=object」是硬条件：模型侧 schema 会被 provider 直接
// 翻成请求参数描述，一个 `{"type":"string"}` 的 input 在那边是非法请求（整轮 400），
// 而这跟插件作者写的意图无关 —— 只能安装期拦住。
func validateSurfaceToolInput(id string, index int, name string, raw json.RawMessage) error {
	where := fmt.Sprintf("%s: tools[%d] (%s) 的 input", id, index, name)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return fmt.Errorf(`%s 缺失：必须是 JSON object（如 {"type":"object","properties":{}}）`, where)
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("%s 必须是 JSON object（实际以 %q 开头）", where, trimmed[0])
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return fmt.Errorf("%s 不是合法 JSON：%v", where, err)
	}
	rawType, ok := obj["type"]
	if !ok {
		return fmt.Errorf(`%s 缺 "type"：必须是 "object"`, where)
	}
	var typ string
	if err := json.Unmarshal(rawType, &typ); err != nil || typ != "object" {
		return fmt.Errorf(`%s 的 "type" 必须是 "object"（实际 %s）`, where, string(bytes.TrimSpace(rawType)))
	}
	// required / properties 只判形状：两者是 provider 侧最常被直接消费的两个键，
	// 形状错了同样是整轮请求失败，而语义（必填是否在 properties 里）不归这一层管。
	if rawReq, ok := obj["required"]; ok {
		var req []string
		if err := json.Unmarshal(rawReq, &req); err != nil {
			return fmt.Errorf(`%s 的 "required" 必须是字符串数组`, where)
		}
	}
	if rawProps, ok := obj["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(rawProps, &props); err != nil {
			return fmt.Errorf(`%s 的 "properties" 必须是对象`, where)
		}
	}
	// $ref：P2 不做引用解析（既没有解析器，也没有「能指向哪」的白名单）。允许了就等于
	// 让 manifest 声明一个宿主永远不会展开的 schema —— 模型看到的是无法满足的参数。
	if path, found := findSurfaceToolRef(trimmed); found {
		return fmt.Errorf(`%s 不许含 "$ref"（%s）：P2 只支持内联定义`, where, path)
	}
	// 体积按**紧凑序列化后**的字节数算（与真正发给 provider 的形状同口径）。
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return fmt.Errorf("%s 不是合法 JSON：%v", where, err)
	}
	if n := compact.Len(); n > maxSurfaceToolInputBytes {
		return fmt.Errorf("%s 有 %d 字节，超过 16 KiB 上限", where, n)
	}
	return nil
}

// findSurfaceToolRef 在内联 schema 里找 `$ref` 键，返回它的路径（"input.properties.x.$ref"）。
//
// 走解码后的 any 而不是正则在原文里搜：`{"description":"别用 $ref"}` 是合法描述，
// 正则会把作者的解释当成引用拒掉 —— 那是最典型的假红。
func findSurfaceToolRef(raw []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return walkSurfaceToolRef(v, "input")
}

func walkSurfaceToolRef(v any, path string) (string, bool) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if k == "$ref" {
				return path + "." + k, true
			}
			if p, found := walkSurfaceToolRef(child, path+"."+k); found {
				return p, true
			}
		}
	case []any:
		for i, child := range node {
			if p, found := walkSurfaceToolRef(child, fmt.Sprintf("%s[%d]", path, i)); found {
				return p, true
			}
		}
	}
	return "", false
}

func resolveSurfaceEntry(target, entry string) (string, error) {
	if filepath.IsAbs(entry) {
		return "", fmt.Errorf("entry %q 不许是绝对路径（必须是 target 内的相对路径）", entry)
	}
	clean := filepath.Clean(entry)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("entry %q 逃出了 target（不许含 ..）", entry)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("解析 target 失败：%v", err)
	}
	realEntry, err := filepath.EvalSymlinks(filepath.Join(realTarget, clean))
	if err != nil {
		return "", fmt.Errorf("entry %q 不存在（面板入口默认是 %s）：%v", entry, surfaceDefaultEntry, err)
	}
	if !strings.HasPrefix(realEntry, realTarget+string(os.PathSeparator)) {
		return "", fmt.Errorf("entry %q 经软链逃出了 target", entry)
	}
	return realEntry, nil
}

// validateSurfaceEntries 校验 manifest.entries[]（D-3：任何一条不过 = 安装 rejected）。
//
// 三条边界上的取舍，改之前先读：
//  1. **零条合法**（缺省 / 空数组）：宿主会给一个默认入口（D-2 的硬约束是「至少一个可见入口」），
//     所以这里不能把「没写 entries」判成错 —— 那样所有老 manifest 都会装不上。
//  2. **title 按 rune 数**：`len([]rune(s))`，与 version 的既有校验同口径（字节数会把中文判死）。
//  3. **icon 只查「在目录内」，不查「存在」**：逃逸是安全问题（拒绝安装），缺失只是少一个图标
//     （D-3 明说用 kind 图标兜底、不阻塞安装）。两者处置不同，判据也就必须分开写。
func validateSurfaceEntries(target, id string, list []surfaceEntryDecl) error {
	if len(list) == 0 {
		return nil
	}
	if len(list) > maxSurfaceEntries {
		return fmt.Errorf("%s: entries 有 %d 项，超过 %d 项上限", id, len(list), maxSurfaceEntries)
	}
	seen := make(map[string]struct{}, len(list))
	for i, e := range list {
		entryID := strings.TrimSpace(e.ID)
		if !surfaceIDPattern.MatchString(entryID) {
			return fmt.Errorf("%s: entries[%d].id 不合法：%q（需匹配 ^[a-z][a-z0-9-]{1,40}$）", id, i, entryID)
		}
		if _, dup := seen[entryID]; dup {
			// 重复 id = 两个入口共用一个 params.entry：打开「课表」会命中上一次的标签，
			// 面板也分不清该画哪一页。这在写盘前必须死（装完再发现只能卸载重装）。
			return fmt.Errorf("%s: entries 的 id 重复：%q（每个入口的 params.entry 必须唯一）", id, entryID)
		}
		seen[entryID] = struct{}{}
		// title 两种写法（R16b）：单字符串 或 {zh,en}。长度与「至少一个非空」在这里一次判死。
		if err := e.Title.validate(maxSurfaceEntryTitleChars); err != nil {
			return fmt.Errorf("%s: entries[%d] (%s) 的 %v", id, i, entryID, err)
		}
		if err := validateSurfaceEntryIcon(target, e.Icon); err != nil {
			return fmt.Errorf("%s: entries[%d] (%s) 的 icon %v", id, i, entryID, err)
		}
	}
	return nil
}

// validateSurfaceEntryIcon 入口图标的路径纪律（与 resolveSurfaceEntry 同款三道闸）。
//
// 与 resolveSurfaceEntry 的唯一差别：**文件不存在不算错**（D-3：用 kind 图标兜底，不阻塞安装）。
// 因此「语法上在目录内」是硬判据，「解析后仍在目录内」（软链逃逸）只在文件真的存在时才判 ——
// 不存在的路径谈不上逃逸，而存在的软链指到目录外就是一次任意文件读取。
func validateSurfaceEntryIcon(target, icon string) error {
	icon = strings.TrimSpace(icon)
	if icon == "" {
		return nil // 没声明图标：渲染层用 kind 图标（合法）
	}
	if filepath.IsAbs(icon) {
		return fmt.Errorf("不许是绝对路径：%q", icon)
	}
	// 反斜杠在 darwin/linux 上是合法文件名字符，但渲染层的 relInDir 与 index 协议都拒它：
	// 放进来会出现「Go 校验通过、渲染层 URL 拼不出来」的半合法状态。一处口径，两处都拒。
	if strings.Contains(icon, `\`) {
		return fmt.Errorf("不许含反斜杠：%q", icon)
	}
	clean := filepath.Clean(icon)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("逃出了插件目录（不许含 ..）：%q", icon)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("解析插件目录失败：%v", err)
	}
	realIcon, err := filepath.EvalSymlinks(filepath.Join(realTarget, clean))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // 缺失 → 渲染层兜底（不是安全问题）
		}
		return fmt.Errorf("解析失败：%v", err)
	}
	if !strings.HasPrefix(realIcon, realTarget+string(os.PathSeparator)) {
		return fmt.Errorf("经软链逃出了插件目录：%q", icon)
	}
	return nil
}

// surfaceTreeSize 目录总大小（含 manifest / icon；不含符号链接指向的内容）。
func surfaceTreeSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// copySurfaceTree 拷到暂存目录：目录 0700、文件 0600，符号链接一律拒绝。
//
// 为什么拒绝软链而不是「解析后拷内容」：来源目录在拷完到 rename 之间还能被改，
// 软链会让「校验过的内容」与「落盘的内容」变成两份不同的东西 —— 而这一轮的全部
// 安全性就建立在「校验过的就是落盘的」上。
func copySurfaceTree(src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, surfaceDirMode)
		case d.Type()&os.ModeSymlink != 0:
			return fmt.Errorf("插件目录不许包含符号链接：%s", rel)
		case !d.Type().IsRegular():
			return fmt.Errorf("插件目录里有非常规文件：%s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxSurfaceDirBytes {
			return fmt.Errorf("目录共 %s，超过 1 MiB 上限", humanKiB(total))
		}
		return copySurfaceFile(p, out)
	})
}

func copySurfaceFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, surfaceFileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- 落盘辅助 ----

// ensureStore 建根目录并预建 audit.jsonl（0600）。
//
// audit 预建失败只记日志：审计是观测面，不能因为它让安装做主（§5.4「写失败不影响
// 主流程」）。atomicfile.Append 建新文件用的是 0644，所以权限必须由这里先种下。
func (s *surfaceStore) ensureStore() error {
	root := s.Dir()
	if err := os.MkdirAll(root, surfaceDirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, surfaceAuditName), os.O_CREATE|os.O_WRONLY, surfaceFileMode)
	if err != nil {
		log.Printf("surfaces: 预建 %s 失败（不影响安装）：%v", surfaceAuditName, err)
		return nil
	}
	return f.Close()
}

// displaceOld 把已存在的 <id> 目录挪进 .trash（升级与「外部残留」共用），返回它
// 在 .trash 里的路径（"" = 原本就没有）。
func (s *surfaceStore) displaceOld(root, id string) (string, error) {
	dir := filepath.Join(root, id)
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	trashDir := filepath.Join(root, surfaceTrashDir)
	if err := os.MkdirAll(trashDir, surfaceDirMode); err != nil {
		return "", err
	}
	dst := filepath.Join(trashDir, id+"-"+surfaceStamp())
	if err := os.Rename(dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// restoreOld 回滚 displaceOld（index 没写成时用）：尽力把旧目录放回去。
func restoreOld(trash, dir string) {
	if trash == "" {
		return
	}
	if err := os.Rename(trash, dir); err != nil {
		log.Printf("surfaces: 回滚旧目录失败（留在 %s）：%v", trash, err)
	}
}

// audit 追加一行审计。**永不返回错误**（写失败只记日志）。
//
// 先 ensureStore：atomicfile.Append 建父目录用的是 0755，若「校验失败 + 首次安装」
// 这条路径直接追加，就会把 surfaces 根目录种成 0755（后面所有文件都住在这个洞
// 里）。审计写不写得成是次要点，目录权限不是。
func (s *surfaceStore) audit(id, action string, ok bool, reason string) {
	s.writeAudit(map[string]any{"id": id, "action": action, "ok": ok}, reason)
}

// auditCommand `surface_command` 的审计行：在通用三字段上多一条 `cmd`。
//
// 为什么单独一条而非塞进 reason：「哪个命令被拒了」是权限排查的**第一个**问题
// （01-design §5.5「软约束 + 可审计」），写进自由文本就再也统计不出来。
// 允许的调用也写一行（ok:true）—— 只有拒绝记录时，「这个插件到底调过什么」无解，
// 而那正是用户装完插件后唯一的对账材料。
func (s *surfaceStore) auditCommand(id, cmd string, ok bool, reason string) {
	s.writeAudit(map[string]any{"id": id, "action": "command", "cmd": cmd, "ok": ok}, reason)
}

// writeAudit 审计的唯一写点（rec 里已有 id/action/ok，timestamp 与 reason 在这里补）。
func (s *surfaceStore) writeAudit(rec map[string]any, reason string) {
	_ = s.ensureStore()
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	if reason != "" {
		rec["reason"] = clipText(reason, 300)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := atomicfile.Append(filepath.Join(s.Dir(), surfaceAuditName), string(b)+"\n"); err != nil {
		log.Printf("surfaces: 写 %s 失败（不影响主流程）：%v", surfaceAuditName, err)
	}
}

// ---- 小工具 ----

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// surfaceStamp 时间戳（.trash/<id>-<ts> 与 index.json.corrupt-<ts> 共用）。
// 文件名安全（无冒号）+ 纳秒级：同一秒内两次卸载也不会互相覆盖。
func surfaceStamp() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func humanKiB(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
