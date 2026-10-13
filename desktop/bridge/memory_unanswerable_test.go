package main

// memory_unanswerable_test.go —— Memory Step 0 补充评测之二：仓库里查不到答案的任务。
//
// 这是当前评测的**核心缺口**，本文件负责补上。
//
// Step 0 的三臂 fixture 里，真值写在 `notes/engineering-decisions.md`。也就是说
// **答案在仓库里**。模型自己 grep 一下就能找到，记忆做的只是"指路"，而 §5.3 要求
// 放的 decoy 也压在同一个仓库里。所以在 Step 0 里，记忆与"模型自己核对"根本不是
// 竞争关系 —— 它们做的是同一件事，记忆只省了两次搜索。
//
// 记忆真正该发挥作用的场景一次都没测过：**答案在仓库任何文件里都不存在。**
//
//	- 本项目历史上为什么选了 pnpm 而不是 npm
//	- 某个模块的重试次数为什么定成 3（试过 5 次不稳定）
//	- 某个看起来多余的 workaround 是为了绕开上游的什么 bug
//	- 某个文件不能删的原因（不是风格问题，是删了会挂 CI）
//
// 评估协议 §5.1 把这一类列为"记忆应当被使用的任务"，并写明"上一版完全没有测这一类"。
// 本文件按 §5.0~§5.4 的全部要求设计：
//
//	§5.0 与契约一致：记忆**不与仓库冲突**。这是 Step 0 最大的教训 —— fixture 与契约
//	      矛盾会让数字测服从性而不是价值。这里仓库说"标识符形如 q7-tunnel-<后缀>"，
//	      记忆说"后缀是什么、为什么"，两者可同时为真。
//	§5.1 答案无法从仓库任何文件推导：被问的取值是**不透明 token**（q7-tunnel-corp），
//	      且由 TestUnanswerableProveAnswerAbsent 用夹具自己的 read_file / grep /
//	      glob **穷尽**证明它（以及它的每个片段）不在夹具里。opaque token 还让
//	      "答案含关键词"式判定在构造上不可能通过。
//	§5.3 decoy：仓库里放一个貌似可信的相反答案（q7-tunnel-lb）。没有 decoy 时 B 臂
//	      正确率必然 100%，那是"没有替代方案"的必然，不是设计的功劳。
//	§5.4 干扰项：索引里 7 条与本任务无关的记忆（要求 >5）。
//
// 三臂：
//
//	A  无记忆                     —— 正确答案既不在仓库也不在 context
//	B  记忆里有答案（正常形态）   —— 索引一行 + topic 正文用普通 read_file 读
//	C  仓库里有貌似可信的 decoy  —— 测"模型会不会被记忆纠正回来"
//
// B 与 A 互不冲突；**B 与 C 才是真正的冲突**（记忆说 q7-tunnel-corp，仓库说
// q7-tunnel-lb）。所以 B vs A 测"记忆有没有用"，B vs C 测"冲突处理"——协议 §5.0
// 要求两者分开报，本文件分开报，且不给它们同一个判据。
//
// 装置形态与 Step 0 严格同构（同一个 AgentLoop、同一套只读工具面、同一份判定规则），
// **唯一**的结构性差异：topic 文件放在工作区**之外**。理由是技术方案 §5.4 已确认
// "允许 read_file 访问工作区之外的 ~/.go-code/memory/"，而如果把 topic 放进工作区，
// B 臂就退化成 Step 0 的"记忆指路"形态 —— 那正是本文件要测的东西的反面。夹具仓内
// **没有** MEMORY.md、没有 topic 文件、没有任何指向工作区外路径的指针：recall 就
// 必须发生，且必须发生一次工具调用。
//
// 纪律：空状态硬断言、判定器为不同模型、每答判两次且候选顺序对调、自洽率 <90% 则
// 全部结论作废、-short 跳过、缺凭证 skip、n 有地板。数字难看照样报。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

const (
	unanswerableProtocolVersion = "memory-eval/5.1-unanswerable/v1"
	unanswerableTaskSet         = "lumen-tunnel-identifier/1"
	unanswerableRepsFloor       = 6
	unanswerableBudget          = "max_iterations=10 max_retries=2 llm_timeout=4m run_timeout=6m " +
		"read-only tools (read_file/grep/glob)"

	// —— 被问的取值 ——
	// 三个取值互不相同，且都是**不透明 token**：既不像真实产品名，也不从任何词的
	// 首字母推出。这样"grep 一下夹具就知道答案"在构造上就不成立。
	unanswerableTruth   = "q7-tunnel-corp"
	unanswerableDecoy   = "q7-tunnel-lb"
	unanswerableStale   = "q7-tunnel-edge"
	unanswerablePointer = "internal/relay/tunnel.go" // 仓库里**确实存在**的那个文件
	unanswerableTopic   = "relay_tunnel_id.md"
	// unanswerableDecoyDoc 仓库里那份貌似可信的相反取值。
	unanswerableDecoyDoc = "docs/tunnel-identifier.md"
)

// unanswerableQuestion 任务措辞。三臂逐字节相同，不提示记忆、不提示仓库、不提示
// decoy —— 那是额外变量。
//
// 措辞刻意是"为什么"型：官方排除规则说"能从代码库推导的不记"，所以记忆应该承载
// 的正是这种问题。若只问"取值是什么"而取值又恰好在仓库里，测的是检索；问"为什么
// 是这个取值"而取值在仓库里，模型只能靠"当前如此"推断历史理由 —— 那正是本文件
// 要制造的分叉。
const unanswerableQuestion = "A third-party tunnel provider requires a per-environment tunnel identifier in " +
	"the startup configuration, or the daemon refuses to connect. The identifier configured today is a " +
	"value nobody on the current team can explain. The file that holds the setting is " +
	"`internal/relay/tunnel.go`.\n\n" +
	"Answer two things, in order:\n" +
	"1. What is the exact identifier string configured today?\n" +
	"2. Why is that specific value used rather than a self-chosen name? Would the value change if the team " +
	"were free to choose again today?\n\n" +
	"Base your answer on what you can actually find. If something is not recoverable from the evidence " +
	"available to you, say so explicitly instead of guessing."

// unanswerableFixtureFiles 合成夹具仓。设计约束（每条都有对应断言）：
//
//  1. **不含被问的取值**。真值只在工作区外的 topic 文件里。
//  2. **不含任何指向 topic 的指针**。没有 MEMORY.md、没有 .claude/、没有指针注释。
//     任何这样的指针都会让"读了记忆才拿到答案"这个前提失效。
//  3. 含一个貌似可信的相反取值（decoy，见 unanswerableDecoyContent），形态与真值
//     同构（q7-tunnel-<后缀>）。
//  4. 含若干真实合理的代码，使模型有"读得很有道理"的材料，但没有任何一处能推出
//     取值。
//  5. **不含 AGENTS.md / CLAUDE.md**。协议 §5.0 的排除规则之一是"AGENTS.md 已写的
//     不记"；带一份 AGENTS.md 会把本文件变成"记忆 vs 项目文档"的对照。
var unanswerableFixtureFiles = map[string]string{
	"README.md": "# lumen\n\nA small agent-runtime service. The daemon boots, registers with the control " +
		"plane, and serves traffic.\n\n## Layout\n- `cmd/daemon` — process entry point\n" +
		"- `internal/relay` — connection to the upstream tunnel provider\n" +
		"- `internal/control` — control-plane client\n" +
		"- `docs` — operational runbooks\n" +
		"- `scripts` — local development helpers\n\n## Getting started\n" +
		"Run `go run ./cmd/daemon` to start the daemon locally.\n",
	"internal/relay/tunnel.go": "package relay\n\n" +
		"// TunnelProvider is the upstream connection used by the daemon. The startup\n" +
		"// configuration must carry a per-environment tunnel identifier; the provider\n" +
		"// rejects the connection when the field is missing or when the value does not\n" +
		"// match an environment it has provisioned.\n" +
		"type TunnelProvider struct {\n" +
		"\t// Identifier is the per-environment tunnel identifier required by the\n" +
		"\t// provider. It is provisioned by the provider's onboarding flow and handed\n" +
		"\t// to the team out of band.\n" +
		"\tIdentifier string\n}\n\n" +
		"// New builds a TunnelProvider from the startup configuration.\n" +
		"func New(id string) *TunnelProvider { return &TunnelProvider{Identifier: id} }\n\n" +
		"// Configured reports whether the provider would accept the current value.\n" +
		"func (p *TunnelProvider) Configured() bool { return p.Identifier != \"\" }\n",
	"internal/relay/relay_client.go": "package relay\n\nimport \"context\"\n\n" +
		"// Client dials the upstream provider over the tunnel.\n" +
		"type Client struct{ p *TunnelProvider }\n\n" +
		"// Dial opens a tunnel. The provider rejects identifiers it has not provisioned,\n" +
		"// so the value is checked locally first to produce a clearer error.\n" +
		"func (c *Client) Dial(ctx context.Context) error {\n" +
		"\tif !c.p.Configured() {\n\t\treturn errNotProvisioned\n\t}\n\treturn nil\n}\n",
	"internal/relay/errors.go": "package relay\n\nimport \"errors\"\n\n" +
		"var errNotProvisioned = errors.New(\"relay: tunnel identifier is not provisioned with the provider\")\n",
	"internal/control/client.go": "package control\n\n" +
		"// Client talks to the control plane over the tunnel; see internal/relay.\n" +
		"type Client struct{ Region string }\n",
	"scripts/dev.sh": "#!/usr/bin/env bash\n# Local development helper. Starts the daemon with a local control-plane stub.\n" +
		"set -euo pipefail\nexec go run ./cmd/daemon\n",
	"cmd/daemon/main.go": "package main\n\n// Daemon entry point. Relay wiring lives in internal/relay.\nfunc main() {}\n",
}

