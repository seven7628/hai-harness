package remote

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/seven7628/hai-harness/skills"
)

// BrowseItem 一个可浏览的远程技能（市场浏览结果；只含元数据，不含指令正文）。
type BrowseItem struct {
	Name            string // 技能名（= 安装目录名）
	Description     string // SKILL.md frontmatter description
	Installed       bool   // 是否已安装（按名匹配，任一作用域）
	UpdateAvailable bool   // 已安装且同源仓库的 HEAD commit 落后于本地记录 → 可更新
	Kind            string // repo|subdir|name（来源形式）
}

// Browse 浏览远程仓库技能（只读）：git 克隆 → 解析候选 → 逐个读元数据（坏技能跳过记日志，
// 不中断整体）→ 用来源清单标记已安装，并按清单 commit vs 远端 HEAD 判可更新。克隆到临时
// 目录，返回后即清理，绝不写盘、绝不安装。
//   - 整仓库 / tree 子目录 / #name：返回该（些）技能；
//   - 市场仓库（.claude-plugin/marketplace.json）：返回全部可选技能，UI 可逐个选装；
//   - #name 未找到 → resolveSkills 报错；全部候选都是坏技能 → 报错。
func Browse(ctx context.Context, rawURL string, env Env) ([]BrowseItem, error) {
	src, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	staging, err := clone(ctx, src)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	head, err := headCommit(ctx, staging)
	if err != nil {
		return nil, err
	}

	cands, err := resolveSkills(staging, src)
	if err != nil {
		return nil, err
	}

	manifestMu.Lock()
	man := loadManifest(env)
	manifestMu.Unlock()

	var items []BrowseItem
	for _, c := range cands {
		sk, err := skills.ParseSkill(c.dir)
		if err != nil {
			// 坏技能跳过记日志，不中断整体浏览（市场里单个损坏不影响其余可选技能）
			log.Printf("browse: 跳过损坏技能 %s: %v", c.dir, err)
			continue
		}
		installedItem, installed := man.Items[sk.Name]
		// 更新检测：仅同源仓库（归一化克隆 URL 相等）且本地 commit 落后于远端 HEAD → 可更新。
		// 旧条目（Commit 空）或不同来源安装的同名技能 → 视为已安装·最新，不从该市场提示更新。
		updateAvailable := false
		if installed {
			if is, err := ParseURL(installedItem.Source); err == nil && is.URL == src.URL {
				updateAvailable = installedItem.Commit != "" && installedItem.Commit != head
			}
		}
		items = append(items, BrowseItem{Name: sk.Name, Description: sk.Description, Installed: installed, UpdateAvailable: updateAvailable, Kind: src.Kind})
	}
	if len(items) == 0 {
		return nil, errors.New("未找到可浏览的技能")
	}
	return items, nil
}
