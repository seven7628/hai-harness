package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Marketplace 一个已保存的远程技能市场（~/.go-code/marketplaces.json）。
// 保存市场 URL 免去每次手填；浏览/安装/按市场更新技能都以它为入口。
type Marketplace struct {
	Name    string `json:"name"`     // 仓库名（URL 末段，展示用）
	URL     string `json:"url"`      // 用户输入的原始 URL（浏览/更新时原样使用）
	AddedAt string `json:"added_at"` // RFC3339
}

// marketplaceManifest 市场清单文件结构。
type marketplaceManifest struct {
	Marketplaces []Marketplace `json:"marketplaces"`
}

// loadMarketplaces 读市场清单（缺文件/坏 JSON → 空）。
func loadMarketplaces(env Env) *marketplaceManifest {
	m := &marketplaceManifest{Marketplaces: []Marketplace{}}
	if env.MarketplacePath == "" {
		return m
	}
	b, err := os.ReadFile(env.MarketplacePath)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, m)
	if m.Marketplaces == nil {
		m.Marketplaces = []Marketplace{}
	}
	return m
}

func saveMarketplaces(env Env, m *marketplaceManifest) error {
	if dir := dirOf(env.MarketplacePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(env.MarketplacePath, b, 0o644)
}

// normalizeURL 把用户输入归一化为克隆 URL（github.com/a/b 与 https://github.com/a/b.git 视为同一市场）。
func normalizeURL(raw string) string {
	if src, err := ParseURL(raw); err == nil {
		return src.URL
	}
	return strings.TrimSpace(raw)
}

// AddMarketplace 保存一个市场 URL（幂等：已存在则直接返回；不克隆校验，浏览/安装时再暴露问题）。
// 市场须为仓库 URL（整仓库），不支持 #name 定向。
func AddMarketplace(rawURL string, env Env) (Marketplace, error) {
	src, err := ParseURL(rawURL)
	if err != nil {
		return Marketplace{}, err
	}
	if src.Kind == "name" || src.Kind == "subdir" {
		return Marketplace{}, errors.New("市场需为仓库 URL（如 github.com/owner/repo），不支持 #name 定向")
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()
	mm := loadMarketplaces(env)
	for _, mk := range mm.Marketplaces {
		if normalizeURL(mk.URL) == src.URL {
			return mk, nil // 幂等
		}
	}
	mk := Marketplace{Name: marketName(src.URL), URL: strings.TrimSpace(rawURL), AddedAt: time.Now().Format(time.RFC3339)}
	mm.Marketplaces = append(mm.Marketplaces, mk)
	if err := saveMarketplaces(env, mm); err != nil {
		return Marketplace{}, fmt.Errorf("市场清单写盘失败: %w", err)
	}
	return mk, nil
}

// RemoveMarketplace 移除已保存市场（按 URL 归一化匹配）。
func RemoveMarketplace(rawURL string, env Env) error {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	mm := loadMarketplaces(env)
	// 注意：不能用 mm.Marketplaces[:0] 复用底层数组——saveMarketplaces 存 mm 原长度会残留已删项
	var out []Marketplace
	target := normalizeURL(rawURL)
	for _, mk := range mm.Marketplaces {
		if normalizeURL(mk.URL) == target {
			continue
		}
		out = append(out, mk)
	}
	mm.Marketplaces = out
	return saveMarketplaces(env, mm)
}

// ListMarketplaces 已保存市场（保持添加顺序；UI 展示）。
func ListMarketplaces(env Env) []Marketplace {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	mm := loadMarketplaces(env)
	out := make([]Marketplace, len(mm.Marketplaces))
	copy(out, mm.Marketplaces)
	return out
}

// IsMarketSaved 是否已保存该市场（bridge/前端冲突判断）。
func IsMarketSaved(rawURL string, env Env) bool {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	target := normalizeURL(rawURL)
	for _, mk := range loadMarketplaces(env).Marketplaces {
		if normalizeURL(mk.URL) == target {
			return true
		}
	}
	return false
}

// UpdateMarket 按市场更新该市场全部已安装技能（来源 URL 归一化匹配清单条目，含 #name 安装）。
// 逐个走 Update（重拉最新 ref → 校验同名 → 替换）。返回更新的技能名；全失败报错，部分失败带列表。
func UpdateMarket(ctx context.Context, marketURL string, env Env) ([]string, error) {
	target, err := ParseURL(marketURL)
	if err != nil {
		return nil, err
	}
	// 注意：manifestMu 不可重入——先收集匹配名，释放锁后再逐个 Update（Update 内部重新加锁）
	manifestMu.Lock()
	man := loadManifest(env)
	var names []string
	for name, it := range man.Items {
		src, err := ParseURL(it.Source)
		if err != nil {
			continue
		}
		if src.URL == target.URL {
			names = append(names, name)
		}
	}
	manifestMu.Unlock()

	if len(names) == 0 {
		return nil, fmt.Errorf("市场 %s 下没有已安装技能（先浏览并从市场安装）", marketURL)
	}
	var updated []string
	var errs []string
	for _, name := range names {
		if err := Update(ctx, name, env); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		updated = append(updated, name)
	}
	if len(updated) == 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		return updated, fmt.Errorf("部分成功（%d/%d）：%s", len(updated), len(names), strings.Join(errs, "; "))
	}
	return updated, nil
}

// marketName 从克隆 URL 取仓库名（github.com/owner/repo.git → repo；git@github.com:owner/repo → repo）。
func marketName(cloneURL string) string {
	s := strings.TrimSuffix(cloneURL, ".git")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
