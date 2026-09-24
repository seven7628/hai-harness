package prompt

// 技能相关注入层的标题文案(英文)。技能本身由上层 Registry 管理,
// 这里只定义注入层的固定文案(稳定字节)。
//
// 注意:这些标题是拼进系统提示词的固定字节,正文英文、此处中文仅作备注;
// 变更会改变系统提示词字节 → 影响前缀缓存,保持稳定。
const (
	// SkillManifestHeader 技能清单注入层标题(渐进披露的发现阶段:仅元数据)。
	// 用法:agents.agent_loop.buildSkillManifest 在系统提示词尾部追加清单。
	// 中文释义:"可用技能清单(任务匹配时,调用 load_skill 工具加载完整指令):"
	SkillManifestHeader = "\n\nAvailable skills (load full instructions via the load_skill tool when a task matches):\n"

	// LoadedSkillHeader 已加载技能注入层标题(激活阶段:完整指令进上下文)。
	// 用法:agents.commands 追加已加载技能的完整指令。
	// 中文释义:"已加载技能(当前任务必须遵循):"
	LoadedSkillHeader = "Loaded skills (must be followed for the current task):\n"

	// LoadedSkillResourcesHeader 已加载技能的关联资源标题(可按需读取)。
	// 中文释义:"技能资源(可按需读取):"
	LoadedSkillResourcesHeader = "Skill resources (read on demand):\n"
)
