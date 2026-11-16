package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools"
)

// memory_dream：让模型自己动手做记忆盘点（证据）与归档（执行）。
//
// 为什么需要这个工具。此前"盘点 → 裁决 → 归档"整条链只存在于
// memory/cmd/memory_dream 这个仓库内命令行里：无人值守的会话开在**用户的工作区**，
// 够不着它（没有产品级 CLI 入口，跨目录也跑不了 go run）。于是模型看到了问题也没法处理。
//
// 分工（与 lifecycle.go 的护栏一致）：
//   - 证据由宿主算（锚点 / 转录 / 冲突），模型只负责**判断**；
//   - 归档由宿主执行，模型只提交决策 JSON —— 保护清单、7 天冷却、单次≤2、
//     快照与可恢复这些护栏全在宿主侧，模型绕不过去。
//
// 刻意不提供 delete/merge：这两个动作的回滚语义比"移动 + 摘索引行"复杂得多，
// 等它们被真实跑过再说。
func newMemoryDreamTool(h *memoryHost, workspace string) tools.Tool {
	return &memoryDreamTool{
		BaseTool: tools.BaseTool{
			Name_: "memory_dream",
			Description_: "Inventory project memory and apply the archive decisions you make. " +
				"action=evidence returns the full report (dead code anchors, last-referenced transcript, " +
				"conflict verdicts) as JSON — read it, then submit " +
				"{decisions:{decisions:[{file,action:keep|archive,reason}]}} via action=apply. " +
				"Only archive when the evidence supports it; the host still enforces a protected list, " +
				"a 7-day cooldown and a per-run cap. Archiving is reversible (snapshot is kept); " +
				"deletion is not offered. " +
				"repo 可指定要处理的记忆库（~/.go-code/memory/repos/<repo>）；不给 = 当前工作区那一个。",
			Params_: tools.Obj(map[string]any{
				"action":    tools.Str("evidence = 取证据；apply = 提交裁决并执行"),
				"decisions": tools.Str(`apply 时必填：{"decisions":[{"file":"x.md","action":"keep|archive","reason":"…"}]}`),
				"repo":      tools.Str("可选：记忆库目录名（~/.go-code/memory/repos 下的子目录名）；不给 = 当前工作区的记忆库"),
			}, "action"),
			ReadOnly_: false,
		},
		host:      h,
		workspace: workspace,
	}
}

type memoryDreamTool struct {
	tools.BaseTool
	host      *memoryHost
	workspace string
}

// memoryDir 解析本次要处理的记忆库。
//
// repo 为空 = 当前工作区那一个（既有行为，会话内手工盘点用它）；非空 = 指定
// ~/.go-code/memory/repos/<repo>。后者是**全应用巡检**的必要条件：Dream 派生会话
// 的工作目录是个空目录（见 dreamWorkspaceDir），它自己的记忆库是空的，要遍历
// 全部记忆库就必须能按名字逐个指。
func (t *memoryDreamTool) memoryDir(ctx context.Context, repo string) (*memory.Dir, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		dir, err := t.host.resolve(ctx, t.workspace)
		if err != nil || dir == nil {
			return nil, fmt.Errorf("memory_dream: 取不到记忆目录（%v）", err)
		}
		return dir, nil
	}
	if !validDreamRepoName(repo) {
		return nil, fmt.Errorf("memory_dream: repo 只能是记忆库目录名，收到 %q", repo)
	}
	root, err := memory.ReposRoot()
	if err != nil {
		return nil, fmt.Errorf("memory_dream: 取不到记忆根目录（%v）", err)
	}
	p := filepath.Join(root, repo)
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("memory_dream: 记忆库 %q 不存在（%s）", repo, p)
	}
	return &memory.Dir{ID: repo, Path: p}, nil
}

// validDreamRepoName 记忆库名必须是单段目录名（无分隔符、非 . / ..）。
func validDreamRepoName(repo string) bool {
	if repo == "" || repo == "." || repo == ".." {
		return false
	}
	if strings.ContainsRune(repo, os.PathSeparator) || strings.ContainsRune(repo, '/') {
		return false
	}
	return repo == filepath.Base(repo)
}

func (t *memoryDreamTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		Action    string `json:"action"`
		Decisions string `json:"decisions"`
		Repo      string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	// repo 只认单段目录名：它直接拼进 ~/.go-code/memory/repos/ 之下，
	// 放行 ".." 或分隔符就等于让模型指定任意目录当"记忆库"。
	if a.Repo != "" && !validDreamRepoName(a.Repo) {
		return fmt.Errorf("memory_dream: repo 只能是记忆库目录名，收到 %q", a.Repo)
	}
	switch a.Action {
	case "evidence":
		return nil
	case "apply":
		if a.Decisions == "" {
			return errors.New("memory_dream: apply 需要 decisions")
		}
		return nil
	default:
		return fmt.Errorf("memory_dream: action 只能是 evidence 或 apply，收到 %q", a.Action)
	}
}

func (t *memoryDreamTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a struct {
		Action    string `json:"action"`
		Decisions string `json:"decisions"`
		Repo      string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	dir, err := t.memoryDir(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	cfg := t.host.cfg

	if a.Action == "evidence" {
		raw, err := cfg.DreamEvidence(*dir)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}

	res, err := cfg.ApplyDreamDecisions(*dir, []byte(a.Decisions))
	if err != nil {
		return "", err
	}
	out := "记忆归档：" + res.Summary()
	if len(res.Skipped) > 0 {
		out += "\n跳过原因："
		for i, s := range res.Skipped {
			if i >= 5 {
				out += fmt.Sprintf("\n… 另有 %d 条", len(res.Skipped)-i)
				break
			}
			out += "\n- " + s
		}
	}
	if res.Snapshot != "" {
		out += "\n恢复：memory_dream 支持 --restore（快照保留在 .state/dream/ 下）"
	}
	// 注入是会话创建时拍的快照（WithMemoryInjection 传一次），所以刚归档的条目
	// 仍然列在**当前会话**的 ## Memory 段里。不说这一句，模型会去读一个已经
	// 不存在的文件，然后花几轮排查一个预期内的 ENOENT。
	if len(res.Archived) > 0 {
		out += "\n注意：已归档的条目在**当前会话**的记忆段里仍会出现（注入是会话创建时的快照），" +
			"读到 ENOENT 是预期行为；新建会话后即消失。"
	}
	return out, nil
}
