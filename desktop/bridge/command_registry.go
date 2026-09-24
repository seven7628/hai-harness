// command_registry.go —— 命令类型化基础设施（V2 P2-PROTOCOL-03/04 增量落地）。
//
// 目标：消除「Type + map[string]any」裸字符串漂移的第一层——命令名常量表 +
// payload 类型化访问。命令名与前端共用同一字面量（bridge 是协议权威），
// 拼写错误在编译期暴露（引用常量而非字符串字面量）。
//
// 迁移策略（增量）：新命令一律用常量；存量命令按批次迁移（provider/会话路由优先）。
// 完整 schema/codegen（Go↔TS 单一协议源）为 Phase 3 目标，见 docs/CODEBASE_REVIEW_V2.md。
package main

// Command 命令名常量（bridge 协议权威；前端 transport 命令字面量应与此一致）。
// 命名：小写下划线，与 dispatch switch 的 case 字面量一一对应。
type Command string

const (
	// —— 会话与运行 ——
	CmdNewSession    Command = "new_session"
	CmdList          Command = "list"
	CmdAsk           Command = "ask"
	CmdAskBatch      Command = "ask_batch"
	CmdInterrupt     Command = "interrupt"
	CmdInterruptTask Command = "interrupt_task"
	CmdApprove       Command = "approve"
	CmdCompact       Command = "compact"
	CmdDeleteSession Command = "delete_session"
	CmdSessionPin    Command = "session_pin"

	// —— 会话配置热切换 ——
	CmdSwitchMode     Command = "switch_mode"
	CmdSwitchEffort   Command = "switch_effort"
	CmdSwitchPersona  Command = "switch_persona"
	CmdSwitchModel    Command = "switch_model"
	CmdSetProvider    Command = "set_provider"
	CmdReloadSettings Command = "reload_settings"
	// CmdSetUILang 界面语言热通知（settings.lang 变化 → 仅刷新 Hub mesh 文案语言，不重建 loop）。
	CmdSetUILang Command = "set_ui_lang"
	// CmdMeshStatus 查询 Session Mesh 状态（本机身份 + 可连接地址 + 已连节点 + TLS），
	// 供设置页「别人怎么连我」展示与排障。
	CmdMeshStatus Command = "mesh_status"
	// CmdMeshPing 主动 ping 一个已连/出站节点（连接检测；设置页「检测连接」按钮）。
	CmdMeshPing Command = "mesh_ping"

	// —— Provider 管理 ——
	CmdListModels          Command = "list_models"
	CmdTestConnection      Command = "test_connection"
	CmdListProviderPresets Command = "list_provider_presets"

	// —— OAuth ——
	CmdOAuthLogin        Command = "oauth_login"
	CmdOAuthLogout       Command = "oauth_logout"
	CmdOAuthStatus       Command = "oauth_status"
	CmdOAuthPromptAnswer Command = "oauth_prompt_answer"

	// —— 工具/技能/MCP/插件 ——
	CmdFilePreview Command = "file_preview"
	// CmdXlsxPreview 工作簿（.xlsx/.xlsm）→ HTML 预览：受管 Python + openpyxl 转换，
	// 产物是临时文件的绝对路径，前端拿它走内嵌浏览器 file://（与 PDF 同一条渲染通道）。
	CmdXlsxPreview Command = "xlsx_preview"
	// CmdDocxPreview 文档（.docx/.docm）→ HTML 预览：受管 Python + mammoth 转换
	//（语义 HTML：标题/粗斜体/列表/表格/内嵌图片），产物与通道同 CmdXlsxPreview。
	CmdDocxPreview Command = "docx_preview"
	// CmdPptxPreview 演示文稿（.pptx/.pptm）→ HTML 预览：受管 Python + python-pptx
	// 按真实 EMU 几何做**近似**版式重建（LibreOffice 不可用，做不到渲染 —— 页面顶部有
	// 明确标注，见 builtin.PptxPreviewer）。产物与通道同上。
	CmdPptxPreview Command = "pptx_preview"
	CmdSkills      Command = "skills"
	// CmdContextBreakdown 上下文构成（Composer ctx 环 hover 面板）：静态分区
	//（系统提示词/技能/工具/MCP/其他）按请求组装字节**固定**展示，消息分区取
	//「锚点 − 静态合计」残差（口径见 docs/CTX_ACCOUNTING_FIX_2026-09-20.md）。
	CmdContextBreakdown Command = "context_breakdown"
	CmdSkillToggle      Command = "skill_toggle"
	CmdSkillGet         Command = "skill_get"
	CmdLoadSkill        Command = "load_skill"
	CmdCommand          Command = "command"
	CmdMCPList          Command = "mcp_list"
	CmdMCPSet           Command = "mcp_set"
	CmdMCPRefresh       Command = "mcp_refresh"
	CmdMCPReload        Command = "mcp_reload" // 重读配置分层（用户 + 项目）→ 热应用 + 重建本工作区会话
	// CmdMCPProjects 多项目项目层汇总（MCP 页「项目」区；payload.workspaces = 前端绑定的项目）。
	// **不创建运行态**：只为查看而读盘，有运行态的项目才附连接状态/工具数。
	CmdMCPProjects Command = "mcp_projects"
	// CmdMCPProjectSet 写某项目的项目层 MCP（{ws}/.go-code/settings.json 的 mcpServers 段）：
	// 校验同 mcp_set；有运行态 → 立即 reloadMCP 热应用，无 → 写盘即生效（下次打开读取）。
	CmdMCPProjectSet Command = "mcp_project_set"
	// CmdHooksList / CmdHooksSet / CmdHooksTrust 钩子面板三件套（见 hooks_commands.go）：
	// 来源汇总（不创建运行态）/ 写某一层（严格校验 + 热替换，下一次 Run 生效）/ 记录信任（即刻生效）。
	CmdHooksList       Command = "hooks_list"
	CmdHooksSet        Command = "hooks_set"
	CmdHooksTrust      Command = "hooks_trust"
	CmdPluginList      Command = "plugin_list"
	CmdPluginInstall   Command = "plugin_install"
	CmdPluginUninstall Command = "plugin_uninstall"
	CmdPluginEnable    Command = "plugin_enable"
	CmdPluginDisable   Command = "plugin_disable"

	// —— Git ——
	CmdGitSnapshot   Command = "git_snapshot"
	CmdGitFileDiff   Command = "git_file_diff"
	CmdGitHistory    Command = "git_history"
	CmdGitCommitDiff Command = "git_commit_diff"
	CmdGitStage      Command = "git_stage"
	CmdGitUnstage    Command = "git_unstage"
	CmdGitDiscard    Command = "git_discard"
	CmdGitCommit     Command = "git_commit"
	CmdGitBranchList Command = "git_branch_list"
	CmdGitCheckout   Command = "git_checkout"
	CmdGitInit       Command = "git_init"

	// —— 工作区/系统 ——
	CmdWorkspaceList   Command = "workspace_list"
	CmdDeleteWorkspace Command = "delete_workspace"
	CmdShutdown        Command = "shutdown"
	CmdMetrics         Command = "metrics"
	// CmdRefreshPrices 用实时价源（models.dev / OpenRouter 自报价）补「无价」模型的价，
	// 免发版（内置价表是编译期快照）。详见 desktop/bridge/price_refresh.go。
	CmdRefreshPrices Command = "refresh_prices"
	// CmdTrace 历史执行链路（trace）投影：读会话事件日志按需投影成 trace
	//（列表 / 详情两粒度），不落盘、零新增存储。见 desktop/bridge/trace.go 与
	// docs/TRACE_CHAIN_REDESIGN.md。
	CmdTrace Command = "trace"
)

// String 命令名字符串（dispatch switch 用）。
func (c Command) String() string { return string(c) }
