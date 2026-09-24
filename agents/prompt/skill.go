package prompt

// 技能相关注入层的标题文案(英文)。技能本身由上层 Registry 管理,
// 这里只定义注入层的固定文案(稳定字节)。
//
// 注意:技能清单标题是拼进系统提示词的固定字节,正文英文、此处中文仅作备注;
// 变更会改变系统提示词字节 → 影响前缀缓存,保持稳定。
const (
	// SkillManifestHeader 技能清单注入层标题(渐进披露的发现阶段:仅元数据)。
	// 用法:agents.agent_loop.buildSkillManifest 在系统提示词尾部追加清单。
	// 中文释义:"可用技能清单(任务匹配时,调用 load_skill 工具加载完整指令):"
	SkillManifestHeader = "\n\nAvailable skills (load full instructions via the load_skill tool when a task matches):\n"

	// LoadedSkillHeader 用户主动加载技能时的指令块标题(激活阶段:完整指令进上下文)。
	// 用法:agents.commands 的 skills load 命令 —— 标题 + skills.InstructionsBlock(s)
	// 组装成一条 **user 消息**追加进对话历史(core.NewSkillMessage),与模型 load_skill
	// 的工具结果同构,差别只在决策来源(用户 vs 模型)。
	// 为什么不拼进系统提示词:system 是 provider 缓存前缀的首段(tools → system →
	// messages),会话中途加层 = 已有整段上下文按全价重写一次缓存;历史尾插只增量写入。
	// 中文释义:"已加载技能(当前任务必须遵循):"
	LoadedSkillHeader = "Loaded skills (must be followed for the current task):\n"
)
