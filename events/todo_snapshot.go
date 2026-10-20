package events

// TodoSnapshot 待办清单的**事件层投影**（id/title/done/blocked_by）。
//
// 为什么在这里而不是直接复用 todo.Item：包依赖方向是 todo → tools → events，
// events 不能反向 import todo（成环）。事件层只需要这四列展示字段，映射在
// agents（发事件时投影）与宿主（落 checkpoint）各做一次。
//
// 语义：**发出该事件的运行自己的**待办清单。主 run → 会话待办；子 agent run →
// 它自己那份独立 store（子 agent 有独立 TODO，工程上见 agents.RunStream 的
// 「未注入则新建」与 subagent.buildOptions 刻意不继承父 Todo）。宿主据此把清单
// 归属到 run_id，而不是一律读会话级 store（那曾让子 agent 的 TODO 覆盖成主会话的）。
type TodoSnapshot struct {
	Id        string   `json:"id"`
	Title     string   `json:"title"`
	Done      bool     `json:"done"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}
