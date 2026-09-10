package llm

import (
	"fmt"
	"os"
)

// Hint returns an actionable, kind-specific suggestion for a provider
// error, or "" when there is nothing useful to add. The engine attaches
// hints to stage failures so users fix pipelines instead of decoding
// provider payloads. The engine names the model and env var itself — it
// has the config in scope; this layer stays generic.
func Hint(err error) string {
	e, ok := AsError(err)
	if !ok {
		return ""
	}
	switch e.Kind {
	case ErrAuth:
		return "the API key was rejected — check the key env var named in the model config (api_key_env)"
	case ErrModelNotFound:
		return "unknown or unavailable model for this provider — check the model id (and base_url) in your pipeline"
	case ErrContextTooLong:
		return "input exceeded the model's context window — shorten the prompt or the upstream stage output it embeds"
	case ErrRateLimited:
		return "provider rate limits persisted even with Retry-After honored — raise retry.max_attempts or reduce parallel calls"
	case ErrOverloaded:
		return "provider is overloaded — retry.max_attempts/backoff_ms may ride it out, or switch models for this stage"
	case ErrNetwork:
		return "could not reach the provider — check network/DNS connectivity (or the base_url)"
	case ErrBadRequest:
		return "the provider rejected the request — usually a malformed model id or unsupported parameter"
	default:
		return ""
	}
}

// AuthEnvDetail annotates auth failures with the ground truth about the
// configured env var: missing vs set-but-rejected.
func AuthEnvDetail(err error, envName string) string {
	if envName == "" {
		return ""
	}
	e, ok := AsError(err)
	if !ok || e.Kind != ErrAuth {
		return ""
	}
	if os.Getenv(envName) == "" {
		return fmt.Sprintf("env var %s is not set", envName)
	}
	return fmt.Sprintf("env var %s is set but was rejected", envName)
}
