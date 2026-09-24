package session

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"sync"
)

// MemoryStore 内存会话存储（单机/测试用，进程重启即失）。
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]*SessionData
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]*SessionData)}
}

func (s *MemoryStore) Load(_ context.Context, sessionId string) (*SessionData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data[sessionId]
	if !ok {
		return nil, ErrNotFound
	}
	// 返回副本，避免调用方与存储共享底层 slice
	return &SessionData{
		History: append([]core.Message{}, d.History...),
		State:   d.State,
	}, nil
}

func (s *MemoryStore) AppendHistory(_ context.Context, sessionId string, msgs []core.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data[sessionId]
	if d == nil {
		d = &SessionData{}
		s.data[sessionId] = d
	}
	d.History = append(d.History, msgs...)
	return nil
}

func (s *MemoryStore) SaveState(_ context.Context, sessionId string, state *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data[sessionId]
	if d == nil {
		d = &SessionData{}
		s.data[sessionId] = d
	}
	d.State = state
	return nil
}
