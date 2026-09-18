package usage

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/llm"
)

func TestMeterRecordsAndTotals(t *testing.T) {
	m := NewMeter()
	m.Record("room:demo/agent:ceo", llm.Usage{InputTokens: 1000, OutputTokens: 50})
	m.Record("room:demo/agent:ceo", llm.Usage{InputTokens: 500, OutputTokens: 25})
	m.Record("room:demo/decide:cfo", llm.Usage{InputTokens: 200, OutputTokens: 10})

	snap := m.Snapshot()
	if len(snap) != 2 || snap[0].Label != "room:demo/agent:ceo" || snap[1].Label != "room:demo/decide:cfo" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].Calls != 2 || snap[0].InputTokens != 1500 || snap[0].OutputTokens != 75 {
		t.Errorf("ceo entry = %+v", snap[0])
	}
	tot := m.Totals()
	if tot.Calls != 3 || tot.InputTokens != 1700 || tot.OutputTokens != 85 {
		t.Errorf("totals = %+v", tot)
	}
	if got := tot.FormatTotal(); got != "3 calls · in 1,700 · out 85 tokens" {
		t.Errorf("format = %q", got)
	}
}

func TestWrapRecordsBothPaths(t *testing.T) {
	m := NewMeter()
	inner := llm.NewMock(
		&llm.Response{Text: "one", Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}},
		&llm.Response{Text: "two", Usage: llm.Usage{InputTokens: 20, OutputTokens: 3}},
	)
	p := Wrap(inner, m, "room:x/agent:ceo")

	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stream(context.Background(), llm.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	e := m.Snapshot()
	if len(e) != 1 || e[0].Calls != 2 || e[0].InputTokens != 30 || e[0].OutputTokens != 5 {
		t.Fatalf("entries = %+v", e)
	}
}

// The decorator is transparent: errors pass through and record nothing
// (a failed call consumed unknown tokens — inventing numbers would be
// worse than a gap).
func TestWrapPassesErrorsThrough(t *testing.T) {
	m := NewMeter()
	erring := llm.NewMockFunc(func(llm.Request) *llm.Response { return nil })
	_ = erring // MockFunc always answers; use a tiny failing provider instead
	p := Wrap(failProvider{}, m, "gate:approval")
	if _, err := p.Complete(context.Background(), llm.Request{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if tot := m.Totals(); tot.Calls != 0 {
		t.Errorf("failed call recorded: %+v", tot)
	}
}

type failProvider struct{}

func (failProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errBoom
}

func (failProvider) Stream(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	return failProvider{}.Complete(ctx, req)
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }
