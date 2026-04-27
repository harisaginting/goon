package llm

import (
	"context"
	"errors"
	"sync"
)

// Mock is a deterministic in-memory provider, used in tests and `--provider=mock`.
type Mock struct {
	mu       sync.Mutex
	Replies  []string // pop from front each call
	Calls    int
	LastMsgs []Message
}

// NewMock creates a Mock pre-loaded with replies.
func NewMock(replies []string) *Mock {
	return &Mock{Replies: replies}
}

// Name returns "mock".
func (m *Mock) Name() string { return "mock" }

// Generate returns the next queued reply.
func (m *Mock) Generate(_ context.Context, messages []Message, _ Options) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	m.LastMsgs = append([]Message(nil), messages...)
	if len(m.Replies) == 0 {
		return "", errors.New("mock: no replies queued")
	}
	r := m.Replies[0]
	m.Replies = m.Replies[1:]
	return r, nil
}

// Stream emits the queued reply as a single chunk.
func (m *Mock) Stream(ctx context.Context, messages []Message, opts Options, onChunk func(string)) (string, error) {
	out, err := m.Generate(ctx, messages, opts)
	if err == nil && onChunk != nil {
		onChunk(out)
	}
	return out, err
}
