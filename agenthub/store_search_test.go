package agenthub

// store_search_test.go：人类搜索 vs agent-safe 搜索的存储层契约（P4-04b-1）。
//
// 关键断言（改坏了会静默泄露/漏搜）：
//   - 人类搜索能看到 owner-only 正文；agent-safe 的 SearchMessages 看不到；
//   - 两边的系统噪声（routing_receipt / round_summary / approval_stub）都排除；
//   - beforeSeq 只回更旧的命中，newest 优先截断；
//   - 空 query 不返回全量日志；
//   - DM 索引与群日志同口径。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type searchTestStore struct {
	name string
	open func(t *testing.T) Store
}

func searchStores() []searchTestStore {
	return []searchTestStore{
		{name: "file", open: func(t *testing.T) Store {
			fs, err := OpenFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("open file store: %v", err)
			}
			return fs
		}},
		{name: "memory", open: func(t *testing.T) Store { return NewMemoryStore() }},
	}
}

// dmAppender 是 DM 索引的写入面（不在 Store 接口里：只有 bridge 和测试需要）。
type dmAppender interface {
	AppendDirectMessage(ctx context.Context, agentID AgentID, m *Message) (int64, error)
}

func appendSearchMsg(t *testing.T, s Store, text string, kind MessageKind, vis Visibility, at time.Time) Message {
	t.Helper()
	m := Message{
		GroupID: "grp_test", Kind: kind, Sender: Sender{Kind: SenderHuman},
		Text: text, Visibility: vis, CreatedAt: at,
	}
	if _, err := s.AppendMessage(context.Background(), &m); err != nil {
		t.Fatalf("AppendMessage(%q): %v", text, err)
	}
	return m
}

func searchTexts(msgs []Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Text)
	}
	return strings.Join(parts, "|")
}

func TestSearchMessagesHumanVisibilityAndNoise(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, tc := range searchStores() {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.open(t)
			appendSearchMsg(t, s, "alpha 普通消息", KindText, VisibilityGroup, base)
			appendSearchMsg(t, s, "alpha 群主悄悄话", KindText, VisibilityOwner, base.Add(time.Second))
			appendSearchMsg(t, s, "alpha targets=ag_a", KindRouting, VisibilityGroup, base.Add(2*time.Second))
			appendSearchMsg(t, s, "alpha 轮次摘要", KindRoundSummary, VisibilityGroup, base.Add(3*time.Second))
			appendSearchMsg(t, s, "alpha SECRET-STUB", KindApprovalStub, VisibilityGroup, base.Add(4*time.Second))
			appendSearchMsg(t, s, "gamma", KindText, VisibilityGroup, base.Add(5*time.Second))
			appendSearchMsg(t, s, "ALPHA 第三条", KindText, VisibilityGroup, base.Add(6*time.Second))

			got, err := s.SearchMessagesHuman(context.Background(), "grp_test", "alpha", 0, 50)
			if err != nil {
				t.Fatalf("SearchMessagesHuman: %v", err)
			}
			if want := "alpha 普通消息|alpha 群主悄悄话|ALPHA 第三条"; searchTexts(got) != want {
				t.Fatalf("人类搜索 = %q, want %q（owner-only 必须可见、噪声必须排除）", searchTexts(got), want)
			}
			if got[1].Visibility != VisibilityOwner {
				t.Fatalf("owner-only 命中 visibility = %q", got[1].Visibility)
			}

			// agent-safe 口径不变：owner-only 与噪声都不能出现在群工具的搜索结果里。
			agentHits, err := s.SearchMessages(context.Background(), "grp_test", "alpha", 0, 50)
			if err != nil {
				t.Fatalf("SearchMessages: %v", err)
			}
			if want := "alpha 普通消息|ALPHA 第三条"; searchTexts(agentHits) != want {
				t.Fatalf("agent 搜索 = %q, want %q（owner-only 不得泄露）", searchTexts(agentHits), want)
			}
		})
	}
}

