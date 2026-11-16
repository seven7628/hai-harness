package memory

import (
	"encoding/json"
	"fmt"
)

// DreamTool* 是"模型自己动手"这条路的数据面：给证据、收裁决。
//
// 为什么需要它：无人值守的会话开在用户的工作区里，够不着 go-code 仓库里的
// `memory/cmd/memory_dream`（没有产品级 CLI 入口，跨目录也跑不了 go run）。
// 而归档必须有护栏（保护清单 / 7 天 / 单次≤2 / 快照 / 可恢复）——让模型自己
// mv 文件等于绕过全部护栏。所以裁决交给模型，**执行留在宿主**：模型读到证据、
// 给出决策 JSON，由这里带护栏落盘。

// DreamEvidence 导出裁决所需的全部证据（只读）。
//
// 形态与 `memory_dream --evidence` 一致：一个可直接读的 JSON 对象。
// 刻意不裁剪：让模型自己决定看哪几段，比宿主替它筛更可靠——宿主筛掉的
// 往往正是"为什么这条该走"的那一条。
func (c Config) DreamEvidence(d Dir) ([]byte, error) {
	c = c.withDefaults()
	rep, err := c.Dream(d)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"repo":         rep.Repo,
		"path":         rep.Path,
		"topics":       rep.Topics,
		"not_indexed":  rep.NotIndexed,
		"stale":        rep.Stale,
		"missing_type": rep.MissingType,
		"duplicates":   rep.Duplicates,
		"never_read":   rep.NeverRead,
		"anchors": map[string]any{
			"unbound": rep.Anchors.Unbound,
			"refs":    rep.Anchors.Refs,
			"dead":    rep.Anchors.Dead,
		},
		"transcripts": rep.Transcripts,
		"conflicts":   rep.Conflicts,
		"how_to_decide": "每条给 {file, action: keep|archive, reason}。" +
			"archive 必须基于证据（锚点失效 / 被后续会话推翻 / 有明确继任者），" +
			"不确定就 keep。单次归档额度由宿主按库的大小给（小库 2 条，大库最多 5 条），" +
			"超出额度的会被裁掉——所以宁可少提几条，也不要把 keep 凑成 archive。" +
			"宿主还会按保护清单与 7 天冷却再筛一遍。",
	}
	return json.MarshalIndent(payload, "", " ")
}

// ApplyDreamDecisions 解析模型给的裁决并带护栏执行（归档 / 保留）。
//
// 解析失败或没有任何 archive 决策时不落盘任何东西——只有 keep 不需要动文件。
func (c Config) ApplyDreamDecisions(d Dir, raw []byte) (ApplyResult, error) {
	c = c.withDefaults()
	decs, err := ParseDecisions(raw)
	if err != nil {
		return ApplyResult{}, err
	}
	var archive int
	for _, dec := range decs {
		if dec.Action == ActionArchive {
			archive++
		}
	}
	if archive == 0 {
		return ApplyResult{Skipped: []string{"本次裁决没有 archive，未改动任何文件"}}, nil
	}
	res, err := c.ApplyDecisions(d, decs)
	if err != nil {
		return res, err
	}
	// 记账：自动路径每天最多一次，靠的就是这条。
	_ = c.MarkAutoDream(d, len(res.Archived))
	return res, nil
}

// ApplyResult 摘要（给工具结果用，避免把整个结构体塞进上下文）。
func (r ApplyResult) Summary() string {
	s := fmt.Sprintf("归档 %d 条", len(r.Archived))
	if len(r.Archived) > 0 {
		s += "：" + joinList(r.Archived)
	}
	if r.Snapshot != "" {
		s += "；快照 " + r.Snapshot
	}
	if len(r.Skipped) > 0 {
		s += "；跳过 " + fmt.Sprint(len(r.Skipped)) + " 条"
	}
	return s
}

func joinList(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "、"
		}
		out += x
	}
	return out
}
