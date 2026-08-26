package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Chat→responses: an assistant message carrying BOTH text and tool_calls must
// emit both — previously the text was dropped the moment any tool_call was
// present, losing the model's accompanying narration.
func TestChatRequestToResponsesKeepsTextWithToolCalls(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"assistant","content":"Let me check the weather.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}
	]}`
	out, err := ChatRequestToResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	items, _ := parsed["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 input items (message + function_call), got %d: %v", len(items), items)
	}
	msg, ok := items[0].(map[string]any)
	if !ok || msg["type"] != "message" {
		t.Fatalf("first item should be the text message: %v", items[0])
	}
	parts, _ := msg["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "Let me check the weather." {
		t.Fatalf("text lost alongside tool_calls: %v", msg)
	}
	if msg["role"] != "assistant" {
		t.Fatalf("role: %v", msg["role"])
	}
	fc := items[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" {
		t.Fatalf("function_call item: %v", fc)
	}
}

// Chat→responses: a tool message with an empty tool_call_id must be dropped,
// not forwarded — it can't match any call and would make the upstream 400.
func TestChatRequestToResponsesDropsEmptyCallID(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"tool","tool_call_id":"","content":"orphan result"}
	]}`
	out, err := ChatRequestToResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	items, _ := parsed["input"].([]any)
	if len(items) != 0 {
		t.Fatalf("orphan tool message should be dropped, got: %v", items)
	}
}

// Chat→responses: reasoning_effort maps into the nested reasoning block, and
// a specific-function tool_choice gets flattened.
func TestChatRequestToResponsesReasoningAndToolChoice(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"low",
		"tool_choice":{"type":"function","function":{"name":"pick"}}}`
	out, err := ChatRequestToResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := parsed["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "low" {
		t.Fatalf("reasoning_effort not mapped to reasoning block: %v", parsed["reasoning"])
	}
	tc, _ := parsed["tool_choice"].(map[string]any)
	if tc == nil || tc["name"] != "pick" || tc["type"] != "function" {
		t.Fatalf("tool_choice not flattened to Responses shape: %v", parsed["tool_choice"])
	}
	if _, nested := tc["function"]; nested {
		t.Fatalf("tool_choice must not keep the nested function key: %v", tc)
	}
}

// Responses→chat: text and tool_calls survive together, incomplete status maps
// to length/content_filter, and the model is the client alias, not the
// upstream's raw model id.
func TestResponsesResponseToChatKeepsTextAndIncomplete(t *testing.T) {
	in := `{"id":"resp_x","model":"upstream/model","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"partial answer"}]},
			{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
		],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	out, err := ResponsesResponseToChatResponse([]byte(in), "client-alias")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["model"] != "client-alias" {
		t.Fatalf("model must be the client alias: %v", parsed["model"])
	}
	choices := parsed["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v, want length (status incomplete + max_output_tokens)", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["content"] != "partial answer" {
		t.Fatalf("content lost alongside tool_calls: %v", msg)
	}
	if len(msg["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls lost: %v", msg)
	}

	// content_filter variant
	in2 := strings.Replace(in, "max_output_tokens", "content_filter", 1)
	out2, err := ResponsesResponseToChatResponse([]byte(in2), "m")
	if err != nil {
		t.Fatal(err)
	}
	var parsed2 map[string]any
	_ = json.Unmarshal(out2, &parsed2)
	if parsed2["choices"].([]any)[0].(map[string]any)["finish_reason"] != "content_filter" {
		t.Fatalf("content_filter reason not mapped: %v", parsed2["choices"])
	}
}

// Responses→chat: float usage values ("123.0") must not zero the usage block.
func TestResponsesResponseToChatFloatUsage(t *testing.T) {
	in := `{"id":"resp_x","output":[],"usage":{"input_tokens":12.0,"output_tokens":3.0,"total_tokens":15.0}}`
	out, err := ResponsesResponseToChatResponse([]byte(in), "m")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	usage := parsed["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(3) {
		t.Fatalf("float usage zeroed or mangled: %v", usage)
	}
}

// Chat→responses: usage detail keys must use the Responses-spec names.
func TestChatToResponsesUsageDetailKeys(t *testing.T) {
	chat := []byte(`{"id":"c","choices":[{"message":{"content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,
			"prompt_tokens_details":{"cached_tokens":4},
			"completion_tokens_details":{"reasoning_tokens":2}}}`)
	out, err := ChatToResponsesResponse(chat, "m")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	usage := parsed["usage"].(map[string]any)
	if _, ok := usage["input_tokens_details"]; !ok {
		t.Errorf("input_tokens_details missing (Responses-spec name): %v", usage)
	}
	if _, ok := usage["output_tokens_details"]; !ok {
		t.Errorf("output_tokens_details missing (Responses-spec name): %v", usage)
	}
	if _, ok := usage["prompt_tokens_details"]; ok {
		t.Errorf("chat-shaped prompt_tokens_details must not leak into a Responses body")
	}
}

// Chat→responses: a "length" finish reason must surface as status incomplete.
func TestChatToResponsesIncompleteStatus(t *testing.T) {
	chat := []byte(`{"id":"c","choices":[{"message":{"content":"trunc"},"finish_reason":"length"}]}`)
	out, err := ChatToResponsesResponse(chat, "m")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if parsed["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete for finish_reason length", parsed["status"])
	}
}

// Responses→chat request: the developer role maps onto the chat "system" role.
func TestResponsesToChatDeveloperRole(t *testing.T) {
	in := []byte(`{"model":"x","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"be careful"}]}]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role must map to system: %v", msgs[0])
	}
}

// healPeek must preserve Close delegation to the original transport body —
// wrapping in io.NopCloser (the old bug) would leak the connection.
func TestHealPeekPreservesClose(t *testing.T) {
	orig := &trackingReadCloser{Reader: strings.NewReader("error body here")}
	resp := &http.Response{StatusCode: 500, Body: orig}
	peeked := healPeek(resp)
	if string(peeked) != "error body here" {
		t.Fatalf("peeked = %q", peeked)
	}
	// Body must still be fully readable after the peek.
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "error body here" {
		t.Fatalf("reattached body = %q, want the full content", rest)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !orig.closed {
		t.Fatal("Close did not reach the original body — socket would not be released for reuse")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (t *trackingReadCloser) Close() error { t.closed = true; return nil }
