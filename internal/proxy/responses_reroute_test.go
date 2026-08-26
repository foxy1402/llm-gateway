package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

// lightningReasoningEffortWording is the real wording captured live from
// Lightning AI's "openai/gpt-5.6-sol": rejects tool calls on
// /v1/chat/completions outright and points at /v1/responses as the real fix
// (verified live: "set reasoning_effort to none" does NOT actually work on
// Lightning's raw API, despite what this message claims — that's why
// tryResponsesRerouteHeal, not tryReasoningEffortHeal, is the one that
// actually resolves this specific wording).
const lightningReasoningEffortWording = "Function tools with reasoning_effort are not supported for gpt-5.6-sol in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'."

func TestLooksLikeNeedsResponsesEndpoint(t *testing.T) {
	positive := []string{
		lightningReasoningEffortWording,
		`{"error":"tool calls require /v1/responses for this model"}`,
	}
	for _, body := range positive {
		if !looksLikeNeedsResponsesEndpoint([]byte(body)) {
			t.Errorf("expected match for: %s", body)
		}
	}
	negative := []string{
		genericReasoningEffortWording, // no /v1/responses mention
		lightningAIWording,
		lightningTooSmallWording,
		`{"error":"rate limited"}`,
		"",
	}
	for _, body := range negative {
		if looksLikeNeedsResponsesEndpoint([]byte(body)) {
			t.Errorf("unexpected match for: %s", body)
		}
	}
}

// lightningStyleMock simulates Lightning AI's real, observed behavior:
// /chat/completions rejects any request carrying "tools" (regardless of
// reasoning_effort — mirroring the live-verified fact that the upstream's own
// suggested workaround doesn't actually work), while /responses genuinely
// supports tool calls.
func lightningStyleMock(calls *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]json.RawMessage
		_ = json.Unmarshal(body, &b)
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/chat/completions":
			if _, hasTools := b["tools"]; hasTools {
				*calls = append(*calls, "chat_completions_with_tools_rejected")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(lightningReasoningEffortWording))
				return
			}
			*calls = append(*calls, "chat_completions_ok")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		case "/responses":
			*calls = append(*calls, "responses_ok")
			var toolCall map[string]any
			if _, hasTools := b["tools"]; hasTools {
				toolCall = map[string]any{
					"id": "fc_1", "type": "function_call", "status": "completed",
					"call_id": "call_1", "name": "add", "arguments": `{"a":2,"b":2}`,
				}
			}
			output := []any{}
			if toolCall != nil {
				output = append(output, toolCall)
			} else {
				output = append(output, map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "4"}},
				})
			}
			resp := map[string]any{
				"id": "resp_abc123", "object": "response", "created_at": 1700000000,
				"model": "gpt-5.6-sol", "status": "completed", "output": output,
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestResponsesRerouteHealForToolCalls reproduces the exact live failure:
// tool-call requests are unconditionally rejected on chat.completions, and
// the fix genuinely requires calling /v1/responses instead (not just tweaking
// a field on the same endpoint, unlike the other two heals in this package).
func TestResponsesRerouteHealForToolCalls(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := lightningStyleMock(&calls, &mu)
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"lightning","messages":[{"role":"user","content":"what is 2+2"}],"tools":[{"type":"function","function":{"name":"add","description":"adds","parameters":{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}}}}}],"tool_choice":"auto"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected transparent success via reroute, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf(`expected object:"chat.completion" (client called chat.completions, must never see raw responses shape), got %v`, out["object"])
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected the translated tool_calls array to survive the round trip, got: %v", msg)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "chat_completions_with_tools_rejected" || calls[1] != "responses_ok" {
		t.Fatalf("unexpected call sequence: %v", calls)
	}
}

// TestResponsesRerouteHealStreamingClient: a client that asked for an SSE
// stream must still get back a well-formed chat-completions.chunk stream
// (buffered/single-shot is fine — see chatResponseToSingleShotSSE — but it
// must be valid SSE, not a bare JSON blob dropped into an SSE response).
func TestResponsesRerouteHealStreamingClient(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := lightningStyleMock(&calls, &mu)
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"lightning","messages":[{"role":"user","content":"what is 2+2"}],"tools":[{"type":"function","function":{"name":"add"}}],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected transparent success, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected an SSE response, got Content-Type %q", ct)
	}
	sseBody := rec.Body.String()
	if !strings.Contains(sseBody, "data: ") {
		t.Fatalf("expected SSE data lines, got: %s", sseBody)
	}
	if !strings.HasSuffix(strings.TrimRight(sseBody, "\n"), "data: [DONE]") {
		t.Fatalf("expected stream to terminate with [DONE], got: %s", sseBody)
	}
	if !strings.Contains(sseBody, `"chat.completion.chunk"`) {
		t.Fatalf("expected chat-completions.chunk shaped events, got: %s", sseBody)
	}
}

