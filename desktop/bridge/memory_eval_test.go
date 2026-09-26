package main

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools"
)

func TestMemoryViewIsPlanModeVisible(t *testing.T) {
	s := memory.NewService(memory.Config{Root: t.TempDir(), Resolver: bridgeMemoryResolver{}})
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	tool := memory.NewViewTool(s)
	if !agents.PlanMode().AllowTool(tool) {
		t.Fatal("memory_view must be visible in plan mode")
	}
	if _, ok := interface{}(tool).(tools.ReadOnlyTool); !ok {
		t.Fatal("memory_view must declare ReadOnlyTool")
	}
}

func TestExploreLoopHasNoMemoryWriteTool(t *testing.T) {
	eng := exploreToolEngine(t.TempDir(), nil)
	if _, err := eng.GetTool(context.Background(), "memory_write"); err == nil {
		t.Fatal("explore engine must not register memory_write")
	}
	if _, err := eng.GetTool(context.Background(), "memory_view"); err == nil {
		t.Fatal("explore engine should not register memory tools in Phase A")
	}
}
