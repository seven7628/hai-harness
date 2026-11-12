package main

import (
	"encoding/json"
	"fmt"

	"github.com/seven7628/hai-harness/events"
)

// eventConstructors event_type → 空事件构造器（重放用）。
var eventConstructors = map[string]func() events.Event{
	"llm_start":               func() events.Event { return &events.LLMStart{} },
	"llm_end":                 func() events.Event { return &events.LLMEnd{} },
	"llm_error":               func() events.Event { return &events.LLMError{} },
	"reasoning_chunk":         func() events.Event { return &events.ReasoningChunk{} },
	"content_chunk":           func() events.Event { return &events.ContentChunk{} },
	"tool_run_start":          func() events.Event { return &events.ToolStart{} },
	"tool_run_end":            func() events.Event { return &events.ToolResponse{} },
	"tool_approval_requested": func() events.Event { return &events.ToolApprovalRequested{} },
	"agent_start":             func() events.Event { return &events.AgentStart{} },
	"agent_end":               func() events.Event { return &events.AgentEnd{} },
	"agent_user_message":      func() events.Event { return &events.AgentUserMessage{} },
	"compress_start":          func() events.Event { return &events.CompressStart{} },
	"compress_end":            func() events.Event { return &events.CompressEnd{} },
	"task_started":            func() events.Event { return &events.TaskStarted{} },
	"task_end":                func() events.Event { return &events.TaskEnd{} },
	"user_inputs_consumed":    func() events.Event { return &events.UserInputsConsumed{} },
	"session_run_error":       func() events.Event { return &events.SessionRunError{} },
	"session_closed":          func() events.Event { return &events.SessionClosed{} },
}

// parseEventLine 从 bridge 事件行解析 events.Event。
func parseEventLine(line string) (events.Event, error) {
	var raw struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, err
	}
	newEv, ok := eventConstructors[raw.EventType]
	if !ok {
		return nil, fmt.Errorf("unknown event type %q", raw.EventType)
	}
	ev := newEv()
	if err := json.Unmarshal([]byte(line), ev); err != nil {
		return nil, err
	}
	return ev, nil
}
