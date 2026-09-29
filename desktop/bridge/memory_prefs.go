package main

// Project Memory 的用户开关（设置面板 → Agent 运行时的接线）。
//
// 生效时机：**下一个会话**，或重新初始化后的会话（switch_model / switch_persona /
// set_provider 会重建 loop）。正在运行的会话不改变 —— 这是决策而不是缺陷：
// 记忆注入是**系统提示词的一部分**，会话中途删掉它等于让同一会话里前后两轮的
// "你记得什么"不一致，而记忆的价值恰恰建立在跨轮一致上。改动等下一个会话，
// 用户预期明确，也不会出现"上一轮还记得、这一轮忘了"的诡异行为。
//
// 存储：~/.go-code/memory.json（全局一份，不分工作区）。理由与官方
// autoMemoryEnabled 一致 —— 它是**用户**开关，不是项目配置；而记忆内容本身才按
// 仓库身份分目录（memory/identity.go）。
//
// 三态：文件不存在 = 用户从未设置过 = 默认开（官方 autoMemoryEnabled 默认开）。
// 存成 *bool 而不是 bool，是为了区分"没设置过"与"显式设成关"，后者必须在 UI 上
// 显示为关而不是回落到默认值。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/seven7628/hai-harness/memory"
)

// memoryPrefs ~/.go-code/memory.json 的内容。
//
// 字段用 *bool：nil = 用户没设置过（默认开）；非 nil = 显式开/关。
// 写盘时 nil 不落字段，读回仍是 nil，语义闭合。
type memoryPrefs struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// memoryPrefFile 偏好文件路径（~/.go-code/memory.json）。
func memoryPrefFile() string { return homeCfgPath("memory.json") }

// memoryPrefStore 读改写偏好文件。
//
// 进程内缓存一份 + 写时落盘：设置面板的读请求可能很频繁（每次打开面板），
// 而这是一个几字节的小文件。写失败**不静默**：设置面板会回滚开关并显示错误 ——
// 「显示已关闭但重启后又开着」是比报错更糟的故障。
type memoryPrefStore struct {
	mu   sync.Mutex
	path string
}

func newMemoryPrefStore() *memoryPrefStore { return &memoryPrefStore{path: memoryPrefFile()} }

// load 返回当前偏好。文件不存在/损坏 = 用户没设置过（默认开），不报错：
// 偏好读不出来不该阻断会话创建。
func (s *memoryPrefStore) load() memoryPrefs {
	if s == nil {
		return memoryPrefs{} // 未装配偏好存储 = 用户没设置过 = 默认开
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var p memoryPrefs
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return p
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		fmt.Fprintf(os.Stderr, "[memory] 偏好文件损坏，按默认（开）处理（%s）: %v\n", s.path, err)
		return memoryPrefs{}
	}
	return p
}

// set 落盘。原子写（临时文件 + rename）：并发读（面板刷新）不会读到半个 JSON。
func (s *memoryPrefStore) set(p memoryPrefs) error {
	if s == nil {
		return fmt.Errorf("记忆偏好存储未装配")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化记忆偏好: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("建配置目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写记忆偏好: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘记忆偏好: %w", err)
	}
	return nil
}

// enabled 返回解析后的开关值（nil → true，对齐官方默认）。
func (p memoryPrefs) enabled() bool { return p.Enabled == nil || *p.Enabled }

// config 把偏好映射成 memory.Config。返回的配置里 Enabled 是**显式**值：
// 用户在面板上的选择必须压过默认值，但仍然可以被环境变量
// autoMemoryEnabled 覆盖（memory 包内部的优先级：环境变量 > 用户偏好 > 默认），
// 这样 CI 与调试脚本仍能强制固定行为。
func (p memoryPrefs) config() memory.Config {
	on := p.enabled()
	return memory.Config{Enabled: &on}
}

// memoryConfig 取本次会话装配用的记忆配置。
//
// nil 安全是刻意的：部分测试直接构造 &manager{}（不经 newManager），偏好存储因此
// 为空。此时返回零值 Config = 默认开 —— 与"用户没设置过"同义，而不是让装配期
// 空指针崩掉（崩在 buildLoop 里会伪装成装配不相关的失败，极难定位）。
func (m *manager) memoryConfig() memory.Config {
	if m == nil {
		return memory.Config{}
	}
	return m.memPrefs.load().config()
}
