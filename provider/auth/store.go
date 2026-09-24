package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store OAuth 凭证持久化存储（~/.go-code/oauth.json，0600）。
// 对齐 pi credential-store.ts：写路径唯一（Modify 序列化读-改-写，per-provider 互斥）。
// 与 settings.json 分离：token 敏感且会轮换，不混入主配置。
type Store struct {
	mu     sync.Mutex
	path   string
	chains map[string]*sync.Mutex // per-provider 写互斥（对齐 pi enqueue per-provider promise chain）
}

// NewStore 构造凭证存储。path 为空时用默认 ~/.go-code/oauth.json。
func NewStore(path string) *Store {
	if path == "" {
		path = defaultStorePath()
	}
	return &Store{path: path, chains: map[string]*sync.Mutex{}}
}

// Path 存储文件路径。
func (s *Store) Path() string { return s.path }

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "oauth.json"
	}
	return filepath.Join(home, ".go-code", "oauth.json")
}

// Read 读取某 provider 的凭证；不存在返回 (nil, nil)。
func (s *Store) Read(providerID string) (*OAuthCredential, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	raw, ok := all[providerID]
	if !ok {
		return nil, nil
	}
	var cred OAuthCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("oauth store: corrupt entry %s: %w", providerID, err)
	}
	return &cred, nil
}

// Modify 序列化读-改-写（唯一写路径，对齐 pi CredentialStore.modify）。
// fn 返回新凭证 → 写入；返回 (nil, nil) → 保持原样；返回 (nil, err) → 透传错误。
func (s *Store) Modify(providerID string, fn func(cur *OAuthCredential) (*OAuthCredential, error)) error {
	chain := s.chain(providerID)
	chain.Lock()
	defer chain.Unlock()

	cur, err := s.Read(providerID)
	if err != nil {
		return err
	}
	next, err := fn(cur)
	if err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	if !next.Valid() {
		return fmt.Errorf("oauth store: invalid credential for %s", providerID)
	}
	return s.writeProvider(providerID, next)
}

// Delete 删除某 provider 的凭证（登出）。
func (s *Store) Delete(providerID string) error {
	chain := s.chain(providerID)
	chain.Lock()
	defer chain.Unlock()
	return s.deleteLocked(providerID)
}

func (s *Store) deleteLocked(providerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readAll()
	if err != nil {
		return err
	}
	if _, ok := all[providerID]; !ok {
		return nil
	}
	delete(all, providerID)
	return s.writeAll(all)
}

// List 列出已登录的 provider id。
func (s *Store) List() ([]string, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for id := range all {
		out = append(out, id)
	}
	return out, nil
}

// chain 取某 provider 的写互斥（懒创建）。
func (s *Store) chain(providerID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chains[providerID]
	if !ok {
		c = &sync.Mutex{}
		s.chains[providerID] = c
	}
	return c
}

func (s *Store) readAll() (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		// 坏文件：备份后重写覆盖（不阻塞登录）；备份便于排查。
		_ = os.Rename(s.path, s.path+".corrupt")
		return map[string]json.RawMessage{}, nil
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	return all, nil
}

func (s *Store) writeProvider(providerID string, cred *OAuthCredential) error {
	// 全局文件锁：整个文件的读-改-写必须原子（不同 provider 并发写会互相覆盖）。
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readAll()
	if err != nil {
		return err
	}
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	all[providerID] = b
	return s.writeAll(all)
}

// writeAll 原子写（临时文件 + rename；对齐项目 writeSettings 的原子模式）。
func (s *Store) writeAll(all map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