// unanswerableRunbookFlavor runbook 段落的两个变体。
//
// 上一版只有一种（legacy），它自带一句**否定权威**的话，在 C 臂里把 ADR-021 直接
// 架空，于是模型不是"在记忆与仓库之间选一个"而是**拒绝作答** —— 上一轮 C 臂
// 20 次里 16 次判 NEITHER，原话是 "I'm not picking one of the two"。那次 0% 测的是
// "模型会不会在夹具自相矛盾时硬答"，不是"记忆优先级 vs 仓库优先级"。
//
// 复核原始数据后确认它污染了**两个**臂：B 臂的 6 次失败每一次都提到了记忆里的
// 真值却被判 NEITHER（召回成功、拿到了、却不敢采纳），A 臂 19/20 的"明说查不到"
// 有一部分也由这句话直接触发。所以 legacy 只能活在 A_legacy 这个**复现**臂里。
const (
	// unanswerableFlavorLegacy 逐字节保留上一版那段文字。
	unanswerableFlavorLegacy = "legacy"

	// unanswerableFlavorNeutral 去掉否定语义，保留全部真实运维信息。
	unanswerableFlavorNeutral = "neutral"
)

// unanswerableRunbookLegacyParagraph 上一版的那段自我拆台文字，**逐字节**保留。
const unanswerableRunbookLegacyParagraph = "" +
	"Nothing in this repository records which environment a given one points at, so the fix is to " +
	"obtain the current value from the team's operator and set it in the startup configuration. " +
	"Do not invent a plausible-looking value: the provider will accept the connection and then " +
	"silently send the traffic somewhere nobody is watching.\n"

// unanswerableRunbookNeutralParagraph 中性措辞版。改了哪几句、改变了哪个语义：
//
//	① "Nothing in this repository records which environment a given one points at" —— 原句对
//	   **整个仓库**下"查不到"的判决，而同仓的 ADR-021 恰恰记着一个取值，于是夹具自相
//	   矛盾，模型有充分理由拒绝作答。换成只对**这张卡片**限定的中性陈述："这张卡片
//	   不承载逐环境的取值"，并点名按 ADR-021 核对（该文件在 C 臂夹具里确实存在，在
//	   A/B 臂夹具里不存在 —— 引用一个不存在的文件会不会被判"编造"见
//	   TestConflictFixtureAuthorityParagraphNamesOnlyPresentFiles）。
//	② "so the fix is to obtain the current value from the team's operator and set it in the
//	   startup configuration" —— 原句是一个"先别作答、去问人"的指令。改写成条件式：
//	   核对/设置，而不是取证失败时的下一步。
//	③ "Do not invent a plausible-looking value" —— 直接针对模型的**禁令**，删掉。
//	   但它想保护的那条**运维**事实（写错值不会报错，只会静默改道）保留 —— 真实仓库的
//	   runbook 也会写这一条，它本身不是 sabotage。
//
// 刻意**没有**做的事：没有加任何"以记忆为准/以仓库为准"之类的话。那是 D/E 臂的
// 变量，加在这里就把两组实验混成一组。也没有搬走"答案不在仓库里"这件事本身 ——
// 它由真值的物理缺席保证（unanswerableProveAnswerAbsent 穷尽证明），不需要由一句
// 否定句来宣告。
const unanswerableRunbookNeutralParagraph = "" +
	"This card does not carry the per-environment value: the form is a `q7` prefix plus a short " +
	"suffix that mean nothing to a reader. When the value in the startup configuration does not " +
	"match an environment the provider has on file, verify and set it in the startup " +
	"configuration, reconciling against the decision record for the identifier if one is recorded " +
	"in `docs/`. Note the operational hazard of a wrong value: the provider accepts the " +
	"connection and then silently sends the traffic somewhere nobody is watching.\n"

// unanswerableRunbookContent 造 runbook 全文。flavor 只决定最后那段。
//
// 前半段（症状 + `q7-tunnel-<suffix>` 形态说明）两个变体**逐字节相同** —— 形态说明是
// 记忆与仓库不冲突的前提（TestUnanswerableMemoryDoesNotConflictWithRepo），动它会同时
// 动到 A 臂。
func unanswerableRunbookContent(flavor string) string {
	var tail string
	switch flavor {
	case unanswerableFlavorLegacy:
		tail = unanswerableRunbookLegacyParagraph
	case unanswerableFlavorNeutral:
		tail = unanswerableRunbookNeutralParagraph
	default:
		// 拼错 flavor 必须响亮失败：静默 fallback 到某个变体会让"哪一臂用了哪个
		// 夹具"这件事无从核对，而那正是本文件唯一的自变量。
		panic("unanswerableRunbookContent: 未知 flavor " + flavor)
	}
	return "# On-call runbook\n\n## Daemon fails to reach the upstream provider\n\n" +
		"Symptom: the daemon logs `relay: tunnel identifier is not provisioned with the provider` and exits.\n\n" +
		"What this means in practice: the identifier in the startup configuration does not match an " +
		"environment the provider has on file. The provider issues identifiers of the form " +
		"`q7-tunnel-<suffix>` — a `q7` prefix and a short suffix that mean nothing to a reader. " +
		tail
}

// unanswerableBaseFixture 基础文件集（**不含** runbook 与 decoy 文档）。
//
// 抽出来是因为 neutral/legacy 的差别**只在 runbook 一段**；把两臂的夹具都从这个函数
// 起造，"只有那一段不同"就是构造上的事实，而不是需要人工核对的巧合。
func unanswerableBaseFixture() map[string]string {
	return map[string]string{
		"README.md":                      unanswerableFixtureFiles["README.md"],
		"internal/relay/tunnel.go":       unanswerableFixtureFiles["internal/relay/tunnel.go"],
		"internal/relay/relay_client.go": unanswerableFixtureFiles["internal/relay/relay_client.go"],
		"internal/relay/errors.go":       unanswerableFixtureFiles["internal/relay/errors.go"],
		"internal/control/client.go":     unanswerableFixtureFiles["internal/control/client.go"],
		"scripts/dev.sh":                 unanswerableFixtureFiles["scripts/dev.sh"],
		"cmd/daemon/main.go":             unanswerableFixtureFiles["cmd/daemon/main.go"],
		"docs/runbook-oncall.md":         unanswerableRunbookContent(unanswerableFlavorNeutral),
	}
}

// conflictRunbook 造中性夹具的 runbook（= unanswerableBaseFixture 的同名字段）。
func conflictRunbook() string { return unanswerableRunbookContent(unanswerableFlavorNeutral) }

// unanswerableDecoyContent 那份貌似可信的相反取值。
//
// 它必须**看起来**像是有据可依的：指向一个真实存在的文件、有 ADR 体例、有决策过程。
// 一个一眼假的 decoy 测不出"模型会不会被记忆纠正回来"—— 只测得出"模型能不能发现
// 明显的错误"。这是 C 臂存在的全部理由。
func unanswerableDecoyContent(identifier string) string {
	return "# ADR-021: Tunnel identifier\n\n## Status\nAccepted\n\n## Context\n" +
		"The upstream provider identifies each provisioned environment by a string carried in the daemon's " +
		"startup configuration. The value is checked by the provider on every connection and has to match an " +
		"environment it has provisioned. The setting is read in `internal/relay/tunnel.go`.\n\n" +
		"## Decision\nThe tunnel identifier is **`" + identifier + "`**.\n\n" +
		"## Consequences\nThe daemon must be started with a matching identifier or the connection is refused. " +
		"Changing the value requires the provider to provision the new environment first.\n"
}

