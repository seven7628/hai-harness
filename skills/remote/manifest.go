package remote

import (
	"encoding/json"
	"os"
	"sync"
)

// Env 安装环境（bridge 提供路径）。
type Env struct {
	AgentSkillsDir  string // 全局 Agent Skills 根（~/.agents/skills，主路径）
	GlobalSkillsDir string // 全局 go-code 技能根（~/.go-code/skills，兼容旧路径）
	ManifestPath    string // 来源清单文件（~/.go-code/remote-skills.json）
	MarketplacePath string // 市场清单文件（~/.go-code/marketplaces.json）
}

// Item 一个已安装远程技能（来源清单条目，支撑 update/uninstall/状态展示）。
type Item struct {
	Name      string `json:"-"`      // 技能名（清单键；List 填充返回）
	Source    string `json:"source"` // 原始 URL（含 #name，update 原样重解析）
	Kind      string `json:"kind"`   // repo|subdir|name
	Ref       string `json:"ref,omitempty"`
	Subdir    string `json:"subdir,omitempty"`
	Scope     string `json:"scope"`               // global|workspace
	Workspace string `json:"workspace,omitempty"` // workspace 作用域时的工作区路径
	Target    string `json:"target,omitempty"`    // 实际安装目录（.agents 主路径 / .go-code 兼容路径；旧条目为空 → 按 scope 回退）
	Installed string `json:"installed"`           // RFC3339
	Commit    string `json:"commit,omitempty"`    // 安装/更新时的远端 HEAD commit（Browse 更新检测；旧条目为空）
}

// Manifest 来源清单（全局 ~/.go-code/remote-skills.json，含各工作区作用域条目）。
type Manifest struct {
	Items map[string]Item `json:"items"` // 技能名 → 条目
}

// manifestMu 序列化清单读写（安装/更新/卸载并发安全）。
var manifestMu sync.Mutex

// loadManifest 读清单（缺文件/坏 JSON → 空清单）。
func loadManifest(env Env) *Manifest {
	m := &Manifest{Items: map[string]Item{}}
	b, err := os.ReadFile(env.ManifestPath)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, m)
	if m.Items == nil {
		m.Items = map[string]Item{}
	}
	return m
}

// saveManifest 写清单（含父目录创建）。
func saveManifest(env Env, m *Manifest) error {
	if dir := dirOf(env.ManifestPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(env.ManifestPath, b, 0o644)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return ""
}
