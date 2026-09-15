package cli

import (
	"strings"
	"testing"
)

// resolveProvider is the ask/new entry into the resolver: flag values are
// the caller's own, an empty provider follows the configured family, and
// the error must name the env var that fixes it.
func TestResolveProviderEnvFirst(t *testing.T) {
	resetEnv := func() {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("ANTHROPIC_MODEL", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("OPENAI_MODEL", "")
	}

	t.Run("only openai configured", func(t *testing.T) {
		resetEnv()
		t.Setenv("OPENAI_API_KEY", "sk-test")
		t.Setenv("OPENAI_MODEL", "env-model")

		_, name, model, err := resolveProvider("", "", "", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if name != "openai" || model != "env-model" {
			t.Errorf("want openai/env-model, got %s/%s", name, model)
		}
	})

	t.Run("only anthropic configured", func(t *testing.T) {
		resetEnv()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")

		_, name, model, err := resolveProvider("", "", "", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if name != "anthropic" {
			t.Errorf("want anthropic, got %s", name)
		}
		if model == "" {
			t.Error("model must never resolve empty")
		}
	})

	t.Run("explicit provider flag beats env inference", func(t *testing.T) {
		resetEnv()
		t.Setenv("OPENAI_API_KEY", "sk-test")

		_, _, _, err := resolveProvider("anthropic", "", "", "", 1)
		if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Errorf("explicit anthropic with no anthropic key must say so, got: %v", err)
		}
	})

	t.Run("explicit base url is keyless", func(t *testing.T) {
		resetEnv()

		if _, _, _, err := resolveProvider("openai", "m", "http://127.0.0.1:1/v1", "", 1); err != nil {
			t.Errorf("custom endpoint must not demand a key, got: %v", err)
		}
	})

	t.Run("nothing configured names the default key var", func(t *testing.T) {
		resetEnv()

		_, _, _, err := resolveProvider("", "", "", "", 1)
		if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Errorf("nothing-configured error must name the default family's key, got: %v", err)
		}
	})
}