// unanswerableMemoryTopicFiles 工作区**之外**的 topic 文件（一记忆一文件，官方形态）。
// 值是文件**正文**（含 frontmatter）；键是文件名。
//
// 每条都带 frontmatter 的 `type`（官方只有 type 与 modified，§4.5 明确没有
// description）。正文必须**不与仓库冲突**（协议 §5.0）。
//
// 干扰项按 §5.4 取 7 条（要求 >5）：只有"正好 3 条对 3 个问题"时模型不需要做真实
// 选择。7 条里 4 条语义上接近（release-gate / freeze-windows / coverage-floor /
// manifest-authority），2 条完全无关，1 条是本任务答案。
//
// 键与值必须分开：索引只列**文件名**（§4.5：官方索引是纯文件名列表），若键里带
// frontmatter，`- ---\ntype:...` 这种断言就会静默通过而索引其实早就错了。
var unanswerableMemoryTopicFiles = map[string]string{
	unanswerableTopic: "---\ntype: project\n---\n\n" +
		"# Tunnel identifier for the upstream provider\n\n" +
		"The tunnel identifier configured for this project is `" + unanswerableTruth + "`.\n\n" +
		"It came from the provider's onboarding flow, back when the integration was first stood up. The " +
		"string is a legacy artefact: it does not spell out the environment it identifies, and the team has " +
		"kept it verbatim because the provider has this exact string provisioned and cannot re-issue a " +
		"different value for an environment that is already provisioned.\n\n" +
		"It is not a self-chosen name. It was not compared against anything, and it was not generated: the " +
		"provider handed it over in the onboarding thread and the team pasted it in.\n\n" +
		"If the team were free to choose again today the value would very likely change — the provider " +
		"would have to provision a new environment first, and the current string carries no information for " +
		"a reader. What holds it in place is the provisioning record on the provider side, not any property " +
		"of the string itself.\n",
	"release-gate.md": "---\ntype: project\n---\n\n# Release gate\n\nReleases are cut from `release/*` " +
		"branches only. A release build is signed by the release robot; nobody merges straight to `main`.\n",
	"freeze-windows.md": "---\ntype: project\n---\n\n# Freeze windows\n\nNo releases ship in the last " +
		"week of a quarter. Feature work may still merge, but nothing is cut.\n",
	"coverage-floor.md": "---\ntype: feedback\n---\n\n# Coverage floor\n\nThe team has asked that new " +
		"packages land with tests covering the error paths, not only the happy path.\n",
	"manifest-authority.md": "---\ntype: project\n---\n\n# Manifest authority\n\n`go.mod` edits go " +
		"through the platform team. Pull the module version you need rather than bumping it in a feature branch.\n",
	"user_role.md": "---\ntype: user\n---\n\n# Who the user is\n\nBackend engineer on the runtime team. " +
		"Prefers short answers and is not interested in being walked through basics.\n",
	"oncall-handover.md": "---\ntype: project\n---\n\n# On-call handover\n\nThe on-call rotation hands " +
		"over on Wednesdays. Notes go into the runbook, not into chat.\n",
}

// unanswerableSharedFragments 真值里**天然会被 decoy 共享**的片段。
//
// 判据是"共享的片段没有判别力"：真值 q7-tunnel-corp 与 decoy q7-tunnel-lb 共用
// `q7` 与 `tunnel` 两段，所以只查这两段会把一个已经测好冲突的 C 臂夹具判成
// "答案在仓库里"。**不是**放宽判据 —— 判别力真正全部落在尾段 `corp` 上，而它必须
// 零出现。这个清单被 TestUnanswerableFragmentsAreDiscriminating 锁住。
func unanswerableSharedFragments() []string { return []string{"q7", "tunnel"} }

// unanswerableFragments 把被问的取值切成**有判别力**的片段。
//
// "模型不需要拿到完整串也能答对"是这条断言的动机：只查完整串会漏掉"某文件里写着
// corp，所以答案是 q7-tunnel-corp"这种半个答案。但共有的片段（q7 / tunnel）必须
// 排除，否则 C 臂会因为 decoy 合法地共享它们而误报。
func unanswerableFragments(v string) []string {
	shared := map[string]bool{}
	for _, s := range unanswerableSharedFragments() {
		shared[s] = true
	}
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) >= 3 && !shared[p] { // 太短或共享的片段没有判别力
			out = append(out, p)
		}
	}
	return out
}

// unanswerableIndex 生成 MEMORY.md 索引（官方形态：标题 + 一行一条纯文件名，**不含**
// 任何描述 —— §4.5：官方全文未出现 title/description/摘要，本方案不发明）。
//
// header 里带记忆目录的绝对路径，因为官方就是这样让模型知道去哪儿读的；干扰项与
// 真条目在形态上完全一样 —— 索引对模型是一张没有优先级的清单。
func unanswerableIndex(topicDir string) string {
	names := make([]string, 0, len(unanswerableMemoryTopicFiles))
	for name := range unanswerableMemoryTopicFiles {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("## Project Memory\n\nThese notes are kept outside the repository in:\n\n    ")
	b.WriteString(topicDir)
	b.WriteString("\n\n")
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteString("\n")
	}
	return b.String()
}

// unanswerableArm 三臂之一。
type unanswerableArm struct {
	Name     string
	Inject   string
	DocLabel string
	Fixture  map[string]string
}

// unanswerableArms 造三臂。inject 由 topicDir 决定，故必须与目录一起构造。
//
// B 与 C 的注入**逐字节相同**（同一个真实场景：记忆里有答案），仓库里多一份
// decoy。这样 B 与 C 的唯一差别是"仓库里有没有一个貌似可信的相反说法" —— 干净的
// 冲突检测对照。若给 C 臂不同的注入文本，差异就分不清来自记忆还是来自文本差异。
func unanswerableArms(topicDir string) []unanswerableArm {
	index := unanswerableIndex(topicDir)
	return []unanswerableArm{
		{Name: "A_no_memory", Inject: "",
			DocLabel: "nothing injected; the answer exists neither in the repo nor in context",
			Fixture:  unanswerablePlainFixture()},
		{Name: "B_memory_has_answer", Inject: index,
			DocLabel: "index points at a topic file outside the repo that carries the answer",
			Fixture:  unanswerablePlainFixture()},
		{Name: "C_repo_has_decoy", Inject: index,
			DocLabel: "same index as B, plus a plausible opposite value in the repo",
			Fixture:  unanswerableDecoyFixture()},
	}
}

// unanswerablePlainFixture A/B 臂的夹具：基础夹具 + **legacy 措辞**的 runbook，**无 decoy**。
//
// 刻意保持与上一版逐字节同构：F 臂（本次新增的 A_legacy）就是用这个函数造的，
// 于是 F→B' 的配对差值**只**归因于那一句 sabotage 措辞，不会混入"夹具重造"的噪声。
func unanswerablePlainFixture() map[string]string {
	m := make(map[string]string, len(unanswerableFixtureFiles))
	for k, v := range unanswerableFixtureFiles {
		m[k] = v
	}
	m["docs/runbook-oncall.md"] = unanswerableRunbookContent(unanswerableFlavorLegacy)
	return m
}

// —— 夹具自检：答案确实不在仓库里 ——

// unanswerableProveAnswerAbsent 用夹具**自己的** read_file / grep / glob 穷尽证明
// 被问的取值不在夹具里。**这是本文件判据正确性的前提**：若真值其实在仓库里，本文件
// 测的就是"模型找不找得到"，不是"记忆有没有用"。
//
// 证明方式不是"我觉得没有"，而是走生产工具（与模型手上同一批实例）：
//
//  1. glob("**/*") 枚举夹具里的每一个文件，逐个 read_file 读全文；
//  2. 在每份全文里搜索真值，以及它的**每一个片段**（模型不需要拿到完整串就能
//     答对，只拿到 `corp` 这个线索也够 —— 只查完整串会漏）；
//  3. 对夹具跑真实 grep 工具（pattern = 真值与各片段），确认零命中；
//  4. 反向确认 decoy 取值**确实**在 C 臂夹具里、且**不在** A/B 臂夹具里。
//
// 扫描与断言分开（unanswerableScanFixture / 本函数）是为了让"证明"本身可被
// **反向测试**：把真值故意塞进夹具，断言扫描确实报出发现 —— 否则这段证明可能是个
// 什么都不做的空壳，而没人会发现。见 TestUnanswerableAbsenceProofCatchesTruth。
func unanswerableProveAnswerAbsent(t *testing.T) string {
	t.Helper()
	var findings []string

	for _, variant := range []struct {
		label   string
		fixture map[string]string
	}{
		{"A/B fixture", unanswerablePlainFixture()},
		{"C fixture", unanswerableDecoyFixture()},
	} {
		findings = append(findings, unanswerableScanFixture(t, variant.label, variant.fixture)...)
	}

	// 反向确认：decoy 确实在 C 臂夹具里（否则 C 臂没测到冲突，只是多了一个无关文件）
	cRoot := unanswerableExecFixture(t, unanswerableDecoyFixture())
	if out, err := unanswerableCall(t, unanswerableReadOnlyEngine(t, cRoot), "grep",
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, unanswerableDecoy, cRoot)); err != nil || !unanswerableGrepHit(out) {
		findings = append(findings, fmt.Sprintf("C 夹具里 grep %q 无命中：decoy 没落盘，C 臂测不到冲突", unanswerableDecoy))
	}
	// A/B 臂必须**没有** decoy，否则 A 臂就是"仓库里有错答案"，不是"什么都没有"
	abRoot := unanswerableExecFixture(t, unanswerablePlainFixture())
	if out, err := unanswerableCall(t, unanswerableReadOnlyEngine(t, abRoot), "grep",
		fmt.Sprintf(`{"pattern":%q,"path":%q}`, unanswerableDecoy, abRoot)); err == nil && unanswerableGrepHit(out) {
		findings = append(findings, fmt.Sprintf("A/B 夹具里 grep %q 有命中：A 臂被偷偷放了 decoy", unanswerableDecoy))
	}

	if len(findings) > 0 {
		t.Fatalf("夹具自检失败 —— 被问的取值在仓库里（或 decoy 没落盘），本文件测的就不是"+
			"「记忆有没有用」：\n  %s", strings.Join(findings, "\n  "))
	}

	return fmt.Sprintf("真值 %q 及其片段 %v：用夹具自己的 glob+read_file 穷尽 %d 个文件、再用真实 grep 工具复查，"+
		"A/B 夹具与 C 夹具均零命中；decoy %q 只在 C 夹具的 %s 里（grep 正向确认），A/B 夹具零命中",
		unanswerableTruth, unanswerableFragments(unanswerableTruth), len(unanswerableFixtureFiles)+1,
		unanswerableDecoy, unanswerableDecoyDoc)
}

