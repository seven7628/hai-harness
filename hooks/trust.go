package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

// TrustStore 持久化在 ~/.go-code/hooks-state.json（独立文件，避免污染 settings.json，
// 也避免与 Electron 对 settings.json 的全量重写互相覆盖）。
type TrustStore struct {
	Path string

	mu      sync.Mutex
	entries map[string]Entry
	man     map[string]struct{}
}

// LoadTrust 读取信任库；文件缺失或损坏都返回空库（保守：全部未信任），不返回错误。
func LoadTrust(path string) (*TrustStore, error) {
	ts := &TrustStore{Path: path, entries: map[string]Entry{}, man: map[string]struct{}{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ts, nil
	}
	var file struct {
		Entries map[string]Entry `json:"entries"`
		Managed []string         `json:"managed"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return ts, nil
	}
	if file.Entries != nil {
		ts.entries = file.Entries
	}
	for _, c := range file.Managed {
		ts.man[c] = struct{}{}
	}
	return ts, nil
}

// Check 判定命令的信任状态。trust 为 nil（未初始化）时一律 Untrusted。
func (t *TrustStore) Check(command string) TrustStatus {
	if t == nil {
		return TrustUntrusted
	}
	if _, ok := t.man[command]; ok {
		return TrustManaged
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[command]
	if !ok {
		return TrustUntrusted
	}
	if e.Hash != hashCommand(command) {
		return TrustModified
	}
	return TrustTrusted
}

// Trust 记录一条确认（产品层将来接"是否允许"入口时调用）。
func (t *TrustStore) Trust(command, scope, reason string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.entries[command] = Entry{Hash: hashCommand(command), Scope: scope, Reason: reason}
	managed := make([]string, 0, len(t.man))
	for k := range t.man {
		managed = append(managed, k)
	}
	snapshot := struct {
		Entries map[string]Entry `json:"entries"`
		Managed []string         `json:"managed,omitempty"`
	}{Entries: t.entries, Managed: managed}
	t.mu.Unlock()

	sort.Strings(snapshot.Managed)
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(t.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(t.Path, append(raw, '\n'), 0o600)
}

func hashCommand(cmd string) string {
	sum := sha256.Sum256([]byte(cmd))
	return "sha256:" + hex.EncodeToString(sum[:8])
}
