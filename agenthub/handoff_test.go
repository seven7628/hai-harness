package agenthub

// P3a-06：handoff / ack / 护栏。

import (
	"errors"
	"testing"
	"time"
)

func handoffGroup() *Group {
	return &Group{
		ID: "grp_aaaaaa", Name: "交接群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember},
			{AgentID: "ag_cccccccc", Role: RoleMember},
			{AgentID: "ag_dddddddd", Role: RoleObserver},
		},
		Policy: GroupPolicy{MaxHandoffDepth: 3},
	}
}

func TestHandoffDepthAndObserver(t *testing.T) {
	g := handoffGroup()
	now := time.Now().UTC()
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", RoleOwner, RoleMember, 2, nil, now); err != nil {
		t.Fatalf("depth 2 should pass: %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", RoleOwner, RoleMember, 3, nil, now); !errors.Is(err, ErrHandoffTooDeep) {
		t.Fatalf("depth 3 err = %v", err)
	}
	if err := CheckHandoff(g, "ag_dddddddd", "ag_bbbbbbbb", RoleObserver, RoleMember, 0, nil, now); !errors.Is(err, ErrHandoffObserver) {
		t.Fatalf("observer sender err = %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_dddddddd", RoleOwner, RoleObserver, 0, nil, now); !errors.Is(err, ErrHandoffObserver) {
		t.Fatalf("observer target err = %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_aaaaaaaa", RoleOwner, RoleOwner, 0, nil, now); err == nil {
		t.Fatal("self handoff must fail")
	}
}

func TestHandoffPairRateLimit(t *testing.T) {
	g := handoffGroup()
	now := time.Now().UTC()
	history := []Handoff{
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-5 * time.Minute)},
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-1 * time.Minute)},
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", RoleOwner, RoleMember, 0, history, now); !errors.Is(err, ErrHandoffRateLimited) {
		t.Fatalf("third in window err = %v", err)
	}
	// 不同对不受影响。
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_cccccccc", RoleOwner, RoleMember, 0, history, now); err != nil {
		t.Fatalf("other pair: %v", err)
	}
	// 窗口外旧记录不计数。
	old := []Handoff{
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-11 * time.Minute)},
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-12 * time.Minute)},
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", RoleOwner, RoleMember, 0, old, now); err != nil {
		t.Fatalf("old history: %v", err)
	}
}

func TestHandoffAckIdempotent(t *testing.T) {
	now := time.Now().UTC()
	h := &Handoff{Status: HandoffPendingAck}
	if err := h.Ack(true, now); err != nil || h.Status != HandoffAccepted || h.AckedAt == nil {
		t.Fatalf("ack = %+v, %v", h, err)
	}
	// 同状态重复 ack 幂等。
	if err := h.Ack(true, now.Add(time.Minute)); err != nil {
		t.Fatalf("repeat ack: %v", err)
	}
	// 已接受不能再拒绝。
	if err := h.Ack(false, now); !errors.Is(err, ErrHandoffNotPending) {
		t.Fatalf("flip err = %v", err)
	}
	var nilH *Handoff
	if err := nilH.Ack(true, now); err == nil {
		t.Fatal("nil handoff must fail")
	}
}
