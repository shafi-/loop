package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Endpoint conventions are per resolved provider family, and the same
// base URL must land on the same route whether loop's own adapter or the
// cline host consumes it:
//
//	anthropic: base carries no /v1 → <base>/v1/messages; a base that
//	           already ends in /v1 must never double it.
//	openai:    base is used as-is → <base>/chat/completions; the version
//	           prefix (e.g. /v1) belongs to the caller.
func TestEndpointResolutionPerProvider(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// One body that satisfies both adapters' success parsing.
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	complete := func(p Provider) {
		t.Helper()
		if _, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}

	cases := []struct {
		name string
		base string
		want string
	}{
		{"anthropic plain base", srv.URL, "/v1/messages"},
		{"anthropic base already carrying /v1", srv.URL + "/v1", "/v1/messages"},
		{"anthropic trailing slash", srv.URL + "/", "/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			complete(&Anthropic{APIKey: "k", BaseURL: tc.base})
			if gotPath != tc.want {
				t.Errorf("path = %q, want %q", gotPath, tc.want)
			}
		})
	}

	t.Run("openai base used as-is", func(t *testing.T) {
		complete(&OpenAI{APIKey: "k", BaseURL: srv.URL})
		if gotPath != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", gotPath)
		}
	})
	t.Run("openai version prefix belongs to the caller", func(t *testing.T) {
		complete(&OpenAI{APIKey: "k", BaseURL: srv.URL + "/v1"})
		if gotPath != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", gotPath)
		}
	})
}
