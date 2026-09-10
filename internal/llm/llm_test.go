package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockReplaysAndRecords(t *testing.T) {
	m := NewMock(
		&Response{Text: "first", StopReason: StopEndTurn},
		&Response{Text: "second", StopReason: StopToolUse, ToolCalls: []ToolCall{{ID: "t1", Name: "read", Args: `{"p":1}`}}},
	)
	ctx := context.Background()

	r1, _ := m.Complete(ctx, Request{Model: "x", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if r1.Text != "first" {
		t.Errorf("first response = %q", r1.Text)
	}
	r2, _ := m.Complete(ctx, Request{Model: "x"})
	if r2.Text != "second" || len(r2.ToolCalls) != 1 {
		t.Errorf("second response = %+v", r2)
	}
	r3, _ := m.Complete(ctx, Request{Model: "x"})
	if r3.Text != "second" {
		t.Errorf("last response should repeat, got %q", r3.Text)
	}
	if m.Calls() != 3 {
		t.Errorf("calls = %d", m.Calls())
	}
	if len(m.Requests()) != 3 || m.Requests()[0].Messages[0].Content != "hi" {
		t.Error("requests should be recorded verbatim")
	}
}

func TestMockStreamEmitsDelta(t *testing.T) {
	m := NewMock(&Response{Text: "hello"})
	var got string
	resp, err := m.Stream(context.Background(), Request{}, func(d string) { got += d })
	if err != nil || resp.Text != "hello" || got != "hello" {
		t.Errorf("stream = %q/%q/%v", got, resp.Text, err)
	}
}

// flakyProvider fails n times with the given error, then succeeds.
type flakyProvider struct {
	failures int
	err      error
	streams  []bool // records whether each Stream call emitted a delta
}

func (f *flakyProvider) Complete(context.Context, Request) (*Response, error) {
	if f.failures > 0 {
		f.failures--
		return nil, f.err
	}
	return &Response{Text: "ok"}, nil
}

func (f *flakyProvider) Stream(_ context.Context, _ Request, onDelta StreamFunc) (*Response, error) {
	emitted := false
	if f.failures > 0 {
		f.failures--
		if onDelta != nil {
			onDelta("partial")
			emitted = true
		}
		f.streams = append(f.streams, emitted)
		return nil, f.err
	}
	f.streams = append(f.streams, false)
	return &Response{Text: "ok"}, nil
}

func TestRetryRetriesTransientThenSucceeds(t *testing.T) {
	f := &flakyProvider{failures: 2, err: &Error{Kind: ErrServer, Provider: "x"}}
	p := WithRetry(f, 3, 1) // 3 tries, 1ms base backoff
	resp, err := p.Complete(context.Background(), Request{})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("want success after 2 failures, got %v", err)
	}
}

func TestRetryFailsFastOnAuth(t *testing.T) {
	f := &flakyProvider{failures: 5, err: &Error{Kind: ErrAuth, Provider: "x"}}
	_, err := WithRetry(f, 3, 1).Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("auth errors must not be retried")
	}
	if f.failures != 4 {
		t.Errorf("auth error should fail after 1 try, but provider failed %d more times", 5-f.failures-1)
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	f := &flakyProvider{failures: 100, err: &Error{Kind: ErrRateLimited, Provider: "x", RetryAfter: 0}}
	_, err := WithRetry(f, 3, 1).Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if f.failures != 97 {
		t.Errorf("expected exactly 3 attempts, saw %d", 100-f.failures)
	}
}

func TestRetryStreamDoesNotDuplicateEmittedDeltas(t *testing.T) {
	f := &flakyProvider{failures: 1, err: &Error{Kind: ErrServer, Provider: "x"}}
	var got string
	_, err := WithRetry(f, 3, 1).Stream(context.Background(), Request{}, func(d string) { got += d })
	if err == nil {
		t.Fatal("mid-stream failure must surface")
	}
	if got != "partial" {
		t.Errorf("deltas duplicated by retry: %q", got)
	}
}

func TestRetryHonorsContextCancellation(t *testing.T) {
	f := &flakyProvider{failures: 100, err: &Error{Kind: ErrRateLimited, Provider: "x", RetryAfter: 60}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := WithRetry(f, 5, 1).Complete(ctx, Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("context cancellation should cut the retry wait short")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want deadline exceeded, got %v", err)
	}
}