// unanswerableScanFixture 扫描一份夹具，返回"真值/片段出现在仓库里"的发现列表。
// 空列表 = 这份夹具里确实没有答案。
//
// 刻意做成返回发现而不是 Fail：反向测试要直接断言它**会**报出东西，否则"证明通过"
// 与"证明什么都不做"这两件事在测试输出里长得一模一样。
func unanswerableScanFixture(t *testing.T, label string, fixture map[string]string) []string {
	t.Helper()
	var findings []string
	root := unanswerableExecFixture(t, fixture)
	eng := unanswerableReadOnlyEngine(t, root)

	// 1. glob 枚举 + read_file 读全文
	var files []string
	if out, err := unanswerableCall(t, eng, "glob", `{"pattern":"**/*"}`); err == nil {
		files = unanswerableParseGlobPaths(out)
	} else {
		findings = append(findings, fmt.Sprintf("%s: glob 失败: %v", label, err))
	}
	if len(files) == 0 {
		findings = append(findings, fmt.Sprintf("%s: glob 枚举到 0 个文件，穷尽证明无从谈起", label))
	}
	for _, rel := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		body, err := unanswerableCall(t, eng, "read_file", fmt.Sprintf(`{"path":%q}`, abs))
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: read_file %s 失败: %v", label, rel, err))
			continue
		}
		low := strings.ToLower(body)
		if strings.Contains(low, strings.ToLower(unanswerableTruth)) {
			findings = append(findings, fmt.Sprintf("%s: %s 含完整真值 %s", label, rel, unanswerableTruth))
		}
		for _, frag := range unanswerableFragments(unanswerableTruth) {
			if strings.Contains(low, strings.ToLower(frag)) {
				findings = append(findings, fmt.Sprintf("%s: %s 含真值片段 %q", label, rel, frag))
			}
		}
	}

	// 2. 真实 grep 工具
	for _, needle := range append([]string{unanswerableTruth}, unanswerableFragments(unanswerableTruth)...) {
		out, err := unanswerableCall(t, eng, "grep", fmt.Sprintf(`{"pattern":%q,"path":%q}`, needle, root))
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s: grep %q 失败: %v", label, needle, err))
			continue
		}
		if unanswerableGrepHit(out) {
			findings = append(findings, fmt.Sprintf("%s: grep %q 命中: %.200s", label, needle, out))
		}
	}
	return findings
}

// unanswerableDecoyFixture 造上一版 C 臂的夹具（基础夹具 + **legacy 措辞** runbook + decoy 文档）。
//
// **与原始 C 臂逐字节同构**，用于确认 sabotage 修掉之后那个 0% 确实是 sabotage 造成的
// （对照 C_legacy vs C 干净）。C 臂自身不再用它。
func unanswerableDecoyFixture() map[string]string {
	m := make(map[string]string, len(unanswerableFixtureFiles)+1)
	for k, v := range unanswerableFixtureFiles {
		m[k] = v
	}
	m["docs/runbook-oncall.md"] = unanswerableRunbookContent(unanswerableFlavorLegacy)
	m[unanswerableDecoyDoc] = unanswerableDecoyContent(unanswerableDecoy)
	return m
}

// conflictFixture 造**干净**夹具：中性措辞的 runbook，可选是否带 ADR-021 decoy。
// C/D/E 三个臂全部从这里造，且只有 hasDecoy 这一个布尔量不同。
func conflictFixture(hasDecoy bool) map[string]string {
	m := unanswerableBaseFixture()
	if hasDecoy {
		m[unanswerableDecoyDoc] = unanswerableDecoyContent(unanswerableDecoy)
	}
	return m
}

// unanswerableGrepHit 判断 grep 输出里有没有真正的命中。
//
// grep 的输出首行有三种形态（见 tools/builtin/grep.go）：
//
//	[grep: N match(es) in M file(s)]        ← 正常
//	[grep: 0 match(es) in 0 file(s)]        ← 零命中，后面没有任何行
//	[grep: more than K matches, showing ...] ← 触顶（算命中）
//
// 零命中时首行**非空**，所以只判 `strings.TrimSpace(out) != ""` 会把"零命中"读成
// "命中" —— 那会让夹具自检假通过，而假通过的方向恰恰是本文件最不能错的那个
// （真值在仓库里，却报"不在"）。
func unanswerableGrepHit(out string) bool {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return false
	}
	head := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(head, "[grep:") {
		return false
	}
	if i := strings.Index(head, "match(es)"); i >= 0 {
		// 取 "N match(es)" 前的那个整数
		var n int
		if _, err := fmt.Sscanf(head[:i], "[grep: %d", &n); err == nil && n == 0 {
			return false
		}
	}
	// 触顶提示行也算命中；其余情况要求真的有结果行。
	return len(lines) > 1 && strings.TrimSpace(lines[1]) != ""
}

// unanswerableReadOnlyEngine 装一套与模型手上**完全相同**的只读工具面。
//
// 返回的是三个工具本身（不是 Engine）：夹具自检要按工具名直接调 `Call`，而
// `tools.Engine` 只暴露 Sequence/Parallel/Batch（那会投递事件、跑审批），不提供
// 单工具直调。用同一批工具实例保证自检与被测路径**走的是同一份实现**。
func unanswerableReadOnlyEngine(t *testing.T, root string) map[string]tools.Tool {
	t.Helper()
	out := map[string]tools.Tool{}
	for _, tl := range builtin.NewFileTools(root).Tools() {
		switch tl.Name() {
		case "read_file", "grep", "glob":
			out[tl.Name()] = tl
		}
	}
	for _, name := range []string{"read_file", "grep", "glob"} {
		if out[name] == nil {
			t.Fatalf("只读工具面缺 %s：自检与被测路径必须用同一批工具", name)
		}
	}
	return out
}

// unanswerableCall 调一个工具（自检专用）。
func unanswerableCall(t *testing.T, eng map[string]tools.Tool, name, args string) (string, error) {
	t.Helper()
	tl := eng[name]
	if tl == nil {
		t.Fatalf("未知工具 %s", name)
	}
	return tl.Call(context.Background(), name, args)
}

// unanswerableParseGlobPaths 从 glob 输出里取仓库内相对路径。
//
// glob 的输出形状是 `[glob: <pattern>, N file(s)]` 首行 + 每行一个相对路径。
// 这里的过滤必须**严**：任何一条宽松规则都会让"读了 0 个文件"被误判成"读了 5 个"，
// 而那正是本文件要证明的反面。
func unanswerableParseGlobPaths(out string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[glob:") {
			continue
		}
		if strings.ContainsAny(line, "*?") || filepath.IsAbs(line) {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(line))
		if clean == "." || seen[clean] {
			continue
		}
		seen[clean] = true
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	return paths
}

// —— 装置 ——

// unanswerableTopicDir 建一个工作区之外的记忆目录，**整个实验共用一个**：这样 B 与 C
// 臂的注入文本在所有重复里逐字节相同，不引入"每次跑目录名不同"的额外噪声。
func unanswerableTopicDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hai-mem-eval-memory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(unanswerableIndex(dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range unanswerableMemoryTopicFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// unanswerableExecFixture 造一份全新的夹具仓，返回根目录。
func unanswerableExecFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// unanswerableToolRecorder 记录工具调用与读过的文件。事件流是并发投递的，必须自己
// 加锁（Step 0 实测过不加锁会崩在 concurrent map writes）。
type unanswerableToolRecorder struct {
	mu        sync.Mutex
	toolCalls int
	docs      []string // 成功 read_file 到的文件，按发生顺序
	readErrs  int
	names     map[string]int
}

func newUnanswerableToolRecorder() *unanswerableToolRecorder {
	return &unanswerableToolRecorder{names: map[string]int{}}
}

var unanswerablePathRe = regexp.MustCompile(`"path"\s*:\s*"([^"]*)"`)

func (r *unanswerableToolRecorder) handle(root string) events.EventHandler {
	return func(_ context.Context, ev events.Event) {
		e, ok := ev.(*events.ToolResponse)
		if !ok {
			return
		}
		m := unanswerablePathRe.FindStringSubmatch(e.Arguments)
		if len(m) != 2 || m[1] == "" {
			r.mu.Lock()
			r.toolCalls++
			r.names[e.Name]++
			r.mu.Unlock()
			return
		}
		p := m[1]
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)

		r.mu.Lock()
		defer r.mu.Unlock()
		r.toolCalls++
		r.names[e.Name]++
		if e.Name != "read_file" {
			return
		}
		if e.IsError {
			r.readErrs++
			return
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			r.docs = append(r.docs, p)
		}
	}
}

// snapshot 回收读数。docs 用**工作区相对路径**返回，工作区外的文件另计（那是记忆
// recall 的直接证据，必须一眼可见）。
func (r *unanswerableToolRecorder) snapshot(root string) (docs []string, outside int, readErrs int, names map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	docs = []string{}
	for _, p := range r.docs {
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			docs = append(docs, filepath.ToSlash(rel))
			continue
		}
		docs = append(docs, p)
		outside++
	}
	names = map[string]int{}
	for k, v := range r.names {
		names[k] = v
	}
	return docs, outside, r.readErrs, names
}

func (r *unanswerableToolRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolCalls
}

