// Package prompt 是 go-code SDK 全部系统提示词的唯一定义地(规范化仓库)。
//
// 定位:SDK 拥有的提示词与产品层提示词模板统一收口,单一事实来源。
// 被 agents / skills / subagent / desktop / examples 引用;本包零依赖(纯常量)。
//
// 提示词规范(全文见 docs/PROMPT_STANDARD.md):
//
//  1. 语言:一律英文。主流模型对英文指令遵循度最高、token 化最稳;
//     用户语言由 base 契约层的 "Follow the user's language" 规则处理,
//     提示词本体不随用户语言变化。
//  2. 结构:Markdown 分节(## 前缀),先行为后风格,一条提示词一个职责。
//  3. 字节稳定:系统提示词是 provider 前缀缓存的稳定前缀,内容变更使缓存失效
//     (SDK 升级后首次请求变贵);稳定内容在前、动态内容在后(分层序见
//     agents.agent_loop.composeSystemPrompt)。
//  4. 归属:工具说明唯一来源是工具 schema(description),不进系统提示词;
//     系统提示词只放行为契约 / 产品 persona / 工作记忆(Agent.md)/ 技能元数据。
//  5. 压缩防丢失:安全敏感指令与全部 user 消息必须进入摘要(见 compact.go)。
//
// 变更提示词走版本管理;任何文案变更都会改变发送字节,需评估缓存影响。
package prompt
