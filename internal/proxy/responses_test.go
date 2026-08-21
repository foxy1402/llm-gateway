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

// #vision: an input_image part must survive translation as a chat-completions
// array-typed content part instead of being silently dropped (previously the
// translator only read part["text"], so an image-only message produced an
// empty content string with the image gone).
func TestResponsesToChatPreservesImagePart(t *testing.T) {
	in := []byte(`{"model":"x","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"What is in this image?"},
			{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}
		]}
	]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages: %v", m["messages"])
	}
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content must be an array once an image part is present, got %T: %v", msgs[0].(map[string]any)["content"], msgs[0].(map[string]any)["content"])
	}
	if len(content) != 2 {
		t.Fatalf("want 2 content parts (text + image), got %d: %v", len(content), content)
	}
	textPart := content[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "What is in this image?" {
		t.Fatalf("text part: %v", textPart)
	}
	imgPart := content[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Fatalf("image part type: %v", imgPart)
	}
	imgURL, ok := imgPart["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url must be a nested object (chat-completions shape), got %T: %v", imgPart["image_url"], imgPart["image_url"])
	}
	if imgURL["url"] != "data:image/png;base64,iVBORw0KGgo=" {
		t.Fatalf("image url lost: %v", imgURL)
	}
	if imgURL["detail"] != "high" {
		t.Fatalf("detail lost: %v", imgURL)
	}
}

// #vision: an image-only message (no text part at all) must still forward the
// image rather than collapsing to an empty string.
func TestResponsesToChatImageOnlyMessage(t *testing.T) {
	in := []byte(`{"model":"x","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"https://example.com/x.jpg"}
		]}
	]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("want 1 image part, got %v", content)
	}
	imgPart := content[0].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Fatalf("image part: %v", imgPart)
	}
}

// #vision: a text-only array-content message must keep producing the legacy
// plain-string content (no behavior change for the common non-vision case).
func TestResponsesToChatArrayInput_NoImageStaysString(t *testing.T) {
	in := []byte(`{"model":"x","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}
	]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	content := msgs[0].(map[string]any)["content"]
	if _, isString := content.(string); !isString {
		t.Fatalf("text-only content must stay a plain string, got %T: %v", content, content)
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

// A provider returning array-shaped assistant content (some do, for
// structured/refusal responses) must not hard-fail the whole translation —
// the text parts should still be extracted and concatenated.
func TestChatToResponsesArrayContent(t *testing.T) {
	chat := []byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"Hello"},{"type":"text","text":"world"}]},"finish_reason":"stop"}]}`)
	out, err := ChatToResponsesResponse(chat, "m")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	output := m["output"].([]any)
	msg := output[0].(map[string]any)
	content := msg["content"].([]any)
	txt := content[0].(map[string]any)["text"]
	if txt != "Hello\nworld" {
		t.Fatalf("text: %v", txt)
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
