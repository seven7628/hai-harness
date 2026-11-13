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
}

// surfaceReceipt 安装 / 卸载的回执（工具层直接序列化给模型与脚本）。
type surfaceReceipt struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	Hash    string `json:"hash,omitempty"`
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
		return surfaceReceipt{Status: surfaceAlreadyInstalled, ID: it.ID, Version: it.Version, Hash: it.Hash}
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
	return surfaceReceipt{Status: surfaceApplied, ID: entry.ID, Version: entry.Version, Hash: entry.Hash}
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

// resolveSurfaceEntry 把 manifest 的 entry 解析成 target 内的真实文件路径。
//
// 三道闸（对齐 01-design §5.5「装插件 = 信任它」的最低防线：至少要挡住「装 A 读 B」）：
//  1. 不许绝对路径（否则 manifest 就是一次任意文件读取）；
//  2. 不许 `..`（Clean 之后按分段判断，`a/../../b` 也算逃逸）；
//  3. EvalSymlinks 之后仍必须在 target 内（软链逃逸）。
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
//（01-design §5.5「软约束 + 可审计」），写进自由文本就再也统计不出来。
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
