package llm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// retryProvider wraps a Provider and retries transient failures
// (rate limits, server errors, network blips) with exponential backoff,
// honoring the server's Retry-After when present. Auth and bad-request
// failures fail fast: retrying them cannot succeed.
type retryProvider struct {
	next        Provider
	maxAttempts int
	backoffMs   int
}

// WithRetry wraps p with retry middleware. maxAttempts is the total number
// of tries (2 = one retry).
func WithRetry(p Provider, maxAttempts, backoffMs int) Provider {
	return &retryProvider{next: p, maxAttempts: maxAttempts, backoffMs: backoffMs}
}

// sleepBackoff waits the right amount for the given attempt, respecting
// both the error's Retry-After and ctx cancellation.
func (r *retryProvider) sleepBackoff(ctx context.Context, attempt int, err error) error {
	delay := backoff(r.backoffMs, attempt)
	if e, ok := AsError(err); ok && e.RetryAfter > 0 {
		if d := time.Duration(e.RetryAfter) * time.Second; d > delay {
			delay = d
		}
	}
	// Small jitter so concurrent agents don't retry in lockstep.
	delay += time.Duration(rand.Int64N(int64(delay / 10)))
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *retryProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		resp, err := r.next.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if e, ok := AsError(err); !ok || !e.Retryable() || attempt == r.maxAttempts {
			return nil, exhausted(r.maxAttempts, attempt, err)
		}
		if serr := r.sleepBackoff(ctx, attempt, err); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// exhausted annotates the final error so users can tell transient
// failure from persistent failure at a glance.
func exhausted(maxAttempts, attempt int, err error) error {
	if maxAttempts > 1 && attempt == maxAttempts {
		if e, ok := AsError(err); ok && e.Retryable() {
			return fmt.Errorf("%w (still failing after %d attempts)", err, maxAttempts)
		}
	}
	return err
}

func (r *retryProvider) Stream(ctx context.Context, req Request, onDelta StreamFunc) (*Response, error) {
	// A stream that fails midway has already emitted deltas; retrying would
	// duplicate them on screen. The guarded wrapper detects that case and
	// makes the error non-retryable.
	var emitted bool
	guarded := onDelta
	if onDelta != nil {
		guarded = func(delta string) {
			emitted = true
			onDelta(delta)
		}
	}
	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		resp, err := r.next.Stream(ctx, req, guarded)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if e, ok := AsError(err); !ok || !e.Retryable() || attempt == r.maxAttempts || emitted {
			return nil, exhausted(r.maxAttempts, attempt, err)
		}
		if serr := r.sleepBackoff(ctx, attempt, err); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}
