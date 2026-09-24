package prompt

// 自定义 subagent 注入层的标题文案(英文)。定义本身由上层 DefinitionRegistry 管理,
// 这里只定义注入层的固定文案(稳定字节)。
//
// 注意:这些标题是拼进系统提示词的固定字节,正文英文、此处中文仅作备注;
// 变更会改变系统提示词字节 → 影响前缀缓存,保持稳定。
const (
	// SubagentManifestHeader 自定义 subagent 清单注入层标题(渐进披露的发现阶段:仅元数据)。
	// 用法:agents.agent_loop.buildSubagentManifest 在系统提示词尾部追加清单。
	// 中文释义:"可用子代理清单(任务匹配时,调用 spawn_agent 工具派发):"
	SubagentManifestHeader = "\n\nAvailable subagents (spawn via the spawn_agent tool when a task matches):\n"
)
