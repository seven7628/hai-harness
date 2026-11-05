package agenthub

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestIDGeneratorDeterministic(t *testing.T) {
	// 16 字节 = 两个 8 字节 ID，保证两次 New 都能读到数据。
	g := NewIDGenerator(bytes.NewReader([]byte{
		0, 1, 2, 3, 4, 5, 6, 7,
		8, 9, 10, 11, 12, 13, 14, 15,
	}))

	first := g.New("AG")
	second := g.New("grp")

	if !strings.HasPrefix(first, "ag_") {
		t.Fatalf("first = %q, want ag_ prefix", first)
	}
	if !strings.HasPrefix(second, "grp_") {
		t.Fatalf("second = %q, want grp_ prefix", second)
	}
	if suffix := strings.TrimPrefix(first, "ag_"); len(suffix) != 13 {
		t.Fatalf("suffix %q length = %d, want 13 (8 bytes base32)", suffix, len(suffix))
	}
	if suffix := strings.TrimPrefix(first, "ag_"); strings.Contains(suffix, "=") || strings.Contains(suffix, "_") {
		t.Fatalf("ID suffix %q must not contain padding or underscore", suffix)
	}
	if first == second {
		t.Fatalf("two different random inputs produced the same ID %q", first)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestIDGeneratorFallsBackOnRandError(t *testing.T) {
	g := NewIDGenerator(failingReader{})
	a := g.New("task")
	b := g.New("task")

	if !strings.HasPrefix(a, "task_") || !strings.HasPrefix(b, "task_") {
		t.Fatalf("fallback IDs lost prefix: %q, %q", a, b)
	}
	if a == b {
		t.Fatalf("fallback IDs must still differ: %q", a)
	}
}

func TestNewIDDefaults(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		id := NewID(PrefixAgent)
		if !strings.HasPrefix(id, PrefixAgent+"_") {
			t.Fatalf("NewID = %q, want %s_ prefix", id, PrefixAgent)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}
