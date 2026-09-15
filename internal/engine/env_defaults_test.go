package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Full-chain proof of the env defaults: a pipeline whose YAML names only
// a provider (no model id, no base_url) reaches whatever endpoint and
// model the environment selects — for both provider styles. YAML values
// still win when present.
func TestEnvDefaultsDriveModellessPipeline(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		// One body that satisfies both wire shapes' success parsing.
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"}}],"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_MODEL", "env-chosen-model")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	// Keys must NOT be required once a custom endpoint is in play.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	openaiPipeline := parse(t, `
name: env-openai
stages:
  - id: greet
    type: llm
    model: {provider: openai}
    prompt: "say hi"
`)
	r := &Runner{Pipeline: openaiPipeline, Source: []byte("x"), Providers: DefaultProviderFactory(), RunsDir: t.TempDir()}
	if res, err := r.Run(context.Background()); err != nil || !res.Completed {
		t.Fatalf("openai-style run failed: res=%+v err=%v", res, err)
	}
	if gotModel != "env-chosen-model" {
		t.Errorf("openai stage should send OPENAI_MODEL, got %q", gotModel)
	}

	anthropicPipeline := parse(t, `
name: env-anthropic
stages:
  - id: greet
    type: llm
    model: {provider: anthropic}
    prompt: "say hi"
`)
	gotModel = ""
	r = &Runner{Pipeline: anthropicPipeline, Source: []byte("x"), Providers: DefaultProviderFactory(), RunsDir: t.TempDir()}
	if res, err := r.Run(context.Background()); err != nil || !res.Completed {
		t.Fatalf("anthropic-style run failed: res=%+v err=%v", res, err)
	}
	if gotModel != "claude-from-env" {
		t.Errorf("anthropic stage should send ANTHROPIC_MODEL, got %q", gotModel)
	}

	// Explicit YAML beats the env for the model id.
	explicitPipeline := parse(t, `
name: yaml-wins
stages:
  - id: greet
    type: llm
    model: {provider: openai, model: yaml-model}
    prompt: "say hi"
`)
	gotModel = ""
	r = &Runner{Pipeline: explicitPipeline, Source: []byte("x"), Providers: DefaultProviderFactory(), RunsDir: t.TempDir()}
	if res, err := r.Run(context.Background()); err != nil || !res.Completed {
		t.Fatalf("explicit-model run failed: res=%+v err=%v", res, err)
	}
	if gotModel != "yaml-model" {
		t.Errorf("YAML model must beat OPENAI_MODEL, got %q", gotModel)
	}
}
