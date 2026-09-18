package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// streamAcc accumulates one streamed tool call. OpenAI indexes tool calls
// in the delta stream; arguments arrive as JSON string fragments.
type streamAcc struct {
	id      string
	name    string
	args    strings.Builder
	started bool
}

// stream drains the Chat Completions SSE stream into an aggregated Response.
func (o *OpenAI) stream(ctx context.Context, wire *openaiRequest, onDelta StreamFunc) (*Response, error) {
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint()+"/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, vs := range o.headers() {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, &Error{Kind: ErrNetwork, Provider: o.Name(), Body: err.Error()}
	}
	defer resp.Body.Close()

	out := &Response{}
	calls := map[int]*streamAcc{}
	var finish string

	err = streamSSE(ctx, o.Name(), resp, func(ev streamEvent) error {
		data := ev.Data
		if strings.TrimSpace(string(data)) == "[DONE]" {
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil // tolerate keep-alives and non-JSON frames
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				out.Text += c.Delta.Content
				if onDelta != nil {
					onDelta(c.Delta.Content)
				}
			}
			for _, tcd := range c.Delta.ToolCalls {
				acc := calls[tcd.Index]
				if acc == nil {
					acc = &streamAcc{started: true}
					calls[tcd.Index] = acc
				}
				if tcd.ID != "" {
					acc.id = tcd.ID
				}
				if tcd.Function.Name != "" {
					acc.name = tcd.Function.Name
				}
				acc.args.WriteString(tcd.Function.Arguments)
			}
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(calls); i++ {
		if acc := calls[i]; acc != nil {
			args := acc.args.String()
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: acc.id, Name: acc.name, Args: args})
		}
	}
	out.StopReason = mapOpenAIFinish(finish)
	if out.StopReason == "" {
		out.StopReason = StopEndTurn
	}
	return out, nil
}
