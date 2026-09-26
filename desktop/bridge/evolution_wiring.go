package main

// evolution_wiring.go —— Memory 自进化控制面（Phase B/C/D）的宿主侧接线。
//
// # 为什么接线是独立文件
//
// evolution 包是**纯控制面**：它不注册工具、不认识 session、也不自己决定任何事。
// 所有会碰到生产记忆的动作都必须经宿主显式调用。接线集中在这里，是为了让
// 「哪些能力被接上了」可以在一处被读、被测试，而不是散落在 main.go 的装配链里。
//
// # 三条接线的边界
//
//  1. Promoter —— **必接**，且必须接 *memory.MemoryService.WriteTopic。
//     evolution.Service.Promote 只写 topics/ 是半状态：索引同步（syncIndexEntry）
//     在 MemoryService 里，不在 Store 里。接错成裸 Store.WriteTopic 会留下
//     「有正文没索引」的条目，那种记忆永远不会被 memory_view 命中。
//     NewService 在 Promoter 缺失时直接构造失败，就是为了让这个错误在装配期暴露。
//
//  2. Replayer —— 默认**不接**，用 evolution 包的确定性结构回放。
//     结构回放零 LLM 调用，因此启动路径没有成本、没有网络依赖、没有抖动。
//     宿主若要真实验证，接 NewLLMReplayer（见 llm_replayer.go），并把
//     RequireReplay 打开——那时「跳过 Replay」就会是硬错误而不是标记。
//
//  3. SkillInstaller —— 默认**不接**。技能落盘要经过 skills.Registry 的分层命名
//     空间（工作区层覆盖全局层），猜错命名空间的后果是「技能装上了但加载不到」
//     或「覆盖了用户自己的技能」。宁可显式不装。

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/seven7628/hai-harness/evolution"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/skills"
)

// buildEvolutionConfig 组装 evolution.Config。
//
// 三个返回值的语义：
//   - cfg 始终非 nil（构造失败时 cfg 为零值，调用方必须看 err）；
//   - mem 为 nil（无 git 身份 / Open 失败）时返回 err，调用方应当**保持禁用**而不是
//     退化成一个没有仓库边界的控制面——那会让候选写到一个共享目录里；
//   - 有真实 LLM Replayer 时由调用方另行注入（依赖 provider，构造点在别处）。
func buildEvolutionConfig(mem *memory.MemoryService) (evolution.Config, error) {
	if mem == nil {
		return evolution.Config{}, fmt.Errorf("evolution: memory service is nil (no repository identity)")
	}
	bound := mem.Bound()
	if bound == nil {
		return evolution.Config{}, fmt.Errorf("evolution: memory service is not bound to a repository")
	}
	return evolution.Config{
		Store: bound.Store,
		// 唯一的晋级通道：走 MemoryService.WriteTopic，索引同步因此不会绕过。
		Promoter: evolution.PromoterFunc{
			Fn: func(topic *memory.Topic) error { return mem.WriteTopic(topic) },
		},
		// Replayer 留空 → 走 evolution 的确定性结构回放（零 LLM 调用）。
		// 理由见文件头「Replayer」条：要真实验证时由宿主显式注入，且同时打开
		// RequireReplay —— 否则「没验证」会被记成 replay_skipped 而不是失败。
		Replayer: nil,
	}, nil
}

// newEvolutionService 为某个工作区构造控制面；不可用时返回 (nil, err)。
//
// 调用方**不应当**因为错误阻断工作区启动：进化控制面是增强能力，缺失它时
// Phase A 的记忆读写必须照常工作。因此这里的约定是「记日志、保持 nil」。
func newEvolutionService(mem *memory.MemoryService) (*evolution.Service, error) {
	cfg, err := buildEvolutionConfig(mem)
	if err != nil {
		return nil, err
	}
	svc, err := evolution.NewService(cfg)
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// —— SkillInstaller（Phase D；显式接，不默认开） ————————————————————————

// newWorkspaceSkillInstaller 构造工作区技能安装器。
//
// 落盘位置刻意选 `<ws>/.go-code/skills/<name>/SKILL.md`：这正是
// buildSkillsRegistry 的第 4 层（工作区层），因此装完即被同一套发现逻辑看到，
// 不需要为「evolution 装的技能」另开一条发现路径。
//
// 边界：**不覆盖**已存在的技能目录。skills.Registry 是「工作区层覆盖全局层」，
// 如果这里静默覆盖一个用户手写的同名技能，负迁移回退时就找不回原版了。
// 同名冲突直接报错，由宿主显式决定换名或先撤销。
func newWorkspaceSkillInstaller(ws string) evolution.SkillInstaller {
	skillsRoot := filepath.Join(ws, skills.GoCodeDirName, "skills")
	return evolution.SkillInstallerFunc{
		InstallFn: func(c *evolution.SkillCandidate) (string, error) {
			if c == nil || c.Name == "" {
				return "", fmt.Errorf("skill candidate has no name")
			}
			dir := filepath.Join(skillsRoot, c.Name)
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil {
				return "", fmt.Errorf("skill %q already exists at %s; refusing to overwrite", c.Name, dir)
			} else if !os.IsNotExist(err) {
				return "", err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(c.SKILLMD()), 0o600); err != nil {
				return "", err
			}
			return dir, nil
		},
		RemoveFn: func(name string) error {
			dir := filepath.Join(skillsRoot, name)
			// 只删本安装器自己写的那一层，不递归删用户目录。
			return os.RemoveAll(filepath.Join(dir, "SKILL.md"))
		},
	}
}

// evolutionSkillInstallerFor 按工作区取安装器（供接线路径与测试共用同一构造）。
func evolutionSkillInstallerFor(ws string) evolution.SkillInstaller {
	return newWorkspaceSkillInstaller(ws)
}
