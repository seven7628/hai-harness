## Event事件定义

### Event 含义与作用
- Event 为唯一交互标准，从llm流式输出 --> UI --> Sessions --> AgentRun
- 主要涵盖完整生命周期所有事件，不支持自定义；


### EventType 定义

#### 基础类型

- agent_start
- llm_start
- content_delta
- reasoning_delta
- tool_call_start
- tool_call_delta
- tool_call_end
- llm_end
- agent_end

#### 其他类型(待补充)


------------

首先 LLM 只会返回 AssistantMessage 或者 ToolCall 或者 ImageContent(多模态模型输出)

AssistantMessage 又分为 Thinking / Content;

LLM 还会返回Usage、StopReason；


------------


### LLM 相关 Event

```go
package events

type AssistantContent struct {
    Type string // content
    Content string // 完整Assistant内容
}

type AssistantContentDelta struct {
    Type string  // content_delta 
    Content string  // 真实的Assistant内容流式输出
}


type ReasoningContent struct {
    Type string  // reasoning
    Content string  // 完整的思考过程
}

type ReasoningContentDelta struct {
    Type string  // reasoning_delta
    Content string  // 思考过程流式输出
}

type ToolCall struct {
    Id string
    Name string
    Arguments string
}

type ToolCallDelta struct {
    Id string
    Name string
    Arguments string
}


type LLMStart struct {
    Type string // llm_start
}

type LLMEnd struct {
    Reasoning string
    Content string
    ToolCalls []ToolCall
    
    Usage Usage
}

type Usage struct {
    Input int64 // 输入Token
    Output int64 // 输出Token
    CacheRead int64 // 缓存读取 Token
    CacheWrite int64 // 缓存写 Token

    Reasoning int64  // 思考过程占用多少Token
    TotalTokens int64 // 总Token

    Cost Cost // 计费相关
}

// 每百万Token多少钱
type Cost struct {
    Input int64  
    Output int64
    CacheRead int64
    CacheWrite int64
    Total int64
}
```

### Agent相关Event

```go
package events


type AgentStart struct {
	Type string // agent_start
}

type AgentEnd struct {
	Type string // agent_end

	Reasoning string
	Content   string
	ToolCalls []ToolCall // 引用上边 LLM 的ToolCall
    
    Usage Usage // 引用上边 LLM 的Usage结构
}

// 后续拓展的压缩事件: 开始压缩，压缩结束?

```
