package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// looksLikeNeedsResponsesEndpoint detects the "/v1/chat/completions can't do
// this, use /v1/responses instead" error class. Observed live from Lightning
// AI's "openai/gpt-5.6-sol": "Function tools with reasoning_effort are not
// supported for gpt-5.6-sol in /v1/chat/completions. To use function tools,
// use /v1/responses or set reasoning_effort to 'none'." — the "set
// reasoning_effort to 'none'" half of that message is unfortunately WRONG
// (verified live against Lightning's raw API: sending reasoning_effort:
// "none" gets rejected with the exact same error), so this heal — reroute
// through /v1/responses, which genuinely works — is tried first for this
// error class; tryReasoningEffortHeal remains a fallback for other
// providers/wordings where it might actually be accurate.
func looksLikeNeedsResponsesEndpoint(body []byte) bool {
	norm := strings.ToLower(string(body))
	return strings.Contains(norm, "/v1/responses") && strings.Contains(norm, "tool")
}

// tryResponsesRerouteHeal is the request-shape fix for a model/upstream whose
// chat-completions endpoint cannot do tool calls at all, no matter what
// fields are tweaked — the error names the real fix itself. This translates
// the exact same request into Responses-API shape, dispatches it (always
// non-streaming upstream, see chatResponseToSingleShotSSE for why) to the
// SAME account/base_url, and translates the reply back into
// chat-completions shape so the client — which called chat.completions and
// has no idea any of this happened — sees a normal, transparent success.
// Like the other two heals in this package, this is a request-shape problem
// identical across every account on this provider, not an account-scoped
// one: rotating keys would just fail the same way on each of them.
//
// Unlike tryTokenParamHeal/tryReasoningEffortHeal, this does NOT (yet) learn
// a per-account cache to skip the failing first attempt on future requests —
// proactively skipping straight to /v1/responses would mean duplicating a
// large slice of ServeHTTP's dispatch/streaming setup ahead of the normal
// path, which isn't worth it for what should be a rare, model-specific edge
// case. Every tool-call request to an affected account pays one extra round
// trip; that's the accepted trade-off for now.
func (p *Proxy) tryResponsesRerouteHeal(ctx context.Context, resp *http.Response, baseURL, authKey string, reqBody []byte, providerID, accountID string, wantStream bool) (*http.Response, []byte, bool) {
	const peekCap = 8 << 10
	peeked, _ := io.ReadAll(io.LimitReader(resp.Body, peekCap))
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peeked), resp.Body))
	if !looksLikeNeedsResponsesEndpoint(peeked) {
		return resp, reqBody, false
	}
	responsesBody, err := ChatRequestToResponsesRequest(reqBody)
	if err != nil {
		return resp, reqBody, false
	}
	responsesURL := buildUpstreamURL(baseURL, "/responses")
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(responsesBody))
	if err != nil {
		return resp, reqBody, false
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+authKey)
	upReq.Header.Set("Accept", "application/json")
	newResp, err := p.client.Do(upReq)
	if err != nil {
		return resp, reqBody, false
	}
	respBody, readErr := io.ReadAll(io.LimitReader(newResp.Body, 10<<20))
	newResp.Body.Close()
	if readErr != nil {
		return resp, reqBody, false
	}
	if newResp.StatusCode < 200 || newResp.StatusCode >= 300 {
		// The /v1/responses reroute itself failed too — surface its (more
		// specific, more recent) error instead of the original chat-completions
		// one, but don't claim this was healed since nothing actually succeeded.
		slog.Warn("responses-endpoint reroute also failed", "provider", providerID, "account", accountID, "status", newResp.StatusCode)
		return &http.Response{StatusCode: newResp.StatusCode, Body: io.NopCloser(bytes.NewReader(respBody)), Header: newResp.Header}, responsesBody, false
	}

	chatBody, err := ResponsesResponseToChatResponse(respBody)
	if err != nil {
		slog.Error("responses->chat response translation failed after reroute heal", "err", err)
		return resp, reqBody, false
	}

	finalResp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if wantStream {
		finalResp.Body = io.NopCloser(bytes.NewReader(chatResponseToSingleShotSSE(chatBody)))
		finalResp.Header.Set("Content-Type", "text/event-stream")
	} else {
		finalResp.Body = io.NopCloser(bytes.NewReader(chatBody))
		finalResp.Header.Set("Content-Type", "application/json")
	}

	slog.Info("auto-rerouted tool-call request through /v1/responses and translated the reply back",
		"provider", providerID, "account", accountID, "status_before", resp.StatusCode)
	return finalResp, responsesBody, true
}

// chatResponseToSingleShotSSE wraps an already-complete chat-completions JSON
// response as a minimal, valid chat-completions.chunk SSE stream: one chunk
// carrying the full content/tool_calls, a second carrying finish_reason (and
// usage, if present), then [DONE]. Used when the client asked for streaming
// but tryResponsesRerouteHeal's upstream call was necessarily non-streaming —
// this is a "types out all at once" trade-off in exchange for not needing
// full incremental Responses-SSE -> ChatCompletions-SSE event translation for
// what is a rare, model-specific escape hatch.
func chatResponseToSingleShotSSE(chatJSON []byte) []byte {
	var parsed struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(chatJSON, &parsed); err != nil || len(parsed.Choices) == 0 {
		return []byte("data: [DONE]\n\n")
	}

	base := map[string]any{"id": parsed.ID, "object": "chat.completion.chunk", "created": parsed.Created, "model": parsed.Model}
	choice := parsed.Choices[0]

	delta := map[string]any{"role": "assistant"}
	var text string
	if len(choice.Message.Content) > 0 && json.Unmarshal(choice.Message.Content, &text) == nil && text != "" {
		delta["content"] = text
	}
	if len(choice.Message.ToolCalls) > 0 {
		delta["tool_calls"] = choice.Message.ToolCalls
	}
	chunk1 := cloneMap(base)
	chunk1["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}

	chunk2 := cloneMap(base)
	finishChoice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": choice.FinishReason}
	chunk2["choices"] = []any{finishChoice}
	if len(parsed.Usage) > 0 {
		chunk2["usage"] = parsed.Usage
	}

	var buf bytes.Buffer
	for _, c := range []map[string]any{chunk1, chunk2} {
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		buf.WriteString("data: ")
		buf.Write(b)
		buf.WriteString("\n\n")
	}
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