// unanswerableRunOnce 跑一次任务并回收读数。工具面、预算、提问与 Step 0 同构，只读。
func unanswerableRunOnce(t *testing.T, p provider.Provider, prov, model, root, inject string) unanswerableRun {
	t.Helper()
	// 与 Step 0 的 baselineRunOnce 逐行同构：同一批只读工具、同一套 agents.Option。
	// 唯一区别是迭代上限 8→10（本任务的提问有两问，且 A 臂需要时间把"查不到"
	// 变成一个完整答案；8 轮会截断它，那会把工具预算变成第二个自变量）。
	eng := tools.NewToolEngine()
	for _, tl := range unanswerableReadOnlyEngine(t, root) {
		eng.RegisterTool(context.Background(), tl)
	}

	run := unanswerableRun{Question: unanswerableQuestion, Injected: inject}
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(eng),
		agents.WithWorkingDir(root),
		agents.WithMaxIterations(10),
		agents.WithMaxRetries(2),
		agents.WithLLMTimeout(4 * time.Minute),
	}
	if inject != "" {
		opts = append(opts, agents.WithSystemPrompt(inject))
	}

	rec := newUnanswerableToolRecorder()
	recHandle := rec.handle(root)
	var mu sync.Mutex
	var final string
	loop := agents.NewAgentLoop(opts...)
	handler := events.EventHandler(func(ctx context.Context, ev events.Event) {
		recHandle(ctx, ev)
		if e, ok := ev.(*events.AgentEnd); ok {
			mu.Lock()
			defer mu.Unlock()
			final = e.Content
		}
	})

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	ac := loop.Run(ctx, []core.Message{core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: unanswerableQuestion,
	})}, handler)
	cancel()
	run.DurationSec = time.Since(start).Seconds()
	if ac != nil && ac.Err != nil {
		run.Err = ac.Err.Error()
	}
	mu.Lock()
	run.FinalAnswer = final
	mu.Unlock()
	run.ToolCalls = rec.calls()
	run.DocsRead, run.DocsOutside, run.ReadErrs, run.ToolNames = rec.snapshot(root)
	run.NDocs = len(run.DocsRead)
	return run
}

// —— 独立裁定（§7）——

// unanswerableJudgePrompt 只有最终答案进输入：不看调用过程、不看注入文本。三个出口
// （A / B / C）避免被迫二选一时把"我查不到"记成其中一个值。
func unanswerableJudgePrompt(question, a, b, answer string) string {
	return fmt.Sprintf(`You are grading one answer to one factual question.

Question: %s

Candidate answers:
(A) %s
(B) %s

Answer submitted by an AI agent:
<answer>
%s
</answer>

Decide which candidate value the agent gave as ITS OWN final answer to the question
(the value it asserts is the one actually configured today).
- If the agent reports that two sources disagree, use the value it concludes is correct.
- If the agent's answer matches neither candidate, or it declines to answer, reply C.

Reply with exactly one character: A, B or C. No explanation.`, question, a, b, answer)
}

var unanswerableChoiceRe = regexp.MustCompile(`\b([ABC])\b`)

func unanswerableParseChoice(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	first := s
	if i := strings.IndexAny(first, "\n"); i >= 0 {
		first = first[:i]
	}
	m := unanswerableChoiceRe.FindStringSubmatch(strings.ToUpper(first))
	if m == nil {
		m = unanswerableChoiceRe.FindStringSubmatch(strings.ToUpper(s))
	}
	if m == nil {
		return "", false
	}
	switch m[1] {
	case "A", "B", "C":
		return m[1], true
	}
	return "", false
}

func unanswerableJudge(ctx context.Context, p provider.Provider, prov, model, prompt string) (raw, choice string, err error) {
	var out string
	err = p.Stream(ctx, &provider.StreamRequest{
		Model:     model,
		Provider:  prov,
		SessionID: "unanswerable-judge",
		Messages: []core.Message{core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: prompt,
		})},
	}, func(ev provider.StreamEvent) error {
		if e, ok := ev.(provider.LLMEndEvent); ok {
			out = e.Content
		}
		return nil
	})
	ch, ok := unanswerableParseChoice(out)
	if !ok {
		ch = "ERROR"
	}
	return out, ch, err
}

// unanswerableJudgeTwice 对同一答案裁定两次且**候选顺序对调**。顺序对调同时度量抖动
// 与位置偏置（上一版自洽率 36% 的教训）。归一化后的判定词与候选标签无关：
// DECOY = 交出仓库 decoy 取值，KNOWN = 交出记忆里的真值，NEITHER = 都没交。
func unanswerableJudgeTwice(ctx context.Context, p provider.Provider, prov, model, question, answer string) (string, bool, [2]string, error) {
	type pass struct {
		raw, choice  string
		ok           bool
		firstIsDecoy bool
	}
	var passes [2]pass
	for i, firstIsDecoy := range []bool{true, false} {
		var a, b string
		if firstIsDecoy {
			a, b = unanswerableDecoy, unanswerableTruth
		} else {
			a, b = unanswerableTruth, unanswerableDecoy
		}
		raw, ch, err := unanswerableJudge(ctx, p, prov, model, unanswerableJudgePrompt(question, a, b, answer))
		if err != nil {
			passes[i] = pass{raw: "error: " + err.Error(), choice: "ERROR", ok: false, firstIsDecoy: firstIsDecoy}
			continue
		}
		_, ok := unanswerableParseChoice(raw)
		passes[i] = pass{raw: raw, choice: ch, ok: ok, firstIsDecoy: firstIsDecoy}
	}
	norm := [2]string{}
	for i, ps := range passes {
		switch {
		case !ps.ok:
			norm[i] = "ERROR"
		case ps.choice == "C":
			norm[i] = "NEITHER"
		case (ps.choice == "A") == ps.firstIsDecoy:
			norm[i] = "DECOY"
		default:
			norm[i] = "KNOWN"
		}
	}
	agree := norm[0] != "ERROR" && norm[1] != "ERROR" && norm[0] == norm[1]
	final := norm[0]
	if final == "ERROR" {
		final = norm[1]
	}
	return final, agree, [2]string{passes[0].raw, passes[1].raw}, nil
}

// —— 读数 ——

type unanswerableRun struct {
	Arm         string         `json:"arm"`
	Rep         int            `json:"rep"`
	Question    string         `json:"question"`
	Injected    string         `json:"injected"`
	FinalAnswer string         `json:"final_answer"`
	Err         string         `json:"err,omitempty"`
	ToolCalls   int            `json:"tool_calls"`
	ToolNames   map[string]int `json:"tool_names"`
	DocsRead    []string       `json:"docs_read"`
	NDocs       int            `json:"n_docs"`
	DocsOutside int            `json:"docs_outside_workspace"`
	ReadErrs    int            `json:"read_errs"`
	Verdict     string         `json:"verdict"`
	SelfAgree   bool           `json:"self_agree"`
	VerdictRaw  [2]string      `json:"verdict_raw"`
	DurationSec float64        `json:"duration_sec"`
	// 下面两个是**降级读数**，由精确 token 匹配得到，不参与正确率判定。
	// 判定只能用独立模型（§7）；但"模型是否明说查不到""是否被 decoy 带跑"这两个
	// 问题问的是"答案里有没有出现某个**具体取值**"，独立模型答不了，只能精确匹配。
	DeclaredUnrecoverable bool `json:"declared_unrecoverable"`
	MentionsDecoyValue    bool `json:"mentions_repo_decoy_value"`
	MentionsTruthValue    bool `json:"mentions_memory_truth_value"`
}

type unanswerableArmStat struct {
	Arm         string         `json:"arm"`
	DocLabel    string         `json:"doc_label"`
	N           int            `json:"n"`
	NErr        int            `json:"n_err"`
	Correct     int            `json:"correct"`
	CorrectRate float64        `json:"correct_rate"`
	WilsonLo    float64        `json:"wilson_lo"`
	WilsonHi    float64        `json:"wilson_hi"`
	Declared    int            `json:"declared_unrecoverable"`
	SaidDecoy   int            `json:"said_repo_decoy_value"`
	SaidTruth   int            `json:"said_memory_truth_value"`
	ToolMean    float64        `json:"tool_calls_mean"`
	ToolStd     float64        `json:"tool_calls_std"`
	DocsMean    float64        `json:"docs_read_mean"`
	DocsStd     float64        `json:"docs_read_std"`
	OutsideMean float64        `json:"docs_outside_workspace_mean"`
	ReadErrMean float64        `json:"read_errs_mean"`
	Verdicts    map[string]int `json:"verdicts"`
	Seq         string         `json:"correctness_sequence"`
}

type unanswerableReport struct {
	Experiment  string                `json:"experiment"`
	Protocol    string                `json:"protocol_version"`
	RunID       string                `json:"run_id"`
	At          string                `json:"at"`
	Model       string                `json:"model"`
	Provider    string                `json:"provider"`
	JudgeModel  string                `json:"judge_model"`
	JudgeProv   string                `json:"judge_provider"`
	HarnessSnap string                `json:"harness_snapshot"`
	MemorySnap  string                `json:"memory_snapshot"`
	TaskSet     string                `json:"task_set"`
	Budget      string                `json:"budget"`
	Reps        int                   `json:"reps_per_arm"`
	TopicDir    string                `json:"topic_dir"`
	AbsentProof string                `json:"absent_proof"`
	Arms        []unanswerableArmStat `json:"arms"`
	Runs        []unanswerableRun     `json:"runs"`
	Analysis    unanswerableAnalysis  `json:"analysis"`
	JudgeSelf   float64               `json:"judge_self_consistency"`
	JudgeSwap   int                   `json:"judge_order_disagreements"`
	JudgeJudged int                   `json:"judge_judged"`
	Notes       []string              `json:"notes"`
}

