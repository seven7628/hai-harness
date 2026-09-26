package main

import (
	"context"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools"
)

// registerMemoryTools is the production wiring seam for Phase A Memory tools.
// Keeping it separate makes the wiring assertion below call the same function
// used by buildLoop instead of duplicating the registration list in a test.
func registerMemoryTools(eng *tools.Engine, mem *memory.MemoryService) {
	if eng == nil || mem == nil {
		return
	}
	eng.RegisterTool(context.Background(), memory.NewViewTool(mem))
	eng.RegisterTool(context.Background(), memory.NewWriteTool(mem))
}
