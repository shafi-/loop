package llm

// Options carries construction-time settings for a provider, mapped from
// config.ModelConfig by the engine layer.
type Options struct {
	APIKey  string // resolved from the env by the caller
	BaseURL string

	// Retry: 0 attempts = no retry. BackoffMs doubles per attempt.
	MaxAttempts int
	BackoffMs   int
}

// DefaultAPIKeyEnvName is the fallback env var per provider, consulted
// when a caller does not name one explicitly.
var DefaultAPIKeyEnvName = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// DefaultModel is the fallback model id per provider for commands that
// don't insist on one.
var DefaultModel = map[string]string{
	"anthropic": "claude-sonnet-4-5",
	"openai":    "gpt-5",
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
		return nil, &Error{Kind: ErrBadRequest, Provider: name, Body: "unknown provider (want anthropic or openai)"}
	}
	if opts.MaxAttempts > 1 {
		p = WithRetry(p, opts.MaxAttempts, opts.BackoffMs)
	}
	return p, nil
}