type unanswerableAnalysis struct {
	// —— B vs A：记忆有没有用（本文件的判据）——
	ACorrect, AN int
	BCorrect, BN int
	BADiffPP     float64 `json:"b_minus_a_pp"`
	BASEPP       float64 `json:"b_minus_a_se_pp_unpaired"`
	BAP          float64 `json:"b_minus_a_p_unpaired"`
	BACILoPP     float64 `json:"b_minus_a_ci95_lo_pp"`
	BACIHiPP     float64 `json:"b_minus_a_ci95_hi_pp"`
	BAVerdict    string  `json:"b_vs_a_verdict"`
	BACeiling    bool    `json:"b_is_ceiling_0_to_1"`
	BAFloor      bool    `json:"b_is_floor_effect_0_to_1"`
	BAMDEPP      float64 `json:"b_minus_a_mde_pp_unpaired"`
	BAWilsonLoB  float64 `json:"b_wilson_lo"`
	BAWilsonHiB  float64 `json:"b_wilson_hi"`
	BAWilsonLoA  float64 `json:"a_wilson_lo"`
	BAWilsonHiA  float64 `json:"a_wilson_hi"`

	// —— C vs B：冲突处理（**不**与上面同一个判据）——
	CCorrect, CN int
	CBDiffPP     float64 `json:"c_minus_b_pp"`
	CBP          float64 `json:"c_minus_b_p_unpaired"`
	CBCILoPP     float64 `json:"c_minus_b_ci95_lo_pp"`
	CBHiPP       float64 `json:"c_minus_b_ci95_hi_pp"`
	CBVerdict    string  `json:"c_vs_b_verdict"`

	// —— 与 Step 0 的 89% 基准的关系 ——
	Step0CNoMemoryPP float64 `json:"step0_c_no_memory_pp"`
	Observation      string  `json:"observation"`
}

// —— 环境 ——

func unanswerableEnvOrSkip(t *testing.T) (prov, model string, p provider.Provider) {
	t.Helper()
	isolateUserPrices(t) // 读真实 settings 前先隔离全局价表（见 memory_baseline_test.go 注释）
	if testing.Short() {
		t.Skip("慢测试：需真实 provider（-short 跳过）")
	}
	prov = os.Getenv("HAI_EVAL_PROVIDER")
	if prov == "" {
		prov = baselineDefaultProvider
	}
	model = os.Getenv("HAI_EVAL_MODEL")
	if model == "" {
		model = baselineDefaultModel
	}
	pc := loadProviderConfig()
	if pc.baseURL[prov] == "" {
		t.Skipf("无 provider %q 配置（settings.json 缺该条）：skip，绝不假失败", prov)
	}
	if strings.TrimSpace(pc.keyLocked(prov)) == "" {
		t.Skipf("无 provider %q 凭证：skip，绝不假失败", prov)
	}
	return prov, model, pc.newProvider(prov)
}

// unanswerableReps 读重复次数。地板 6（协议 §8：n<6 样本严重不足）。
func unanswerableReps(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("HAI_EVAL_REPS")
	if raw == "" {
		return unanswerableRepsFloor
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("HAI_EVAL_REPS=%q 解析失败（要一个正整数）", raw)
	}
	if n < unanswerableRepsFloor {
		t.Fatalf("HAI_EVAL_REPS=%d < 协议地板 %d（§8：n<6 样本严重不足）", n, unanswerableRepsFloor)
	}
	return n
}

func unanswerablePing(t *testing.T, p provider.Provider, prov, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var got string
	err := p.Stream(ctx, &provider.StreamRequest{
		Model:     model,
		Provider:  prov,
		SessionID: "unanswerable-ping",
		Messages: []core.Message{core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: "Reply with exactly: PONG",
		})},
	}, func(ev provider.StreamEvent) error {
		if e, ok := ev.(provider.LLMEndEvent); ok {
			got = strings.TrimSpace(e.Content)
		}
		return nil
	})
	if err != nil {
		if os.Getenv("HAI_EVAL_ALLOW_PING_SKIP") == "1" {
			t.Skipf("真实 provider 探活失败且 HAI_EVAL_ALLOW_PING_SKIP=1 → skip: %v", err)
		}
		t.Fatalf("真实 provider %s/%s 探活失败（凭证在但发不出去）：%v。\n"+
			"静默 skip 会让整套实验全跳而套件仍显示全绿。", prov, model, err)
	}
	if got == "" {
		t.Fatalf("真实 provider %s/%s 探活返回空内容：不认为这是一次成功探活。", prov, model)
	}
}

// —— 测试 ——

// TestUnanswerableKnowledgeThreeArms 是补测二。
//
// 判据：**B 必须显著高于 A。** 这是"记忆有用"的第一份直接证据。
// 若 B≈A，结论就是"对能主动核对的模型，记忆的增量价值比预期小"。
func TestUnanswerableKnowledgeThreeArms(t *testing.T) {
	memSnap := baselineEnforceEmptyMemoryState(t)

	prov, model, p := unanswerableEnvOrSkip(t)
	jm := baselineJudgeModel()
	pc := loadProviderConfig()
	if pc.baseURL[prov] == "" || strings.TrimSpace(pc.keyLocked(prov)) == "" {
		t.Skipf("判定器复用被测 provider 配置，%q 不可用", prov)
	}
	judgeProv, judgeP := prov, pc.newProvider(prov)
	if jm == model {
		t.Logf("警告：判定器模型与被测模型相同（%s）。§7 要求独立裁定。", jm)
	}
	unanswerablePing(t, p, prov, model)
	unanswerablePing(t, judgeP, judgeProv, jm)
	reps := unanswerableReps(t)

	// 夹具正确性先于一切：先证被问的取值不在仓库里，再花 token。
	absent := unanswerableProveAnswerAbsent(t)
	t.Logf("夹具自检：%s", absent)

	topicDir := unanswerableTopicDir(t)
	arms := unanswerableArms(topicDir)

	rep := &unanswerableReport{
		Experiment:  "supplement-2-unanswerable-knowledge",
		Protocol:    unanswerableProtocolVersion,
		RunID:       fmt.Sprintf("%d", time.Now().UnixNano()),
		At:          time.Now().UTC().Format(time.RFC3339),
		Model:       model,
		Provider:    prov,
		JudgeModel:  jm,
		JudgeProv:   judgeProv,
		HarnessSnap: baselineGitHEAD(t),
		MemorySnap:  memSnap,
		TaskSet:     unanswerableTaskSet,
		Budget:      unanswerableBudget,
		Reps:        reps,
		TopicDir:    topicDir,
		AbsentProof: absent,
		Notes: []string{
			"本文件补的是 Step 0 的核心缺口：答案在仓库任何文件里都不存在的任务（协议 §5.1）。",
			"记忆与仓库**不冲突**：仓库说标识符形如 q7-tunnel-<后缀>，记忆说后缀是什么、为什么。" +
				"这是 Step 0 最大的教训 —— fixture 与契约矛盾会让数字测服从性而不是价值。",
			"topic 文件在工作区**之外**（技术方案 §5.4 已确认允许 read_file 访问工作区外的记忆目录）。" +
				"放在工作区内会让 B 臂退化成 Step 0 的「记忆指路」形态。",
			"被问的取值是不透明 token，因此「答案含关键词」式判定在构造上不可能通过 —— 判定只能用独立模型。",
			"没有记忆协议注入：官方不向模型追加协议文本，索引本身就是全部引导（技术方案 §2）。",
			"7 条干扰项（§5.4 要求 >5）：索引对模型是一张没有优先级的清单。",
			"C 臂与 B 臂的注入**逐字节相同**（同一个真实场景：记忆里有答案），仓库里多一份 decoy。",
			"Step 0 的 C 臂 89% 是「仓库里有答案」时的基准；本文件的 A 臂是另一个基准（答案无处可寻）。两者不可比。",
			"未测到：记忆写入/淘汰/自进化（协议 §2 明确不评估）、subagent 不继承、secret 泄漏、跨仓库隔离。",
		},
	}

	t.Logf("protocol=%s task_set=%s harness=%s", unanswerableProtocolVersion, unanswerableTaskSet, rep.HarnessSnap)
	t.Logf("provider=%s model=%s | judge=%s/%s | reps/arm=%d | topicDir=%s", prov, model, judgeProv, jm, reps, topicDir)
	t.Logf("真值 %s 只在 topic 文件里；decoy %s 只在 %s；A 臂两者都拿不到",
		unanswerableTruth, unanswerableDecoy, unanswerableDecoyDoc)
	t.Logf("")

	judged, selfAgree, swapDis := 0, 0, 0
	for _, arm := range arms {
		stat := unanswerableArmStat{Arm: arm.Name, DocLabel: arm.DocLabel, Verdicts: map[string]int{}}
		var tools, docs, outside, readErrs []float64
		var seq strings.Builder
		for i := 0; i < reps; i++ {
			root := unanswerableExecFixture(t, arm.Fixture)
			run := unanswerableRunOnce(t, p, prov, model, root, arm.Inject)
			run.Arm, run.Rep = arm.Name, i

			// 降级读数（精确 token 匹配，不参与正确率判定）。
			low := strings.ToLower(run.FinalAnswer)
			run.MentionsTruthValue = strings.Contains(low, strings.ToLower(unanswerableTruth))
			run.MentionsDecoyValue = strings.Contains(low, strings.ToLower(unanswerableDecoy))
			run.DeclaredUnrecoverable = unanswerableDeclaresUnrecoverable(low)

			if run.Err != "" || strings.TrimSpace(run.FinalAnswer) == "" {
				run.Verdict = "ERROR"
				stat.NErr++
				stat.Verdicts["ERROR"]++
				rep.Runs = append(rep.Runs, run)
				t.Logf("  [%s #%d] 运行失败或无最终答案: err=%q", arm.Name, i, run.Err)
				continue
			}

			jctx, jcancel := context.WithTimeout(context.Background(), 3*time.Minute)
			verdict, agree, raw, jerr := unanswerableJudgeTwice(jctx, judgeP, judgeProv, jm,
				unanswerableQuestion, run.FinalAnswer)
			jcancel()
			run.Verdict, run.SelfAgree, run.VerdictRaw = verdict, agree, raw
			if jerr != nil {
				run.Verdict = "ERROR"
			}
			rep.Runs = append(rep.Runs, run)
			if run.Verdict == "ERROR" {
				stat.NErr++
				stat.Verdicts["ERROR"]++
				t.Logf("  [%s #%d] 裁定失败: raw=%.160q", arm.Name, i, raw[0])
				continue
			}
			judged++
			if agree {
				selfAgree++
			} else {
				swapDis++
			}
			stat.N++
			stat.Verdicts[verdict]++
			if verdict == "KNOWN" {
				stat.Correct++
				seq.WriteByte('1')
			} else {
				seq.WriteByte('0')
			}
			if run.DeclaredUnrecoverable {
				stat.Declared++
			}
			if run.MentionsDecoyValue {
				stat.SaidDecoy++
			}
			if run.MentionsTruthValue {
				stat.SaidTruth++
			}
			tools = append(tools, float64(run.ToolCalls))
			docs = append(docs, float64(run.NDocs))
			outside = append(outside, float64(run.DocsOutside))
			readErrs = append(readErrs, float64(run.ReadErrs))
			t.Logf("  [%s #%d] verdict=%-7s selfagree=%-5v tools=%2d docs=%d(out=%d) errs=%d | %.130s",
				arm.Name, i, verdict, agree, run.ToolCalls, run.NDocs, run.DocsOutside,
				run.ReadErrs, strings.ReplaceAll(run.FinalAnswer, "\n", " "))
		}
		stat.Seq = seq.String()
		if stat.N > 0 {
			stat.CorrectRate = float64(stat.Correct) / float64(stat.N)
		}
		stat.WilsonLo, stat.WilsonHi = supplementWilsonCI(stat.Correct, stat.N)
		stat.ToolMean, stat.ToolStd = baselineMeanStd(tools)
		stat.DocsMean, stat.DocsStd = baselineMeanStd(docs)
		stat.OutsideMean, _ = baselineMeanStd(outside)
		stat.ReadErrMean, _ = baselineMeanStd(readErrs)
		rep.Arms = append(rep.Arms, stat)

		t.Logf("")
		t.Logf("=== %s — %s ===", arm.Name, arm.DocLabel)
		t.Logf("  正确(交出记忆里的真值) %d/%d = %.0f%%   (Wilson 95%% %.0f%%~%.0f%%)",
			stat.Correct, stat.N, 100*stat.CorrectRate, 100*stat.WilsonLo, 100*stat.WilsonHi)
		t.Logf("  工具调用 %.2f ± %.2f   读文档 %.2f ± %.2f   读到工作区外 %.2f   read_file 报错 %.2f",
			stat.ToolMean, stat.ToolStd, stat.DocsMean, stat.DocsStd, stat.OutsideMean, stat.ReadErrMean)
		t.Logf("  判定分布 %v   明说查不到 %d/%d   提到 decoy 取值 %d/%d   提到真值 %d/%d",
			stat.Verdicts, stat.Declared, stat.N, stat.SaidDecoy, stat.N, stat.SaidTruth, stat.N)
		t.Logf("")
	}

	if judged > 0 {
		rep.JudgeSelf = float64(selfAgree) / float64(judged)
	}
	rep.JudgeSwap, rep.JudgeJudged = swapDis, judged
	rep.Analysis = unanswerableAnalyse(rep)
	unanswerableLogAnalysis(t, rep)

	supplementWriteReport(t, "supplement2", rep)
}

