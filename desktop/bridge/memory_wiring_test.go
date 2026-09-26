package main

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools"
)

func TestRegisterMemoryToolsProductionWiring(t *testing.T) {
	s := memory.NewService(memory.Config{Root: t.TempDir(), Resolver: bridgeMemoryResolver{}})
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	eng := tools.NewToolEngine()
	registerMemoryTools(eng, s)
	for _, name := range []string{"memory_view", "memory_write"} {
		if _, err := eng.GetTool(context.Background(), name); err != nil {
			t.Fatalf("memory tool %q not wired: %v", name, err)
		}
	}
}

func TestRegisterMemoryToolsNilSafe(t *testing.T) {
	eng := tools.NewToolEngine()
	registerMemoryTools(eng, nil)
	if _, err := eng.GetTool(context.Background(), "memory_view"); err == nil {
		t.Fatal("nil memory service must not register memory tools")
	}
}

type bridgeMemoryResolver struct{}

func (bridgeMemoryResolver) Resolve(context.Context, string) (memory.Identity, error) {
	return memory.Identity{ID: "bridge-repo-12345678", RepoRoot: "/repo", ViaGit: true}, nil
}
