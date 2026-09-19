package llm

import (
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// The construction policy, pinned: a missing key is an explained error
// naming the env var, a custom endpoint exempts the key, and the factory
// hands back the same client for identical configs.
func TestBuildRequiresKeyOrCustomEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	_, err := Build(&config.ModelConfig{Provider: "anthropic"}, 1, 0)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("missing key must name the env var: %v", err)
	}

	t.Setenv("ANTHROPIC_BASE_URL", "http://localhost:11434/v1")
	if _, err := Build(&config.ModelConfig{Provider: "anthropic"}, 1, 0); err != nil {
		t.Errorf("a custom endpoint must exempt the key: %v", err)
	}
}

func TestCheckKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if err := CheckKey(config.ResolvedModel{Provider: "openai", Model: "gpt-5", APIKeyEnv: "OPENAI_API_KEY"}); err == nil {
		t.Errorf("missing key must be an error")
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if err := CheckKey(config.ResolvedModel{Provider: "openai", Model: "gpt-5", APIKeyEnv: "OPENAI_API_KEY"}); err != nil {
		t.Errorf("present key must pass: %v", err)
	}
	if err := CheckKey(config.ResolvedModel{Provider: "openai", Model: "gpt-5", APIKeyEnv: "OPENAI_API_KEY", BaseURL: "http://localhost/v1"}); err != nil {
		t.Errorf("custom endpoint must exempt the key: %v", err)
	}
}

func TestNewFactoryCachesClients(t *testing.T) {
	f := NewFactory()
	cfg := &config.ModelConfig{Provider: "mock"}
	p1, err := f(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f(&config.ModelConfig{Provider: "mock", Temperature: ptrFloat(0.7)})
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Errorf("configs on the same provider must share one client")
	}
}

func ptrFloat(f float64) *float64 { return &f }
