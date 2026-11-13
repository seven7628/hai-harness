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
	// CmdAgentHistory 子智能体完整历史分页（agent 卡片展开时按需拉取）。
	//
	// 背景：agent 卡片的 items 不再整份进 checkpoint（那曾占 record 体积 98%，
	// 让每次提交要序列化近百 MB）。checkpoint 只留最近一屏，更早的从这里按页取。
	CmdAgentHistory Command = "agent_history"
	// CmdAgentSend 用户直接对某个子智能体说话（子会话视图的输入框）。
	//
	// 与模型调用的 agent_send 工具**同一条通道**（Registry.Send → Task.inbox）：
	// 子智能体是独立上下文，「我想让它改一下」和模型再派它一次不是一件事。
	// 语义与工具一致：仅运行中的任务可投；已结束要 agent_resume，未知任务报错。
	CmdAgentSend Command = "agent_send"
	// CmdBlocksPage 主对话历史分页（「加载更早」）。
	//
	// 与 agent_history 同构，但取自**进程内 reducer** 而不是子 agent journal：
	// 打开会话时 bridge 可用 blocks_limit 只回最近一屏（见 main.go new_session），
	// 更早的从这里按 id 游标向前取。游标用 block id 而非 offset —— id 单调唯一，
	// 新块追加不会让用户正在看的内容跳位。
	CmdBlocksPage    Command = "blocks_page"
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

	// —— Agent Hub（P1：智能体 CRUD） ——
	// 命令面按 docs/AGENT_HUB_TECH_PLAN.md §7.1.2 命名；hub_* 一律由 dispatchHub 分派。
	CmdHubAgentCreate  Command = "hub_agent_create"
	CmdHubAgentUpdate  Command = "hub_agent_update"
	CmdHubAgentGet     Command = "hub_agent_get"
	CmdHubAgentList    Command = "hub_agent_list"
	CmdHubAgentDelete  Command = "hub_agent_delete"
	CmdHubAgentRestore Command = "hub_agent_restore" // 从回收站恢复（§5.2 软删可恢复）
	// —— Agent Hub（P1：人格版本 / 回滚 / 提示词预览） ——
	CmdHubAgentVersions Command = "hub_agent_versions"       // 历史版本清单
	CmdHubAgentRollback Command = "hub_agent_rollback"       // 回滚到某一版（新版本落地，不丢当前）
	CmdHubPromptPreview Command = "hub_agent_prompt_preview" // 系统提示词分层预览
	// —— Agent Hub（P1：记忆面板） ——
	CmdHubAgentMemory      Command = "hub_agent_memory"       // 列表 / 读一个文件
	CmdHubAgentMemoryWrite Command = "hub_agent_memory_write" // 写（写前备份）
	// —— Agent Hub（P1：DM 会话） ——
	CmdHubOpen    Command = "hub_open"
	CmdHubSend    Command = "hub_send"
	CmdHubHistory Command = "hub_history"
	// CmdHubMarkRead 推进已读游标（左栏未读点，§7.1.1）：group_id → 群级游标；
	// agent_id → DM 游标。两个只给一个。
	CmdHubMarkRead Command = "hub_mark_read"
	// —— Agent Hub（P2：任务板 + Needs you） ——
	CmdHubTaskList   Command = "hub_task_list"
	CmdHubTaskGet    Command = "hub_task_get"
	CmdHubTaskCancel Command = "hub_task_cancel"
	CmdHubTaskRetry  Command = "hub_task_retry"
	// CmdHubTaskArtifactPreview 任务卡产物的人类只读预览（task_id + path 白名单寻址）。
	// 只给本地桌面 UI；不要加进 mobile_server 白名单。
	CmdHubTaskArtifactPreview Command = "hub_task_artifact_preview"
	CmdHubNeedList            Command = "hub_need_list"
	CmdHubNeedResolve         Command = "hub_need_resolve"
	CmdHubClear               Command = "hub_clear" // 清空 DM 上下文（保留 system，§6.4）
	// —— Agent Hub（P3a：群组基础） ——
	CmdHubGroupList         Command = "hub_group_list"
	CmdHubGroupGet          Command = "hub_group_get"
	CmdHubGroupCreate       Command = "hub_group_create"
	CmdHubGroupUpdate       Command = "hub_group_update"
	CmdHubGroupDelete       Command = "hub_group_delete"
	CmdHubGroupMemberAdd    Command = "hub_group_member_add"
	CmdHubGroupMemberRemove Command = "hub_group_member_remove"
	CmdHubGroupHistory      Command = "hub_group_history"
	CmdHubGroupActivity     Command = "hub_group_activity"
	CmdHubGroupSend         Command = "hub_group_send"
	// CmdHubGroupAssign 人从群 UI 把一条可追踪任务派给成员（P3b-07a，§6.7.9）：
	// 与智能体 assign_task 同一条路径，但 Origin=human、指派消息 Sender=human。
	CmdHubGroupAssign Command = "hub_group_assign"
	// CmdHubConversationStop 人类「停止自动对话」：中断当前群 run + 清队列 + stopped（§6.5.6 规则 6）。
	CmdHubConversationStop Command = "hub_conversation_stop"
	// CmdHubConversationReset 人类「重置本群限制」（P6-04 ④）：立即清零 pair/深度/指纹
	// 并解除 waiting_human，不自动唤醒任何人。
	CmdHubConversationReset Command = "hub_conversation_reset"
	CmdHubArtifactGet       Command = "hub_artifact_get"
	// CmdHubSearch 人类全历史搜索（群 + DM，P4-04b-1）。**人类命令**：智能体侧没有
	// 对应工具（agent 只能 group_history(query) 搜自己参与的群），所以命令面就是边界。
	CmdHubSearch Command = "hub_search"

	// —— Provider 管理 ——
	CmdListModels          Command = "list_models"
	CmdTestConnection      Command = "test_connection"
	CmdListProviderPresets Command = "list_provider_presets"
	// CmdProviderBilling 查余额 / 配额窗口（Provider 设置页的计量区）。
	// 数据面见 provider/billing；payload {provider?, refresh?}；结果以 Snapshot 返回，
	// 单家失败以 error_code 落在各自快照上（命令本身 ok=true）。
	CmdProviderBilling Command = "provider_billing"

	// —— OAuth ——
	CmdOAuthLogin        Command = "oauth_login"
	CmdOAuthLogout       Command = "oauth_logout"
	CmdOAuthStatus       Command = "oauth_status"
	CmdOAuthPromptAnswer Command = "oauth_prompt_answer"

	// —— 工具/技能/MCP/插件 ——
	CmdFilePreview Command = "file_preview"
	// —— 内嵌编辑器读写通道（实现见 file_edit.go；docs/creator-mode-v0.1/03-code-plan.md §2.7）——
	//
	// 与 file_preview 的分工：preview 产出**预览**（可截断、可转换），edit 产出**原文 +
	// sha256**——编辑器要能原样写回，所以必须拿到字节级真相（含行尾，不做规范化）。
	//
	// CmdFileOpen  {path}                    → {ok, path, content, hash, size, mtime, truncated?, binary?}
	// CmdFileSave  {path, content, baseHash} → {ok, hash, mtime}；错误码 conflict/not_found/too_large/denied
	// CmdFileClose {path}                    → 只退订（无写副作用）：关闭编辑器标签时上报
	CmdFileOpen  Command = "file_open"
	CmdFileSave  Command = "file_save"
	CmdFileClose Command = "file_close"
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
	// CmdOfficeKitStatus 办公组件（LibreOffice kit）状态（R08d，实现见 office.go）。
	// payload {refresh?: bool} → {kit:{available,state,source,bin,root,version,platform,
	// sizeBytes,license,licenseUrl,sourceUrl,reason,errors,platforms,pinnedVersion}}。
	// 不可用**不是错误**（ok:true + available:false）：它正是「走近似渲染 + 横幅」的依据。
	CmdOfficeKitStatus Command = "office_kit_status"
	// CmdMediaProbe 音视频元信息 + 直放判断（R08c，实现见 media_probe.go / media_matrix.go）。
	// payload {path, workspace?} → {path, size, modified_ms, available, state, duration_ms,
	// bit_rate, streams, playback{mode,kind,container,video_codec,audio_codec,reason,probed}, cached}。
	// 两个**不是错误**的降级：ffprobe 不存在 → available:false；ffprobe 超时/异常 → state:"failed"。
	// 两种情况都仍然给出按容器默认的 playback（能播的照样能播，只是元信息缺失）。
	CmdMediaProbe Command = "media_probe"
	CmdSkills     Command = "skills"
	// CmdContextBreakdown 上下文构成（Composer ctx 环 hover 面板）：静态分区
	//（系统提示词/技能/工具/MCP/其他）按请求组装字节**固定**展示，消息分区取
	//「锚点 − 静态合计」残差（口径见 docs/CTX_ACCOUNTING_FIX_2026-09-20.md）。
	CmdContextBreakdown Command = "context_breakdown"
	CmdSkillToggle      Command = "skill_toggle"
	// CmdMemorySettings / CmdMemorySetEnabled Project Memory 开关（设置面板）。
	// 生效时机 = **下一个会话**（或 switch_model / switch_persona / set_provider
	// 重建 loop 之后）；运行中的会话不改变，理由见 memory_prefs.go。
	CmdMemorySettings   Command = "memory_settings"
	CmdMemorySetEnabled Command = "memory_set_enabled"
	// CmdMemoryBrowse 浏览 Project Memory 文件（官方 §2.7 的 `/memory`：
	// "Run `/memory` to browse and open memory files from within a session"）。
	//
	// **纯读**（官方承诺"随时可编辑或删除"→ 宿主只列，不做校验也不提供写路径）。
	// 无 payload；响应带 Listing（目录 / 索引是否存在 / 文件清单）。
	CmdMemoryBrowse Command = "memory_browse"
	CmdSkillGet     Command = "skill_get"
	CmdLoadSkill    Command = "load_skill"
	CmdCommand      Command = "command"
	CmdMCPList      Command = "mcp_list"
	CmdMCPSet       Command = "mcp_set"
	CmdMCPRefresh   Command = "mcp_refresh"
	CmdMCPReload    Command = "mcp_reload" // 重读配置分层（用户 + 项目）→ 热应用 + 重建本工作区会话
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
	CmdWorkspaceAdd    Command = "workspace_add"
	CmdWorkspaceRemove Command = "workspace_remove"
	// CmdWorkspaceSync 渲染进程首次迁移：把 localStorage 的旧项目列表灌进
	// ~/.go-code/workspaces.json（只增不减，避免覆盖手机刚加的工作区）。
	CmdWorkspaceSync Command = "workspace_sync"
	// CmdWorkspaceUpdate 重命名 / 置顶工作区（手机端管理页）。
	CmdWorkspaceUpdate Command = "workspace_update"

	// —— 手机端接入（见 mobile_server.go）——
	CmdAnswerQuestion Command = "answer_question"
	CmdMobileStatus   Command = "mobile_status"
	CmdMobileRevoke   Command = "mobile_revoke"
	// CmdSessionRename 手动重命名会话（跨端共享；空标题 = 清除覆盖）。
	CmdSessionRename Command = "session_rename"
	// CmdMobileSetListen 切换监听范围（回环 / 局域网）。桌面端专属：手机不能自己放开。
	CmdMobileSetListen Command = "mobile_set_listen"
	// CmdWorkspacePick / CmdWorkspacePickResult：「让电脑弹目录选择器」的两端。
	// 手机只发 request；result 由 Electron 主进程回填（不在手机白名单）。
	CmdWorkspacePick       Command = "workspace_pick_request"
	CmdWorkspacePickResult Command = "workspace_pick_result"
	CmdDeleteWorkspace     Command = "delete_workspace"
	CmdShutdown            Command = "shutdown"
	CmdMetrics             Command = "metrics"
	// CmdRefreshPrices 用实时价源（models.dev / OpenRouter 自报价）补「无价」模型的价，
	// 免发版（内置价表是编译期快照）。详见 desktop/bridge/price_refresh.go。
	CmdRefreshPrices Command = "refresh_prices"
	// CmdTrace 历史执行链路（trace）投影：读会话事件日志按需投影成 trace
	//（列表 / 详情两粒度），不落盘、零新增存储。见 desktop/bridge/trace.go 与
	// docs/TRACE_CHAIN_REDESIGN.md。
	CmdTrace Command = "trace"
	// CmdUI 渲染层 → 桥的 UI 快照通道（PR-12）。
	// **不是命令**：不回 command_response、不进 events.Event 28 类、不落盘、不重放。
	// payload = {op, data}，op ∈ {snapshot, surface_status}；未知 op 静默忽略。
	// 见 desktop/bridge/ui_state.go 与 03-code-plan §3.1。
	CmdUI Command = "ui"
	// CmdSurfaceCommand 插件调用宿主只读命令（PR-14 D）。
	// payload = {id, name, args?}；白名单与审计在 Go（surfaces_command.go），
	// 渲染层的预检只是省一次 IPC，不是边界。
	CmdSurfaceCommand Command = "surface_command"
)

// String 命令名字符串（dispatch switch 用）。
func (c Command) String() string { return string(c) }
