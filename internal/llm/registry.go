package llm

import "fmt"

// Options carries construction-time settings for a provider, mapped from
// config.ModelConfig by the engine layer.
type Options struct {
	APIKey  string // resolved from the env by the caller
	BaseURL string

	// Retry: 0 attempts = no retry. BackoffMs doubles per attempt.
	MaxAttempts int
	BackoffMs   int
}

// New builds the named provider and wraps it with retry middleware.
func New(name string, opts Options) (Provider, error) {
	var p Provider
	switch name {
	case "anthropic":
		p = &Anthropic{APIKey: opts.APIKey, BaseURL: opts.BaseURL}
	case "openai":
		p = &OpenAI{APIKey: opts.APIKey, BaseURL: opts.BaseURL}
	case "mock":
		p = NewMock()
	default:
		return nil, &Error{Kind: ErrBadRequest, Provider: name, Body: fmt.Sprintf("unknown provider %q (want anthropic or openai — check PROVIDER if set in your env)", name)}
	}
	if opts.MaxAttempts > 1 {
		p = WithRetry(p, opts.MaxAttempts, opts.BackoffMs)
	}
	return p, nil
}