func TestSearchMessagesHumanPaginationAndLimit(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, tc := range searchStores() {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.open(t)
			ctx := context.Background()
			for i := 0; i < 3; i++ {
				appendSearchMsg(t, s, "hit-"+string(rune('a'+i)), KindText, VisibilityGroup, base.Add(time.Duration(i)*time.Second))
			}
			appendSearchMsg(t, s, "miss", KindText, VisibilityGroup, base.Add(3*time.Second))

			// 无 beforeSeq：取**最新** limit 条（返回旧→新）。
			got, err := s.SearchMessagesHuman(ctx, "grp_test", "hit", 0, 2)
			if err != nil {
				t.Fatalf("search limit 2: %v", err)
			}
			if want := "hit-b|hit-c"; searchTexts(got) != want {
				t.Fatalf("limit=2 = %q, want %q", searchTexts(got), want)
			}
			// beforeSeq 只看更旧的命中。
			got, err = s.SearchMessagesHuman(ctx, "grp_test", "hit", 3, 10)
			if err != nil {
				t.Fatalf("search before 3: %v", err)
			}
			if want := "hit-a|hit-b"; searchTexts(got) != want {
				t.Fatalf("before_seq=3 = %q, want %q", searchTexts(got), want)
			}
			got, err = s.SearchMessagesHuman(ctx, "grp_test", "hit", 1, 10)
			if err != nil || len(got) != 0 {
				t.Fatalf("before_seq=1 = %q, %v, want empty", searchTexts(got), err)
			}
		})
	}
}

func TestSearchMessagesHumanEmptyQueryAndCJK(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, tc := range searchStores() {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.open(t)
			ctx := context.Background()
			appendSearchMsg(t, s, "Q4 复盘：把结论写清楚", KindText, VisibilityGroup, base)
			appendSearchMsg(t, s, "另一个话题", KindText, VisibilityGroup, base.Add(time.Second))

			// 空查询不返回任何消息：搜索不该等于「导出全部日志」。
			for _, q := range []string{"", "   ", "\n\t"} {
				if got, err := s.SearchMessagesHuman(ctx, "grp_test", q, 0, 10); err != nil || len(got) != 0 {
					t.Fatalf("空 query %q = %q, %v；want empty", q, searchTexts(got), err)
				}
			}
			// CJK substring：中间的汉字也能命中，不做分词。
			got, err := s.SearchMessagesHuman(ctx, "grp_test", "复盘", 0, 10)
			if err != nil || len(got) != 1 || !strings.Contains(got[0].Text, "复盘") {
				t.Fatalf("CJK 搜索 = %q, %v", searchTexts(got), err)
			}
			// 大小写不敏感（ASCII）。
			got, err = s.SearchMessagesHuman(ctx, "grp_test", "q4", 0, 10)
			if err != nil || len(got) != 1 {
				t.Fatalf("小写 Q4 搜索 = %q, %v", searchTexts(got), err)
			}
		})
	}
}

func TestSearchDirectMessagesHuman(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, tc := range searchStores() {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.open(t)
			dm, ok := s.(dmAppender)
			if !ok {
				t.Fatalf("%s 不支持 DM 索引", tc.name)
			}
			ctx := context.Background()
			seed := []*Message{
				{Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "DM 里的 alpha 一", CreatedAt: base},
				{Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "DM 里的 alpha 二（仅群主）", Visibility: VisibilityOwner, CreatedAt: base.Add(time.Second)},
				{Kind: KindRouting, Sender: Sender{Kind: SenderSystem}, Text: "alpha targets=ag_a", CreatedAt: base.Add(2 * time.Second)},
				{Kind: KindRoundSummary, Sender: Sender{Kind: SenderSystem}, Text: "alpha 轮次摘要", CreatedAt: base.Add(3 * time.Second)},
				{Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "别的话题", CreatedAt: base.Add(4 * time.Second)},
			}
			for _, m := range seed {
				if _, err := dm.AppendDirectMessage(ctx, "ag_test", m); err != nil {
					t.Fatalf("AppendDirectMessage: %v", err)
				}
			}
			got, err := s.SearchDirectMessages(ctx, "ag_test", "ALPHA", 0, 50)
			if err != nil {
				t.Fatalf("SearchDirectMessages: %v", err)
			}
			if want := "DM 里的 alpha 一|DM 里的 alpha 二（仅群主）"; searchTexts(got) != want {
				t.Fatalf("DM 搜索 = %q, want %q", searchTexts(got), want)
			}
			// beforeSeq 分页 + 空查询。
			got, err = s.SearchDirectMessages(ctx, "ag_test", "alpha", 2, 50)
			if err != nil || len(got) != 1 || got[0].Text != "DM 里的 alpha 一" {
				t.Fatalf("DM before_seq=2 = %q, %v", searchTexts(got), err)
			}
			if got, err := s.SearchDirectMessages(ctx, "ag_test", "", 0, 50); err != nil || len(got) != 0 {
				t.Fatalf("DM 空 query = %q, %v；want empty", searchTexts(got), err)
			}
		})
	}
}

func TestSearchHumanRejectsInvalidIDs(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.SearchMessagesHuman(ctx, "", "x", 0, 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空 gid err = %v, want ErrInvalid", err)
	}
	if _, err := s.SearchDirectMessages(ctx, "../etc", "x", 0, 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法 agentID err = %v, want ErrInvalid", err)
	}
}
