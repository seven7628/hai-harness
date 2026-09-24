// loader.go —— 分层配置的**有状态加载器**：逐层保留「上次有效」快照。
//
// 为什么需要它（文件损坏的恢复语义）：配置文件由多方写入——用户在编辑器里保存、
// 应用写盘（settings.json）、外部脚本改写。任何一次「半写 / 坏 JSON / 暂时读不到」
// 如果被当成「该层为空」，后果是该层全部服务器从工具面消失（连接被关），等文件修好
// 才回来。更糟的是故障静默：用户只看到服务器没了，不知道为什么。
//
// 本文件的规则（简单可讲清）：**有效 JSON 才生效；坏 JSON / 空文件 / 读失败 → 沿用
// 上次有效内容，并在 LayerState 里报 stale + 原因**；文件恢复有效后自动切回。
// 层被删除（ENOENT）视为有意为之 → 清掉 last-good，该层贡献空。
// 单条目校验失败（如未知 type）不算层损坏：跳过该条并在 Skipped 里列出（其余照常生效）。
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// LayerState 单层加载结果（观测/排障 → bridge mcp_list 的 layers 字段）。
// Present 与 OK 正交：Present = 文件读到了内容（好坏都算）；OK = 本次内容有效可用。
type LayerState struct {
	Path      string    `json:"path"`
	Source    string    `json:"source"`
	Present   bool      `json:"present"`              // 文件存在且读到了内容
	OK        bool      `json:"ok"`                   // 本次内容有效（解析成功）
	Stale     bool      `json:"stale"`                // 沿用上次有效内容（当前文件坏/半写）
	Err       string    `json:"error,omitempty"`      // 解析/读取失败原因（stale 时同时给出）
	Servers   int       `json:"servers"`              // 该层贡献的服务器数（stale = 上次有效值）
	Skipped   []string  `json:"skipped,omitempty"`    // 校验失败被跳过的条目名（坏条目可见）
	UpdatedAt time.Time `json:"updated_at,omitempty"` // 上次有效内容的读取时刻
}

// LoadResult 一次分层加载的合并结果。
type LoadResult struct {
	Config map[string]ServerConfig // 合并配置（低 → 高优先级，同名后层覆盖）
	Source map[string]string       // 每个服务器名的胜出来源标签
	Layers []LayerState            // 逐层状态（含 stale/err；顺序同入参）
}

// Loader 分层配置加载器：逐层保留上次有效快照，坏文件/半写不静默清空该层。
// 零值可用（首次即坏 → 该层为空，Err 可见）。
//
// 并发安全：Load* 内部持锁——文件监听的重载 goroutine 与 mcp_set 热应用会并发调用。
type Loader struct {
	mu   sync.Mutex
	last map[string]layerSnapshot // path → 上次有效内容
}

// layerSnapshot 某层「上次有效」的内容快照。
type layerSnapshot struct {
	cfg       map[string]ServerConfig
	skipped   []string
	updatedAt time.Time
}

// Load 加载分层（低 → 高优先级，同名后层覆盖前层）并合并。
func (l *Loader) Load(layers ...Layer) LoadResult {
	return l.LoadWithBase(nil, "", layers...)
}

// LoadWithBase 在 base（最低优先级，如 UI 刚保存、尚未落盘的用户层）之上叠加分层。
// base 不做 last-good（它是内存里的显式输入）：非法项跳过，与 Merge 同语义。
func (l *Loader) LoadWithBase(base map[string]ServerConfig, baseSource string, layers ...Layer) LoadResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]layerSnapshot{}
	}
	out := make(map[string]ServerConfig, len(base))
	src := make(map[string]string, len(base))
	for name, c := range base {
		if strings.TrimSpace(name) == "" {
			continue
		}
		norm, err := c.Validate()
		if err != nil {
			continue // 非法 base 项跳过（与 Merge 一致）
		}
		out[name] = norm
		src[name] = baseSource
	}
	states := make([]LayerState, 0, len(layers))
	for _, ly := range layers {
		st, snap := l.loadLayer(ly)
		states = append(states, st)
		for name, c := range snap.cfg {
			out[name] = c
			src[name] = ly.Source
		}
	}
	if len(out) == 0 {
		out, src = nil, nil
	}
	return LoadResult{Config: out, Source: src, Layers: states}
}

// loadLayer 读 + 解析单层并做 last-good 决策。调用方须持 l.mu。
func (l *Loader) loadLayer(ly Layer) (LayerState, layerSnapshot) {
	st := LayerState{Path: ly.Path, Source: ly.Source}
	if strings.TrimSpace(ly.Path) == "" {
		st.OK = true // 空路径 = 该层未配置（不算错误）
		return st, layerSnapshot{}
	}
	raw, err := os.ReadFile(ly.Path)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件被删除 = 有意为之：清掉 last-good，该层贡献空（区别于「读坏」）。
			delete(l.last, ly.Path)
			st.OK = true
			return st, layerSnapshot{}
		}
		st.Present = true // 非 ENOENT（权限/IO）：文件在，但读不到
		return l.staleLayer(st, fmt.Sprintf("读取失败: %v", err)), l.last[ly.Path]
	}
	st.Present = true
	// 空/纯空白内容：视为半写（写盘先截断后写入的窗口）→ 沿用上次有效。
	// 想清空某层请写 `{}`（有效 JSON = 有意为之）。
	if strings.TrimSpace(string(raw)) == "" {
		return l.staleLayer(st, "内容为空（视为半写，沿用上次有效；清空该层请写 {}）"), l.last[ly.Path]
	}
	var v struct {
		MCPServers map[string]ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return l.staleLayer(st, fmt.Sprintf("JSON 解析失败: %v", err)), l.last[ly.Path]
	}
	cfg := make(map[string]ServerConfig, len(v.MCPServers))
	skipped := make([]string, 0)
	for name, c := range v.MCPServers {
		if strings.TrimSpace(name) == "" {
			continue
		}
		norm, err := c.Validate()
		if err != nil {
			skipped = append(skipped, name) // 坏条目跳过（层本身有效），名字可见
			continue
		}
		cfg[name] = norm
	}
	snap := layerSnapshot{cfg: cfg, skipped: skipped, updatedAt: time.Now()}
	l.last[ly.Path] = snap
	st.OK = true
	st.Servers = len(cfg)
	st.Skipped = skipped
	st.UpdatedAt = snap.updatedAt
	return st, snap
}

// staleLayer 构造「沿用上次有效内容」的层状态（无 last-good 时该层贡献空，原因仍可见）。
func (l *Loader) staleLayer(st LayerState, reason string) LayerState {
	st.OK = false
	st.Err = reason
	if snap, ok := l.last[st.Path]; ok {
		st.Stale = true
		st.Servers = len(snap.cfg)
		st.Skipped = snap.skipped
		st.UpdatedAt = snap.updatedAt
	}
	return st
}