// TestResponsesRerouteHealDoesNotBurnRotationBudget: single-key provider (no
// sibling to rotate to) must still self-heal via the reroute.
func TestResponsesRerouteHealDoesNotBurnRotationBudget(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := lightningStyleMock(&calls, &mu)
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, reg := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"solo","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("single-key provider should still self-heal via reroute, got %d: %s", rec.Code, rec.Body.String())
	}
	if !reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("the account must NOT be put in cooldown by a chat.completions/tools conflict — it's not an account-scoped failure")
	}
}

func TestChatRequestToResponsesRequest(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"2"}
	],"tools":[{"type":"function","function":{"name":"add","description":"adds","parameters":{"type":"object"}}}],"tool_choice":"auto","max_completion_tokens":100,"stream":true}`

	out, err := ChatRequestToResponsesRequest([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if parsed["instructions"] != "be nice" {
		t.Errorf("instructions = %v, want \"be nice\"", parsed["instructions"])
	}
	if parsed["stream"] != false {
		t.Errorf("stream = %v, want false (always buffered upstream)", parsed["stream"])
	}
	if parsed["max_output_tokens"] != float64(100) {
		t.Errorf("max_output_tokens = %v, want 100", parsed["max_output_tokens"])
	}
	input, _ := parsed["input"].([]any)
	if len(input) != 3 { // user message, function_call, function_call_output (system consumed into instructions)
		t.Fatalf("expected 3 input items, got %d: %v", len(input), input)
	}
	tools, _ := parsed["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", parsed["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "add" || tool["type"] != "function" {
		t.Errorf("tool not flattened to responses shape: %v", tool)
	}
	if _, stillNested := tool["function"]; stillNested {
		t.Errorf("tool should not have a nested \"function\" key after conversion: %v", tool)
	}
}

func TestResponsesResponseToChatResponse(t *testing.T) {
	t.Run("text output", func(t *testing.T) {
		in := `{"id":"resp_abc","created_at":1700000000,"model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
		out, err := ResponsesResponseToChatResponse([]byte(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(out, &parsed)
		if parsed["object"] != "chat.completion" {
			t.Errorf("object = %v, want chat.completion", parsed["object"])
		}
		if parsed["id"] != "chatcmpl-abc" {
			t.Errorf("id = %v, want chatcmpl-abc", parsed["id"])
		}
		choices := parsed["choices"].([]any)
		msg := choices[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "hello" {
			t.Errorf("content = %v, want hello", msg["content"])
		}
		if choices[0].(map[string]any)["finish_reason"] != "stop" {
			t.Errorf("finish_reason = %v, want stop", choices[0].(map[string]any)["finish_reason"])
		}
	})
	t.Run("preserves cached_tokens and reasoning_tokens", func(t *testing.T) {
		// Reproduces a live-observed gap: a real reroute response reported
		// prompt_tokens_details/cached_tokens (Lightning's raw /v1/responses
		// genuinely returns input_tokens_details.cached_tokens for repeated
		// system-prompt-heavy tool-call turns), but the translated
		// chat-completions response silently dropped it, hiding real prompt-cache
		// savings from the dashboard's cost/usage tracking (extractChatUsage
		// reads prompt_tokens_details.cached_tokens from exactly this shape).
		in := `{"id":"resp_abc","model":"m","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":27478,"output_tokens":44,"total_tokens":27522,"input_tokens_details":{"cached_tokens":27000,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":10}}}`
		out, err := ResponsesResponseToChatResponse([]byte(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(out, &parsed)
		usage := parsed["usage"].(map[string]any)
		ptd, ok := usage["prompt_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("prompt_tokens_details missing from translated usage: %v", usage)
		}
		if ptd["cached_tokens"] != float64(27000) {
			t.Errorf("cached_tokens = %v, want 27000", ptd["cached_tokens"])
		}
		ctd, ok := usage["completion_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("completion_tokens_details missing from translated usage: %v", usage)
		}
		if ctd["reasoning_tokens"] != float64(10) {
			t.Errorf("reasoning_tokens = %v, want 10", ctd["reasoning_tokens"])
		}

		pt, ct, cached := extractChatUsage(out)
		if pt == nil || *pt != 27478 || ct == nil || *ct != 44 {
			t.Fatalf("extractChatUsage prompt/completion mismatch: pt=%v ct=%v", pt, ct)
		}
		if cached == nil || *cached != 27000 {
			t.Fatalf("extractChatUsage did not recover cached_tokens: %v", cached)
		}
	})
	t.Run("function_call output", func(t *testing.T) {
		in := `{"id":"resp_abc","model":"m","output":[{"type":"function_call","call_id":"call_1","name":"add","arguments":"{\"a\":1}"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
		out, err := ResponsesResponseToChatResponse([]byte(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(out, &parsed)
		choices := parsed["choices"].([]any)
		msg := choices[0].(map[string]any)["message"].(map[string]any)
		toolCalls, _ := msg["tool_calls"].([]any)
		if len(toolCalls) != 1 {
			t.Fatalf("expected 1 tool_call, got %v", msg)
		}
		tc := toolCalls[0].(map[string]any)
		if tc["id"] != "call_1" {
			t.Errorf("tool call id = %v, want call_1", tc["id"])
		}
		if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
			t.Errorf("finish_reason = %v, want tool_calls", choices[0].(map[string]any)["finish_reason"])
		}
	})
}
