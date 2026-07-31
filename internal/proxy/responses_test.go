package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesToChatStringInput(t *testing.T) {
	in := []byte(`{"model":"combo-x","input":"Tell me a joke","instructions":"Be funny","stream":false}`)
	out, err := ResponsesToChatRequest(in, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "real-model" {
		t.Fatalf("model: %v", m["model"])
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages: %v", m["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first msg role: %v", first["role"])
	}
	second := msgs[1].(map[string]any)
	if second["content"] != "Tell me a joke" {
		t.Fatalf("second msg content: %v", second["content"])
	}
	if m["stream"] != false {
		t.Fatalf("stream passthrough wrong: %v", m["stream"])
	}
}

func TestResponsesToChatArrayInput(t *testing.T) {
	in := []byte(`{"model":"x","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"},{"type":"input_text","text":"World"}]},
		{"type":"message","role":"assistant","content":[{"type":"input_text","text":"Hi there"}]}
	]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages: %v", m["messages"])
	}
	c0 := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(c0, "Hello") || !strings.Contains(c0, "World") {
		t.Fatalf("concatenated content: %q", c0)
	}
}

func TestResponsesToChatMaxTokens(t *testing.T) {
	in := []byte(`{"model":"x","input":"hi","max_output_tokens":256}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, ok := m["max_tokens"]; !ok {
		t.Fatal("max_tokens missing")
	}
}

// #9: responses→chat translation must not drop IDE-critical fields.
func TestResponsesTranslationPreservesAdvancedFields(t *testing.T) {
	in := []byte(`{
		"model":"x","stream":true,"temperature":0.1,"top_p":0.9,
		"tools":[{"type":"function","function":{"name":"f"}}],
		"tool_choice":"auto","parallel_tool_calls":true,
		"response_format":{"type":"json_object"},
		"logit_bias":{"1234":-100},"stop":["END"],"n":1,"seed":42,"user":"u1",
		"frequency_penalty":0.5,"presence_penalty":0.2,"max_output_tokens":256,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls", "response_format", "logit_bias", "stop", "n", "seed", "user", "temperature", "top_p", "frequency_penalty", "presence_penalty"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing passthrough field %q", k)
		}
	}
	if _, ok := m["max_tokens"]; !ok {
		t.Error("max_output_tokens not mapped to max_tokens")
	}
}

func TestChatToResponsesBasic(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl-1","created":123,"model":"m","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	out, err := ChatToResponsesResponse(chat, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["object"] != "response" {
		t.Fatalf("object: %v", m["object"])
	}
	if m["model"] != "client-model" {
		t.Fatalf("model: %v", m["model"])
	}
	output := m["output"].([]any)
	msg := output[0].(map[string]any)
	content := msg["content"].([]any)
	txt := content[0].(map[string]any)["text"]
	if txt != "Hello" {
		t.Fatalf("text: %v", txt)
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(5) || u["output_tokens"] != float64(3) {
		t.Fatalf("usage: %v", u)
	}
}

func TestChatToResponsesMissingUsage(t *testing.T) {
	chat := []byte(`{"id":"c","choices":[{"message":{"content":"hi"}}]}`)
	out, err := ChatToResponsesResponse(chat, "m")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	u := m["usage"].(map[string]any)
	if u["input_tokens"] != float64(0) {
		t.Fatalf("usage: %v", u)
	}
}
