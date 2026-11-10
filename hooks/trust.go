package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/seven7628/hai-harness/owner"
)

// TrustStatus 信任状态（内容哈希模型，抄 Codex 的 HookTrustStatus）。
type TrustStatus string

const (
	TrustManaged   TrustStatus = "managed"   // 托管层下发，始终可信
	TrustTrusted   TrustStatus = "trusted"   // 用户已确认
	TrustUntrusted TrustStatus = "untrusted" // 未确认（默认**不执行**）
	TrustModified  TrustStatus = "modified"  // 命令与确认时不一致
)

// Entry 一条已确认记录：命令的内容哈希（改一个字符即回到未信任）。
type Entry struct {
	Hash   string `json:"hash"`
	Scope  string `json:"scope,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// trustFileV2 信任库落盘格式（version=2 起）。
//
// 为什么 entries 必须带 owner 维（owner 隔离）：确认的语义是「**这个归属**下允许执行这条
// 命令」，归属是 agent / group / 工作区。旧格式把命令平铺在文件顶层 = 隐式「全局」：
// A 群确认过的命令在 B 智能体、C 项目里都算 trusted（审计 BR-21 的跨 owner 越权）。
type trustFileV2 struct {
	Version int                         `json:"version"`
	Entries map[string]map[string]Entry `json:"entries"` // owner → command → Entry
	Managed []string                    `json:"managed,omitempty"`
}

// trustFileV1 旧版格式（无 version，命令全局平铺）。只用于读取迁移。
type trustFileV1 struct {
	Entries map[string]Entry `json:"entries"`
	Managed []string         `json:"managed"`
}

// TrustStoreOwnerVersion 当前落盘版本。
const TrustStoreOwnerVersion = 2

// TrustStore 持久化在 ~/.go-code/hooks-state.json（独立文件，避免污染 settings.json，
// 也避免与 Electron 对 settings.json 的全量重写互相覆盖）。
//
// 实例仍是**进程级单例**（一个文件、一份真相，见 bridge 的 hookTrustStore），
// 但每一条记录的**归属**由 owner 决定；查询/写入都必须带 owner。
type TrustStore struct {
	Path string

	mu      sync.Mutex
	entries map[string]map[string]Entry // owner → command → Entry
	man     map[string]struct{}
	// migrated 读到的是 v1（旧全局格式）→ 已在内存里迁移到 legacy owner；
	// 下一次写入按 v2 落盘。只读不写时文件保持原样（不因「打开设置页」改用户文件）。
	migrated bool
}

// LoadTrust 读取信任库；文件缺失或损坏都返回空库（保守：全部未信任），不返回错误。
//
// 迁移规则（v1 → v2，见 migrateV1）：
//   - 旧全局条目**不会**被套到所有 agent/group —— 那正是本次要消除的跨 owner 越权；
//   - 改为落在 owner.Legacy 下，且**只在项目工作区这一维**参与判定
//     （见 Check 的兼容分支）：旧的确认记录本来就是为项目级 hook 建立的，
//     升级后项目会话照常工作，而 Hub 智能体/群聊必须重新确认；
//   - 迁移是只读的：文件在你下一次「确认信任」时才写成 v2（不偷偷改用户文件）。
func LoadTrust(path string) (*TrustStore, error) {
	ts := &TrustStore{
		Path:    path,
		entries: map[string]map[string]Entry{},
		man:     map[string]struct{}{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ts, nil
	}
	// 先按 v2 解析（带 version）；再回退 v1。用 map 探测而不是结构体零值判断，
	// 避免「v1 的 entries 恰好也能被 v2 结构体解析成空表」这种静默丢条目。
	var probe struct {
		Version int             `json:"version"`
		Entries json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ts, nil // 坏文件：空库（全部未信任），不阻断
	}
	if probe.Version >= TrustStoreOwnerVersion {
		var f trustFileV2
		if json.Unmarshal(raw, &f) != nil {
			return ts, nil
		}
		for own, cmds := range f.Entries {
			if owner.Kind(own) == "" || cmds == nil {
				continue // 未知归属：丢弃（不猜、不套给别人）
			}
			ts.entries[own] = cmds
		}
		for _, c := range f.Managed {
			ts.man[c] = struct{}{}
		}
		return ts, nil
	}
	var f1 trustFileV1
	if json.Unmarshal(raw, &f1) != nil {
		return ts, nil
	}
	for _, c := range f1.Managed {
		ts.man[c] = struct{}{}
	}
	if len(f1.Entries) > 0 {
		ts.entries[owner.Legacy] = f1.Entries
		ts.migrated = true
	}
	return ts, nil
}

// Migrated 本次加载是否发生过 v1 → v2 迁移（诊断/测试用；落盘发生在下一次 Trust）。
func (t *TrustStore) Migrated() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.migrated
}

// Check 判定命令在某 owner 下的信任状态。owner 为空或非法 → 一律未信任
// （宁可多确认一次，也不让「不知道属于谁」的确认生效）。
//
// 兼容分支（legacy）：只有**项目工作区** owner 会回退到旧版全局条目 ——
// 这是「升级后项目会话照常工作」的兼容路径；agent / group 一律不继承（产品决策：
// A 智能体/群的确认不得让 B 智能体/群免确认）。
func (t *TrustStore) Check(ownerID, command string) TrustStatus {
	if t == nil {
		return TrustUntrusted
	}
	if _, ok := t.man[command]; ok {
		return TrustManaged
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := checkIn(t.entries[ownerID], command); ok {
		return st
	}
	if owner.IsWorkspace(ownerID) {
		if st, ok := checkIn(t.entries[owner.Legacy], command); ok {
			return st
		}
	}
	return TrustUntrusted
}

// checkIn 在某个 owner 的表里查命令；ok=false 表示「该 owner 没有这条记录」。
func checkIn(m map[string]Entry, command string) (TrustStatus, bool) {
	if m == nil {
		return TrustUntrusted, false
	}
	e, ok := m[command]
	if !ok {
		return TrustUntrusted, false
	}
	if e.Hash != hashCommand(command) {
		return TrustModified, true
	}
	return TrustTrusted, true
}

// Trust 记录一条确认（owner = 谁被确认；scope/reason 只作展示与审计）。
//
// 拒绝 legacy 与非法 owner：legacy 是迁移只读层，由它派生新确认会让旧全局条目
// 继续扩散，等于把刚拆掉的跨 owner 共享偷偷装回去。
func (t *TrustStore) Trust(ownerID, command, scope, reason string) error {
	if t == nil {
		return fmt.Errorf("hooks: 信任库未初始化")
	}
	if !owner.Valid(ownerID) {
		return fmt.Errorf("hooks: 非法 owner %q（必须是 workspace:/agent:/group: 之一）", ownerID)
	}
	t.mu.Lock()
	cmds := t.entries[ownerID]
	if cmds == nil {
		cmds = map[string]Entry{}
		t.entries[ownerID] = cmds
	}
	cmds[command] = Entry{Hash: hashCommand(command), Scope: scope, Reason: reason}
	snapshot := t.snapshotLocked()
	t.migrated = false // 已按 v2 落盘
	t.mu.Unlock()

	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	// 原子写（同包 writeFileAtomic：目录 0700、临时文件 0600 + rename）。
	// 半截 JSON 会让下次 LoadTrust 按空库处理 —— 用户之前所有确认一次性作废，
	// 且没有任何提示（信任库是"宁可多确认一次"的语义，绝不能静默丢）。
	return writeFileAtomic(t.Path, append(raw, '\n'))
}

// snapshotLocked 生成一份可序列化快照（调用方持锁）。
func (t *TrustStore) snapshotLocked() trustFileV2 {
	entries := make(map[string]map[string]Entry, len(t.entries))
	for own, cmds := range t.entries {
		if len(cmds) == 0 {
			continue
		}
		cp := make(map[string]Entry, len(cmds))
		for cmd, e := range cmds {
			cp[cmd] = e
		}
		entries[own] = cp
	}
	managed := make([]string, 0, len(t.man))
	for k := range t.man {
		managed = append(managed, k)
	}
	sort.Strings(managed)
	return trustFileV2{Version: TrustStoreOwnerVersion, Entries: entries, Managed: managed}
}

// Owners 已登记过确认的 owner（升序；含 legacy）。
func (t *TrustStore) Owners() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.entries))
	for own := range t.entries {
		out = append(out, own)
	}
	sort.Strings(out)
	return out
}

// HasOwnerEntry 该 owner 是否有一条**直接**记录（不含 legacy 兼容回退）。
// hooks_list 用它区分「本来就没确认过」与「靠旧全局条目在跑」。
func (t *TrustStore) HasOwnerEntry(ownerID, command string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[ownerID][command]
	return ok
}

// LegacyOnly 该命令在当前 owner 下的信任是否**只**来自旧全局条目
// （面板要能提示「这条靠迁移前的全局确认在跑，建议在当前归属下重新确认」）。
func (t *TrustStore) LegacyOnly(ownerID, command string) bool {
	if t == nil || !owner.IsWorkspace(ownerID) {
		return false
	}
	if t.HasOwnerEntry(ownerID, command) {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[owner.Legacy][command]
	return ok
}

func hashCommand(cmd string) string {
	sum := sha256.Sum256([]byte(cmd))
	return "sha256:" + hex.EncodeToString(sum[:8])
}
