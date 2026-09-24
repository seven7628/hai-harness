// Package todo 会话级待办清单：长时运行的任务状态锚点。
//
// 设计（方案 B：独立于对话上下文）：
//   - todo 挂在 Session 状态上（State.Todo），随快照持久化 ——
//     压缩/摘要不涉及，崩溃恢复后精确还原（压缩边界引擎注入真实清单，模型无需读工具）；
//   - 工具实例无状态：每次执行从 context 动态获取会话 Store（WithStore 注入，
//     AgentLoop 在工具轮注入 ac.Todo），因此 todo 工具可跨 Session 共享注册；
//   - 依赖（blocked_by）为信息性提示：模型声明依赖关系，列表展示阻塞状态，
//     不做执行级强制（不限制模型灵活性）。
package todo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"sync"
)

// storeCtxKey context 值键（独立于 events.ToolContext，避免包依赖环）。
type storeCtxKey struct{}

// WithStore 将会话待办 Store 注入工具执行 context（AgentLoop 工具轮调用）。
func WithStore(ctx context.Context, s *Store) context.Context {
	return context.WithValue(ctx, storeCtxKey{}, s)
}

// StoreFrom 取出会话待办 Store；未注入（无会话场景）返回 nil。
func StoreFrom(ctx context.Context) *Store {
	s, _ := ctx.Value(storeCtxKey{}).(*Store)
	return s
}

// Item 一条待办。
type Item struct {
	Id        string   `json:"id"`                   // 简短 id（t1/t2/...）
	Title     string   `json:"title"`                // 内容
	Done      bool     `json:"done"`                 // 是否完成
	BlockedBy []string `json:"blocked_by,omitempty"` // 依赖的 todo id
}

// Store 会话级待办列表（并发安全）。
type Store struct {
	mu    sync.Mutex
	items []Item
	next  int
}

func New() *Store { return &Store{next: 1} }

// Restore 从持久化快照恢复（NewSession 时调用）。
func (s *Store) Restore(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]Item{}, items...)
	s.next = 1
	for _, it := range s.items {
		if n := idNum(it.Id); n >= s.next {
			s.next = n + 1
		}
	}
}

// Clear 清空全部待办（压缩边界用：压缩后 todo 状态不跨压缩持久，改为把压缩前的
// 清单快照以 system-reminder 注入上下文，需要新待办由模型调用 todo_add/todo_update
// 重建——见 agents/prompt/reminder.go CompactTodoReminder）。
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = nil
	s.next = 1
}

// Items 返回全部待办的副本（持久化快照用）。
func (s *Store) Items() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Item{}, s.items...)
}

// Add 添加待办；blockedBy 引用不存在的 id 时报错（模型可见可修正）。
func (s *Store) Add(title string, blockedBy []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(title) == "" {
		return "", errors.New("todo: title is required")
	}
	if err := s.checkDepsLocked(blockedBy); err != nil {
		return "", err
	}
	id := fmt.Sprintf("t%d", s.next)
	s.next++
	s.items = append(s.items, Item{Id: id, Title: title, BlockedBy: append([]string{}, blockedBy...)})
	return id, nil
}

// Update 更新待办（title/done/blockedBy 传 nil 表示不变）。
func (s *Store) Update(id string, title *string, done *bool, blockedBy *[]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].Id != id {
			continue
		}
		if blockedBy != nil {
			if err := s.checkDepsLocked(*blockedBy); err != nil {
				return err
			}
			s.items[i].BlockedBy = append([]string{}, *blockedBy...)
		}
		if title != nil {
			if strings.TrimSpace(*title) == "" {
				return errors.New("todo: title cannot be empty")
			}
			s.items[i].Title = *title
		}
		if done != nil {
			s.items[i].Done = *done
		}
		return nil
	}
	return fmt.Errorf("todo: id %q not found", id)
}

// checkDepsLocked 校验依赖 id 均存在（持锁调用）。
func (s *Store) checkDepsLocked(ids []string) error {
	known := map[string]bool{}
	for _, it := range s.items {
		known[it.Id] = true
	}
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("todo: blocked_by %q does not exist", id)
		}
	}
	return nil
}