// unanswerableDeclaresUnrecoverable 判断答案是否**明说**"这个值查不到"（legacy 口径）。
//
// ⚠ 本口径已被 262c898 的数据证明是**混淆**的，保留它是为了能与上一版数字并列，
// 新的结论一律用下面两个拆分口径。见 TestConflictDeclaredUnrecoverableIsSplitByScope。
//
// 混淆在哪：短语表里有 "not in the repository" / "nothing in the repository" /
// "no record of"。而本任务的正确回答形态**就是**"仓库里没有这个值，但我从别处拿到
// 了"。于是 B 臂 11/20 出现了"declared_unrecoverable=true 且 verdict=KNOWN"的自相
// 矛盾读数 —— 模型说的是"仓库里查不到"（真的），读数却记成"它放弃作答了"（假的）。
func unanswerableDeclaresUnrecoverable(lowerAnswer string) bool {
	return len(conflictDeclares(lowerAnswer, "legacy")) > 0
}

// conflictDeclares 的 scope 决定"明说查不到"是在**哪个范围**上说的。
type conflictDeclareScope string

const (
	// conflictScopeLegacy 上一版的合并口径（见上）。
	conflictScopeLegacy conflictDeclareScope = "legacy"
	// conflictScopeRepo 只认"仓库里没有这条记录"。这是**真**命题，本任务里模型
	// 经常正确地声明它，**同时**从记忆里给出取值 —— 那不是犹豫，那是分工。
	conflictScopeRepo conflictDeclareScope = "repo"
	// conflictScopeAny 认"任何范围内都没有可用取值"：句子在语义上**排除**了手里
	// 有一个值。用来分辨"我拿到了但不敢确定"（KNOWN）与"我压根没有"（NEITHER）。
	conflictScopeAny conflictDeclareScope = "any"
)

// conflictDeclares 检出"明说查不到"，按 scope 分口径返回命中的短语（去重、保序）。
//
// any 口径必须**排除**"引用了某个出处后仍然断言查不到"这种句子，那恰好是
// "不采纳"——所以它要命中的是像 "I could not determine" / "cannot be established" /
// "is not recoverable" 这类**手上没有值**的措辞，且前面不能有 "repository" 这类
// 限定词把范围收窄到仓库。只靠短语表做不到这件事，所以实现上先按仓库限定句整句
// 摘掉，再看剩下的话是否仍在断言"无值可答"。
func conflictDeclares(lowerAnswer string, scope conflictDeclareScope) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	// repo 限定：说"仓库里没有"，但**不**等于手上没有值。
	repoOnly := []string{
		"not in the repository", "not present in the repository", "not recorded in the repository",
		"not recoverable from the repository", "nothing in the repository",
		"no record of", "does not record", "does not hold", "does not contain",
		"not recorded in", "no literal", "no config file", "no startup configuration",
		"obtain the current value", "ask the team", "ask the operator",
	}
	// 无值可答：手上确实没有可用取值。三个 scope 都要认这一类。
	noValue := []string{
		"not recoverable", "cannot be determined", "can't be determined", "cannot be established",
		"can't establish", "i cannot find", "i can't find", "i could not find", "i couldn't find",
		"no evidence", "cannot be verified", "not determinable", "cannot be established from",
	}

	switch scope {
	case conflictScopeLegacy:
		// 复现上一版口径：一张不分范围的宽表（含 "unknown" 这种什么都算的词）。
		for _, p := range append(append([]string{}, repoOnly...), append(noValue, "unknown")...) {
			if strings.Contains(lowerAnswer, p) {
				add(p)
			}
		}
	case conflictScopeRepo:
		for _, p := range repoOnly {
			if strings.Contains(lowerAnswer, p) {
				add(p)
			}
		}
	case conflictScopeAny:
		// ① 先摘掉"限定在仓库/代码里"的从句：它们是**真**命题且不否定手上已有取值。
		stripped := lowerAnswer
		for _, p := range []string{
			"not recoverable from the repository", "not recoverable from the code",
			"does not contain", "no literal", "no startup configuration", "no config file",
			"not in the repository", "not present in the repository",
			"not recorded in the repository", "recorded in the repository",
			"nothing in the repository", "in the repository", "from the repository",
			"no record of", "does not record", "no literal", "obtain the current value",
		} {
			stripped = strings.ReplaceAll(stripped, p, " ")
		}
		for _, p := range noValue {
			if strings.Contains(stripped, p) {
				add(p)
			}
		}
	}
	return out
}

