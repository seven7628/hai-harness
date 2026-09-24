package session

import (
	"context"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"strings"
)

// 命令框架：产品/用户主动触发 SDK 内部操作（compact / skills / 产品自定义命令）。
//
// 通道形态：内部指令消息（core.NewCommandMessage，Content.Type="command"）经 inbox
// 注入——运行中唤醒当前轮插话检查点消费、空闲排队下个 Run 首轮消费（复用插话
// 基础设施，零新唤醒机制）。消费点在 agents 注入段（injectCommands）：剥出执行、
// 不送 LLM、不进持久化链（崩溃恢复不重放——恢复丢指令可接受）。
//
// 与斜杠命令边界：解析/UI 是产品层；Session.Command 是执行通道。

// CommandFunc 自定义命令处理器（Session.RegisterCommand 注册）。
// 命令在 AgentLoop 注入段异步执行（Command 纯 enqueue 立即返回），执行结果经
// CommandResult 事件回传；失败不 kill agent 主流程（带外操作语义）。
type CommandFunc func(ctx context.Context, s *Session, args string) error

// builtinCommandNames 内置命令名：走内核路径（compact/skills/clear），注册表拒绝覆盖。
var builtinCommandNames = map[string]bool{"compact": true, "skills": true, "clear": true}

// RegisterCommand 注册自定义命令；重复注册与覆盖内置命令名（compact/skills）报错。
func (s *Session) RegisterCommand(name string, fn CommandFunc) error {
	if name == "" || strings.ContainsAny(name, "\n\r ") {
		return fmt.Errorf("invalid command name %q (must be non-empty, no whitespace)", name)
	}
	if builtinCommandNames[name] {
		return fmt.Errorf("builtin command %q cannot be overridden", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commands == nil {
		s.commands = make(map[string]CommandFunc)
	}
	if _, ok := s.commands[name]; ok {
		return fmt.Errorf("command %q already registered", name)
	}
	s.commands[name] = fn
	return nil
}

// Command 主动触发命令（纯 enqueue，立即返回，执行结果经 CommandResult 事件回传）：
//   - compact：手动压缩（本轮结算段强制压缩，CompressStart/End 事件感知）
//   - skills / skills load <name>：发现列表 / 强制加载技能进 system
//   - 自定义：注册表命中 → 注入执行；miss 报错
//
// 消息经 inbox 注入（跳过 Ask 的角色校验与 history 记录）：指令消息不持久化，
// 崩溃恢复后不重放执行（恢复丢指令可接受）。
func (s *Session) Command(ctx context.Context, name, args string) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	if !builtinCommandNames[name] {
		s.mu.Lock()
		fn := s.commands[name]
		s.mu.Unlock()
		if fn == nil {
			return fmt.Errorf("unknown command %q", name)
		}
	}
	msg := core.NewCommandMessage(name, args)
	select {
	case s.inbox <- msg:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrQueueFull
	}
	return nil
}

// dispatchCommand 自定义命令执行器：注入 AgentLoop.RunOptions.CommandHandler
// （agents 层剥出非内置命令后回调；agents 包不依赖 session，经闭包注入）。
func (s *Session) dispatchCommand(ctx context.Context, name, args string) error {
	s.mu.Lock()
	fn := s.commands[name]
	s.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("unknown command %q", name)
	}
	return fn(ctx, s, args)
}

// filterCommandMessages 剔除内部指令消息（持久化前调用）：指令消息不落盘——
// State.Messages 与 State.Pending 均过滤（崩溃恢复不重放执行）。
func filterCommandMessages(msgs []core.Message) []core.Message {
	out := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.IsCommandMessage() {
			continue
		}
		out = append(out, m)
	}
	return out
}
