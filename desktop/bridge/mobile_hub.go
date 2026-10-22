// mobile_hub.go —— 手机端事件扇出。
//
// 设计约束（见 docs/mobile-remote/TECH_PLAN.md §3.3）：
//   - 复用 emit 已经产出的那一行 JSON（注入好 session_id/workspace），不二次序列化；
//   - 订阅者慢（手机弱网/息屏）时**只丢流式 chunk**，绝不丢 agent_end / 审批 / 提问；
//   - 丢过就置 dropped，由写循环补一帧 {"type":"resync"} 让手机重拉快照。
package main

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// mobileSubBuf 每个订阅者的缓冲行数。256 行 ≈ 一次工具调用的完整事件量；
// 再大只是把内存换成延迟，没有收益。
const mobileSubBuf = 256

type mobileHub struct {
	mu   sync.RWMutex
	subs map[*mobileSub]struct{}
}

type mobileSub struct {
	ws  string // 空 = 不限工作区
	sid string // 空 = 不限会话
	ch  chan []byte
	// dropped 标记「这个订阅者漏过事件」。写循环看到它就先发 resync，
	// 再继续增量——手机据此重新 new_session 拉快照。
	dropped atomic.Bool
}

func newMobileHub() *mobileHub {
	return &mobileHub{subs: map[*mobileSub]struct{}{}}
}

func (h *mobileHub) subscribe(ws, sid string) *mobileSub {
	s := &mobileSub{ws: ws, sid: sid, ch: make(chan []byte, mobileSubBuf)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *mobileHub) unsubscribe(s *mobileSub) {
	if h == nil || s == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
	h.mu.Unlock()
}

// broadcast 把一行事件投给所有匹配的订阅者。非阻塞：宁可丢 chunk，也不拖住 agent 的 emit。
func (h *mobileHub) broadcast(ws, sid string, line []byte) {
	if h == nil || len(line) == 0 {
		return
	}
	chunk := isChunkEvent(line)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.ws != "" && s.ws != ws {
			continue
		}
		if s.sid != "" && s.sid != sid {
			continue
		}
		select {
		case s.ch <- line:
			continue
		default:
		}
		if chunk {
			s.dropped.Store(true)
			continue
		}
		// 控制事件（审批/提问/agent_end）：腾一格也要塞进去。
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- line:
		default:
			s.dropped.Store(true)
		}
	}
}

// drainControl 排空订阅者积压，只留下控制事件。
// 场景：订阅者丢过帧，服务端先补一帧 live 快照——快照已包含全部已产出的 chunk，
// 积压里的 chunk 再发一遍会让同一段文本出现两次。closed=true 表示通道已被 close
// （订阅者被移除），调用方应直接退出写循环。
func drainControl(sub *mobileSub) (kept [][]byte, closed bool) {
	for {
		select {
		case next, ok := <-sub.ch:
			if !ok {
				return kept, true
			}
			if !isChunkEvent(next) {
				kept = append(kept, next)
			}
		default:
			return kept, false
		}
	}
}

// isChunkEvent 流式增量（可丢、可重建）。用前缀扫描而不是反序列化：热路径上每秒几十次。
func isChunkEvent(line []byte) bool {
	t := eventTypeOf(line)
	return t == "content_chunk" || t == "reasoning_chunk"
}

// eventTypeOf 从 emit 注入过 session_id/workspace 的行里取 event_type。
// 注入发生在对象首部，所以不能假设 event_type 是第一个键。
func eventTypeOf(line []byte) string {
	const key = `"event_type":"`
	if i := bytes.Index(line, []byte(key)); i >= 0 {
		rest := line[i+len(key):]
		if j := bytes.IndexByte(rest, '"'); j >= 0 {
			return string(rest[:j])
		}
	}
	// 容错：JSON 编码器可能在冒号后留空格（当前不用 indent，纯防御）
	const key2 = `"event_type": "`
	if i := bytes.Index(line, []byte(key2)); i >= 0 {
		rest := line[i+len(key2):]
		if j := bytes.IndexByte(rest, '"'); j >= 0 {
			return string(rest[:j])
		}
	}
	return ""
}

// broadcastGlobal 应用级事件（不属于任何会话，如「电脑选目录」的结果）：
// 不受 workspace/session 过滤，投给所有在线手机。仍保持「控制事件优先」。
func (h *mobileHub) broadcastGlobal(line []byte) {
	if h == nil || len(line) == 0 {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		select {
		case s.ch <- line:
			continue
		default:
		}
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- line:
		default:
			s.dropped.Store(true)
		}
	}
}
