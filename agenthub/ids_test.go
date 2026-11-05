package agenthub

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestIDGeneratorDeterministic(t *testing.T) {
	// 16 字节足够两次 New：agent 读 5 字节，group 读 4 字节。
	g := NewIDGenerator(bytes.NewReader([]byte{
		0, 1, 2, 3, 4, 5, 6, 7,
		8, 9, 10, 11, 12, 13, 14, 15,
	}))

	first := g.New("AG")
	second := g.New("grp")

	if first != "ag_aaaqeaye" {
		t.Fatalf("agent ID = %q, want ag_aaaqeaye (8 chars, bytes 0..4)", first)
	}
	if second != "grp_audaoc" {
		t.Fatalf("group ID = %q, want grp_audaoc (6 chars, bytes 5..8)", second)
	}
	if strings.Contains(first, "=") || strings.Contains(strings.TrimPrefix(first, "ag_"), "_") {
		t.Fatalf("ID %q must not contain padding or extra underscore", first)
	}
}

func TestMessageIDIsDeterministic(t *testing.T) {
	if got, want := MessageID(42), "msg_42"; got != want {
		t.Fatalf("MessageID(42) = %q, want %q", got, want)
	}
	if got, want := MessageID(0), "msg_0"; got != want {
		t.Fatalf("MessageID(0) = %q, want %q", got, want)
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
	if len(strings.TrimPrefix(a, "task_")) != 6 {
		t.Fatalf("fallback task ID %q has wrong suffix length", a)
	}
	if a == b {
		t.Fatalf("fallback IDs must still differ: %q", a)
	}
}

func TestIDGeneratorConcurrentInjectedReader(t *testing.T) {
	// 共享的 bytes.Reader 不是并发安全的；IDGenerator 的 mutex 必须把它串行化。
	g := NewIDGenerator(bytes.NewReader(make([]byte, 5*8*200)))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if id := g.New(PrefixAgent); !strings.HasPrefix(id, "ag_") {
					t.Errorf("bad ID %q", id)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewIDDefaults(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		id := NewID(PrefixAgent)
		if !strings.HasPrefix(id, PrefixAgent+"_") {
			t.Fatalf("NewID = %q, want %s_ prefix", id, PrefixAgent)
		}
		if len(strings.TrimPrefix(id, PrefixAgent+"_")) != 8 {
			t.Fatalf("NewID = %q, want 8-char suffix", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}