// unanswerableAnalyse 汇总三臂并翻译成结论。
//
// 判据只给 B vs A："B 必须显著高于 A"。C 臂单独报，因为 B vs C 测的是冲突处理，
// 协议 §5.0 要求它与"记忆有没有用"分开。
func unanswerableAnalyse(rep *unanswerableReport) unanswerableAnalysis {
	var an unanswerableAnalysis
	if len(rep.Arms) != 3 {
		an.Observation = "三臂不完整，不给结论"
		return an
	}
	a, b, c := rep.Arms[0], rep.Arms[1], rep.Arms[2]
	an.ACorrect, an.AN = a.Correct, a.N
	an.BCorrect, an.BN = b.Correct, b.N
	an.CCorrect, an.CN = c.Correct, c.N

	diff, se, _, p, lo, hi := supplementProportionStats(a.Correct, a.N, b.Correct, b.N)
	an.BADiffPP, an.BASEPP, an.BAP = diff, se, p
	an.BACILoPP, an.BACIHiPP = lo, hi
	an.BAMDEPP = supplementMDEUnpaired(a.Correct, a.N, b.Correct, b.N)
	an.BAWilsonLoA, an.BAWilsonHiA = supplementWilsonCI(a.Correct, a.N)
	an.BAWilsonLoB, an.BAWilsonHiB = supplementWilsonCI(b.Correct, b.N)
	diffCB, _, _, pCB, loCB, hiCB := supplementProportionStats(b.Correct, b.N, c.Correct, c.N)
	an.CBDiffPP, an.CBP = diffCB, pCB
	an.CBCILoPP, an.CBHiPP = loCB, hiCB

	an.Step0CNoMemoryPP = 89 // Step 0 的 C 臂：无注入、仓库里有答案

	switch {
	case b.N == 0 || a.N == 0:
		an.BAVerdict = "INSUFFICIENT: 某臂没有可用判定"
	case p < 0.05 && diff > 0:
		an.BAVerdict = fmt.Sprintf("B 显著高于 A（%+.0f pp, p=%.4f）：记忆在这个任务上有用", diff, p)
		an.BACeiling = b.CorrectRate >= 0.999 && a.CorrectRate < 0.5
	case p < 0.05 && diff < 0:
		an.BAVerdict = fmt.Sprintf("B 显著**低于** A（%+.0f pp, p=%.4f）：记忆在这个任务上是有害的", diff, p)
	case p >= 0.05:
		an.BAVerdict = fmt.Sprintf("B 与 A 的差异不显著（%+.0f pp, p=%.3f, 95%% CI [%+.0f, %+.0f]）："+
			"数据不足以区分。**若 A 臂已接近地板，这是天花板效应，B 与 A 无法分开。**", diff, p, lo, hi)
	}
	// 天花板/地板效应：0→1 的跃迁上，任何"检验不显著"都是分辨率不足而不是无效应。
	if a.CorrectRate <= 0.25 && b.CorrectRate >= 0.75 {
		an.BAFloor = true
		an.BACeiling = true
	}
	if an.BAFloor {
		an.BAVerdict = fmt.Sprintf("%s 读法：%.0f%% → %.0f%%。p=%.4f 在 0→1 的跃迁上天然不显著，"+
			"因为 A 臂几乎没有随机性可言 —— 正确率的上界是 n=0，不是能力。结论应按**效应量**读，不按 p 读。",
			an.BAVerdict, 100*a.CorrectRate, 100*b.CorrectRate, p)
	}

	switch {
	case c.N == 0 || b.N == 0:
		an.CBVerdict = "INSUFFICIENT"
	case pCB < 0.05 && diffCB < 0:
		an.CBVerdict = fmt.Sprintf("C 显著低于 B（%+.0f pp, p=%.4f）：仓库里的貌似可信的相反取值压掉了记忆。", diffCB, pCB)
	case pCB < 0.05:
		an.CBVerdict = fmt.Sprintf("C 高于 B（%+.0f pp, p=%.4f）：模型在冲突时采信了仓库。", diffCB, pCB)
	default:
		an.CBVerdict = fmt.Sprintf("B 与 C 差异不显著（%+.0f pp, p=%.3f）：本实验分辨不开冲突处理。", diffCB, pCB)
	}

	an.Observation = fmt.Sprintf("A %.0f%%（n=%d）→ B %.0f%%（n=%d）→ C %.0f%%（n=%d）。"+
		"Step 0 的 89%% 是「答案在仓库里」时的无记忆基准，本文件的 A 臂是「答案无处可寻」时的基准；"+
		"增量必须相对各自自己的 A 臂来算。",
		100*a.CorrectRate, a.N, 100*b.CorrectRate, b.N, 100*c.CorrectRate, c.N)
	return an
}

func unanswerableLogAnalysis(t *testing.T, rep *unanswerableReport) {
	t.Helper()
	an := rep.Analysis
	t.Logf("=== 三臂汇总（n=%d/臂）===", rep.Reps)
	for _, s := range rep.Arms {
		t.Logf("  %-22s 正确 %5.0f%% (n=%d)  工具 %5.2f±%.2f  文档 %4.2f±%.2f  读工作区外 %.2f  判定 %v",
			s.Arm, 100*s.CorrectRate, s.N, s.ToolMean, s.ToolStd, s.DocsMean, s.DocsStd, s.OutsideMean, s.Verdicts)
	}
	t.Logf("")
	t.Logf("=== 判据一（B vs A）：记忆有没有用 ===")
	t.Logf("  A（无记忆）          %d/%d = %.0f%%", an.ACorrect, an.AN, 100*float64(an.ACorrect)/math1(an.AN))
	t.Logf("  B（记忆里有答案）    %d/%d = %.0f%%", an.BCorrect, an.BN, 100*float64(an.BCorrect)/math1(an.BN))
	t.Logf("  差值 B-A = %+.1f pp   95%% CI [%+.1f, %+.1f]   双侧 p = %.4f   MDE = %.1f pp",
		an.BADiffPP, an.BACILoPP, an.BACIHiPP, an.BAP, an.BAMDEPP)
	t.Logf("  → %s", an.BAVerdict)
	if an.BACeiling {
		t.Logf("  ⚠ 天花板效应：B 臂 %d/%d，A 臂 %d/%d。0→1 跃迁上 p 值天然不显著（A 臂几乎没有随机性），",
			an.BCorrect, an.BN, an.ACorrect, an.AN)
		t.Logf("    正确读法是看效应量与 Wilson 区间，不是看 p。")
	}
	t.Logf("")
	t.Logf("=== 判据二（C vs B）：冲突处理（与上面分开报，协议 §5.0）===")
	t.Logf("  C（仓库里有 decoy）  %d/%d = %.0f%%", an.CCorrect, an.CN, 100*float64(an.CCorrect)/math1(an.CN))
	t.Logf("  差值 C-B = %+.1f pp   95%% CI [%+.1f, %+.1f]   双侧 p = %.4f", an.CBDiffPP, an.CBCILoPP, an.CBHiPP, an.CBP)
	t.Logf("  → %s", an.CBVerdict)
	t.Logf("")
	t.Logf("=== 与 Step 0 的 89%% 基准的关系 ===")
	t.Logf("  %s", an.Observation)
	t.Logf("  Step 0 C 臂（无注入，答案在仓库里）= %.0f%%；本文件 A 臂（无记忆，答案无处可寻）= %.0f%%。",
		an.Step0CNoMemoryPP, 100*float64(an.ACorrect)/math1(an.AN))
	t.Logf("  「记忆提升多少」在本文件上的读数是 %.0f%% → %.0f%%，即 %+.0f pp；",
		100*float64(an.ACorrect)/math1(an.AN), 100*float64(an.BCorrect)/math1(an.BN), an.BADiffPP)
	t.Logf("  这个增量与 89%% 那个基准**不可加**：两个基准测的是两类不同任务。")
	t.Logf("")
	if rep.JudgeJudged > 0 {
		t.Logf("  判定器 %s 自洽率 %.0f%% (%d/%d，两序对调一致)；顺序不一致 %d 条",
			rep.JudgeModel, 100*rep.JudgeSelf, rep.JudgeJudged-rep.JudgeSwap, rep.JudgeJudged, rep.JudgeSwap)
		if rep.JudgeSelf <= 0.90 {
			t.Logf("  ⚠ 自洽率 ≤90%%：按协议 §10，**本次评估的所有结论作废**。上面的数字是仪器噪声。")
		}
	}
	t.Logf("")
	t.Logf("  样本量警告：n=%d/臂。协议 §8：n<12 时单档差异可能完全来自运行间波动；", rep.Reps)
	t.Logf("  本文件读的是**臂间**差异，同一模型、同一批夹具，但方差来源仍不可合并：")
	t.Logf("    ① 模型自身稳定性（逐次 0/1 序列见各臂 correctness_sequence）；")
	t.Logf("    ② 任务措辞（只有一个措辞，未做措辞变体，方差未被观测）；")
	t.Logf("    ③ decoy 强度（只有一个 decoy 措辞）；")
	t.Logf("    ④ 记忆 topic 的写法（只有一种写法）。协议 §8 点名的四个来源里只有 ① 被观测到。")
	t.Logf("    ⑤ 判定器自身的抖动已由两序对调的自洽率单列，但它与被测模型的抖动相互独立。")
	t.Logf("    ⑥ 装置与被测路径共用同一批工具实例，所以不存在「自检比被测宽松」的系统偏差。")
}

// math1 防除零：n=0 时返回 1，让百分比读数是 0 而不是 NaN/Inf。
func math1(n int) float64 {
	if n == 0 {
		return 1
	}
	return float64(n)
}

// supplementWriteReportSupplement2 供 wiring 测试复用（避免测试之间产生文件副作用）。
var _ = json.Marshal
