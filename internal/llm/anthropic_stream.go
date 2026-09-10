package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// blockAcc accumulates one content block during streaming.
type blockAcc struct {
	blockType string // "text" | "tool_use"
	text      strings.Builder
	toolID    string
	toolName  string
	args      strings.Builder
}

// openStream POSTs the wire request and drains the SSE event stream into an
// aggregated Response, forwarding text deltas to onDelta as they arrive.
func (a *Anthropic) openStream(ctx context.Context, wire *anthropicRequest, onDelta StreamFunc) (*Response, error) {
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint()+"/v1/messages", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, vs := range a.headers() {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return &Response{}, &Error{Kind: ErrNetwork, Provider: a.Name(), Body: err.Error()}
	}
	defer resp.Body.Close()

	out := &Response{}
	blocks := map[int]*blockAcc{}

	err = streamSSE(ctx, a.Name(), resp, func(ev streamEvent) error {
		switch ev.Event {
		case "message_start":
			var m struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(ev.Data, &m); err == nil {
				out.Usage.InputTokens = m.Message.Usage.InputTokens
			}

		case "content_block_start":
			var m struct {
				Index int `json:"index"`
				Block struct {
					Type    string `json:"type"`
					ID      string `json:"id"`
					Name    string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &m); err == nil {
				blocks[m.Index] = &blockAcc{blockType: m.Block.Type, toolID: m.Block.ID, toolName: m.Block.Name}
			}

		case "content_block_delta":
			var m struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"` // text_delta | input_json_delta
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				return nil
			}
			acc := blocks[m.Index]
			if acc == nil {
				return nil
			}
			switch m.Delta.Type {
			case "text_delta":
				acc.text.WriteString(m.Delta.Text)
				if onDelta != nil {
					onDelta(m.Delta.Text)
				}
			case "input_json_delta":
				acc.args.WriteString(m.Delta.PartialJSON)
			}

		case "message_delta":
			var m struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(ev.Data, &m); err == nil {
				out.StopReason = mapStop(m.Delta.StopReason)
				out.Usage.OutputTokens = m.Usage.OutputTokens
			}

		case "error":
			var m struct {
				Err struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(ev.Data, &m)
			return &Error{Kind: ErrServer, Provider: a.Name(), Body: m.Err.Message}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Assemble blocks in index order (map iteration is random; indices are not).
	for i := 0; i < len(blocks); i++ {
		acc, ok := blocks[i]
		if !ok {
			continue
		}
		switch acc.blockType {
		case "text":
			out.Text += acc.text.String()
		case "tool_use":
			args := acc.args.String()
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: acc.toolID, Name: acc.toolName, Args: args})
		}
	}
	if out.StopReason == "" {
		out.StopReason = StopEndTurn
	}
	return out, nil
}