// List 渲染待办清单（模型可读）：open/done 计数 + 依赖阻塞标记。
func (s *Store) List() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	open, done := 0, 0
	for _, it := range s.items {
		if it.Done {
			done++
		} else {
			open++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[todo: %d open, %d done]\n", open, done)
	for _, it := range s.items {
		mark := "[ ]"
		if it.Done {
			mark = "[x]"
		}
		fmt.Fprintf(&b, "- %s %s %s", it.Id, mark, it.Title)
		if len(it.BlockedBy) > 0 {
			fmt.Fprintf(&b, " (blocked by %s)", strings.Join(it.BlockedBy, ", "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// idNum 提取 id 中的数字部分（t1→1, t10→10, item-3→3）：restore 后续号不冲突。
func idNum(id string) int {
	n := 0
	for _, c := range id {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// ---------- 工具（无状态实例：Store 从 ToolContext 动态获取） ----------

type todoTool struct {
	tools.BaseTool
	args  func(string) (any, error) // 参数解码
	build func(s *Store, a any) (string, error)
}

func (t *todoTool) ValidParams(_ context.Context, _, arguments string) error {
	_, err := t.args(arguments)
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name_, err)
	}
	return nil
}

func (t *todoTool) Call(ctx context.Context, _, arguments string) (string, error) {
	a, err := t.args(arguments)
	if err != nil {
		return "", err
	}
	store := StoreFrom(ctx)
	if store == nil {
		return "", fmt.Errorf("%s: no session todo store (todo is session-scoped; inject via the Session run)", t.Name_)
	}
	return t.build(store, a)
}

// Tools 返回待办工具集（todo_add / todo_update），注册进引擎后生效。
// todo 刻意不设读工具：变更工具每次返回完整清单；压缩边界引擎清空 Store 并把压缩前
// 的清单快照以 system-reminder 注入（agent_loop.maybeCompact，见
// agents/prompt/reminder.go CompactTodoReminder）——模型任何时刻无需 todo_list，
// 需要新待办由模型决策调用本工具集重建。
func Tools() []tools.Tool {
	return []tools.Tool{
		&todoTool{
			BaseTool: tools.BaseTool{
				Name_:        "todo_add",
				Description_: "Add a todo item; returns the full updated list (with the new item id and progress stats). blocked_by optional: declare dependency on other todo ids (informational; affects list display).",
				Params_: tools.Obj(map[string]any{
					"title":      tools.Str("Todo content"),
					"blocked_by": tools.ArrayStr("Dependent todo ids (optional)"),
				}, "title"),
				CanParallel_: false,
				ReadOnly_:    true, // 会话状态非外部副作用：规划模式也要记 TODO
			},
			args: func(arguments string) (any, error) {
				var a struct {
					Title     string   `json:"title"`
					BlockedBy []string `json:"blocked_by"`
				}
				return a, json.Unmarshal([]byte(arguments), &a)
			},
			build: func(s *Store, av any) (string, error) {
				a := av.(struct {
					Title     string   `json:"title"`
					BlockedBy []string `json:"blocked_by"`
				})
				if _, err := s.Add(a.Title, a.BlockedBy); err != nil {
					return "", err
				}
				return s.List(), nil // 返回完整清单：变更后进度立即可见（对齐 CC TodoWrite）
			},
		},
		&todoTool{
			BaseTool: tools.BaseTool{
				Name_:        "todo_update",
				Description_: "Update a todo (locate by id): change title / mark done or reopen (done) / adjust dependencies. Only send the fields to change. Returns the full updated list (with progress stats).",
				Params_: tools.Obj(map[string]any{
					"id":         tools.Str("Todo id (e.g. t1)"),
					"title":      tools.Str("New title (optional)"),
					"done":       tools.Map{"type": "boolean", "description": "true = mark done, false = reopen (optional)"},
					"blocked_by": tools.ArrayStr("New dependency list (optional)"),
				}, "id"),
				CanParallel_: false,
				ReadOnly_:    true, // 会话状态非外部副作用：规划模式也要记 TODO
			},
			args: func(arguments string) (any, error) {
				var a struct {
					Id        string    `json:"id"`
					Title     *string   `json:"title"`
					Done      *bool     `json:"done"`
					BlockedBy *[]string `json:"blocked_by"`
				}
				return a, json.Unmarshal([]byte(arguments), &a)
			},
			build: func(s *Store, av any) (string, error) {
				a := av.(struct {
					Id        string    `json:"id"`
					Title     *string   `json:"title"`
					Done      *bool     `json:"done"`
					BlockedBy *[]string `json:"blocked_by"`
				})
				if err := s.Update(a.Id, a.Title, a.Done, a.BlockedBy); err != nil {
					return "", err
				}
				return s.List(), nil // 返回完整清单：变更后进度立即可见（对齐 CC TodoWrite）
			},
		},
	}
}
