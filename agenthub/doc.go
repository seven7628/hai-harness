// Package agenthub 是 Agent Hub 的领域层：智能体、群组、消息、任务账本。
//
// 这一层不依赖 desktop/bridge，也不做 I/O；它只描述“Hub 里有什么”和“状态怎么变”。
// 存储、会话装配、事件投影、前端协议全部在 desktop/bridge 与 desktop/app 里完成。
// 这样 Hub 的核心规则可以脱离 Electron、provider、UI 单独测试。
package agenthub
