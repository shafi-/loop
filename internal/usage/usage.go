// Package usage is loop's cost ledger: a Meter records token usage from
// every LLM call, attributed by a caller-chosen label, so rooms and
// pipeline runs can answer "what did this cost?" without guessing.
// Recording is best-effort bookkeeping — a ledger failure must never
// fail the call it measures.
package usage

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/shafi-/loop/internal/llm"
)

// Entry is one attribution bucket: a label and its accumulated tokens.
type Entry struct {
	Label        string
	Calls        int
	InputTokens  int64
	OutputTokens int64
}

// Total returns the meter's whole-session sums.
type Total struct {
	Calls        int   `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Add sums two totals (e.g. a resumed run's prior sessions plus this one).
func (t Total) Add(o Total) Total {
	return Total{
		Calls:        t.Calls + o.Calls,
		InputTokens:  t.InputTokens + o.InputTokens,
		OutputTokens: t.OutputTokens + o.OutputTokens,
	}
}

// Meter accumulates usage by label. Safe for concurrent use — room turns
// run observer decisions and replies concurrently.
type Meter struct {
	mu      sync.Mutex
	entries map[string]*Entry
}

// NewMeter builds an empty meter.
func NewMeter() *Meter {
	return &Meter{entries: map[string]*Entry{}}
}

// Record adds one call's usage under a label.
func (m *Meter) Record(label string, u llm.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[label]
	if !ok {
		e = &Entry{Label: label}
		m.entries[label] = e
	}
	e.Calls++
	e.InputTokens += int64(u.InputTokens)
	e.OutputTokens += int64(u.OutputTokens)
}

// Snapshot returns the accumulated entries, sorted by label for stable
// display.
func (m *Meter) Snapshot() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// Totals sums every entry.
func (m *Meter) Totals() Total {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t Total
	for _, e := range m.entries {
		t.Calls += e.Calls
		t.InputTokens += e.InputTokens
		t.OutputTokens += e.OutputTokens
	}
	return t
}

// wrapped is a Provider that reports its calls' usage to a meter under
// a fixed label.
type wrapped struct {
	inner llm.Provider
	meter *Meter
	label string
}

// Wrap decorates a provider so every call's usage is recorded under
// label. The decorator is transparent: requests, responses, errors,
// and streaming all pass through unchanged.
func Wrap(p llm.Provider, meter *Meter, label string) llm.Provider {
	if p == nil || meter == nil {
		return p
	}
	return &wrapped{inner: p, meter: meter, label: label}
}

func (w *wrapped) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	resp, err := w.inner.Complete(ctx, req)
	if resp != nil {
		w.meter.Record(w.label, resp.Usage)
	}
	return resp, err
}

func (w *wrapped) Stream(ctx context.Context, req llm.Request, onDelta llm.StreamFunc) (*llm.Response, error) {
	resp, err := w.inner.Stream(ctx, req, onDelta)
	if resp != nil {
		w.meter.Record(w.label, resp.Usage)
	}
	return resp, err
}

// FormatTotal renders a session total as one compact line — the shape
// `loop ask` established: "12 calls · in 8,412 · out 1,038 tokens".
func (t Total) FormatTotal() string {
	return fmt.Sprintf("%d calls · in %s · out %s tokens", t.Calls, Human(t.InputTokens), Human(t.OutputTokens))
}

// Human renders counts with thin separators: 8412 → "8,412".
func Human(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
