package llm

import (
	"context"
	"fmt"
	"sync"
)

// Mock is a deterministic Provider for tests: it replays scripted
// responses in order and records every request it was given, so tests can
// assert on the exact conversation the engine produced.
//
// A zero-value Mock replies with "mock: <n>" to every call.
type Mock struct {
	mu        sync.Mutex
	responses []func(req Request) *Response // replayed FIFO; last one repeats
	requests  []Request                     // recorded, for assertions
	calls     int
}

// NewMock builds a Mock that replays responses in order; the final
// response repeats for any extra calls.
func NewMock(responses ...*Response) *Mock {
	m := &Mock{}
	for _, r := range responses {
		resp := r // capture
		m.responses = append(m.responses, func(Request) *Response { return resp })
	}
	return m
}

// NewMockFunc builds a Mock whose responses are computed from the request.
func NewMockFunc(respond func(req Request) *Response) *Mock {
	return &Mock{responses: []func(Request) *Response{respond}}
}

// Requests returns a copy of the recorded requests, oldest first.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// Calls reports how many completions were requested.
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *Mock) next(req Request) *Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	i := m.calls
	m.calls++
	if len(m.responses) == 0 {
		return &Response{Text: fmt.Sprintf("mock: %d", i), StopReason: StopEndTurn}
	}
	if i >= len(m.responses) {
		i = len(m.responses) - 1
	}
	return m.responses[i](req)
}

// Complete implements Provider.
func (m *Mock) Complete(_ context.Context, req Request) (*Response, error) {
	return m.next(req), nil
}

// Stream implements Provider: the scripted response is delivered as one
// delta, then returned whole.
func (m *Mock) Stream(ctx context.Context, req Request, onDelta StreamFunc) (*Response, error) {
	resp := m.next(req)
	if onDelta != nil && resp.Text != "" {
		onDelta(resp.Text)
	}
	return resp, nil
}
