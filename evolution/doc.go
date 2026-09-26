// Package evolution 是 HAI Memory 自进化的 Phase B/C/D 控制面。
//
// 位置：与 Phase A 内核包 memory/ **平级**，不放在 memory/ 下。原因是 Phase A 的
// 内核必须永远只做"仓库记忆的读写"，一旦把候选、门禁、生命周期塞进去，
// memory/ 就开始依赖 hosts 概念（任务、评估器、技能、宿主注入的 provider），
// 而 Phase A 的价值恰恰是"简单、低侵入、无依赖"。因此：
//
//	memory/      Phase A 内核（纯标准库，Repository-scoped Markdown Auto Memory）
//	evolution/   Phase B/C/D（本包，标准库 + yaml/memory，宿主可插拔）
//	skills/      Skill 标准结构（Phase D 只**参考**它，不被本包反向依赖）
//
// # 阶段与链式依赖
//
// 三阶段严格链式（A → B → C → D），每阶段只在上游门禁通过后进入：
//
//	Phase B  Candidate 临时存储 + 静态验证链 + 单次 Replay + 验证失败不进可召回索引
//	Phase C  Outcome + Confidence + Stale/Supersede/Expire/Archive + Rollback + 负证据
//	Phase D  MemoryRefs + Skill Candidate + 联合 Replay + Skill 负迁移评估
//
// Phase C 复用 Phase B 的 Candidate 落盘格式与门禁机制（不另起一套状态机）；
// Phase D 需要 B + C 双侧成熟（技能候选同样要走门禁，负迁移评估同样要靠 Outcome）。
//
// # 不可变的 Phase A 契约（反向约束）
//
// 本包**不得**改变 Phase A 的以下任何一条；每条都在 *_test.go 里有对应的不变量测试：
//
//  1. Repository-scoped：候选与记录都挂在 memory.Store.Dir()（= repos/<id>/）之下，
//     目录名由 Phase A 的仓库身份决定，跨仓库不串。
//  2. bounded Index：候选不写 MEMORY.md；只有 Promote 成功后才由 Phase A 的
//     Store.WriteIndex 写入，200 行 / 25KB 限额由 Phase A 强制。
//  3. Topic JIT：候选正文**不进入**系统提示词；memory_view 只读 topics/，因此
//     未晋级的候选结构上不可被召回。
//  4. Agent 自主读取：本包不新增工具、不改 tools 注册表，接线由宿主决定。
//  5. Subagent 默认隔离：本包不感知 Depth，不放宽 Phase A 的 tools.go 拒斥逻辑。
//
// # Phase B 明确不做（越界即为回归）
//
//	多代 Curator / 复杂 Evidence 图谱 / 自动 Supersede / 自动 Rollback /
//	Memory+Skill 自动生成
//
// # Phase C 禁止自改的对象（本包在结构上无法修改它们）
//
//	评估器、权限、Sandbox、Hook Trust、MCP Trust、Provider 翻译、凭证、插件信任。
//	本包只写 memory/repo 与 evolution/ 目录，没有任何写这些系统的代码路径；
//	并且 BoundaryValidator 会**拒绝**建议修改这些对象的候选内容。
//
// # Replay 默认不做真实 LLM 调用
//
// Replayer 是可注入接口（见 replay.go）。未注入时 Verify 跳过 Replay 并把
// Candidate 标记为 replay_skipped；宿主接入真实 provider 后由宿主注入实现。
// 默认路径不产生任何外部调用，因此可以跑通确定性测试。
//
// # 自动 vs 显式
//
// 本包**没有后台 goroutine、没有定时器、没有自动生命周期推进**：Stale /
// Supersede / Expire / Archive / Rollback 全部是宿主显式调用的操作。
// 理由：自动淘汰需要长期 outcome 数据支撑，Phase C 只建立记录与判定口径，
// 何时推进由宿主根据真实任务评估决定（见评估协议第 9 节）。
package evolution
