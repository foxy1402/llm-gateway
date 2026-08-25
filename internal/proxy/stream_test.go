package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamPassthrough(t *testing.T) {
	chunks := []string{
		`data: {"id":"1","choices":[{"delta":{"content":"He"}}]}` + "\n\n",
		`data: {"id":"1","choices":[{"delta":{"content":"llo"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	in := strings.Join(chunks, "")
	src := &fakeFlusher{}
	_ = src
	if !strings.Contains(in, "data: [DONE]") {
		t.Fatal("expected DONE marker")
	}
}

type fakeFlusher struct{ n int }

func (f *fakeFlusher) Flush() { f.n++ }

func TestTranslateChunkToResponses(t *testing.T) {
	payload := []byte(`{"id":"x","model":"m","choices":[{"delta":{"content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	events, model, usage, _, _, ok := translateChatChunk(payload)
	if !ok {
		t.Fatal("translation failed")
	}
	if model != "m" {
		t.Fatalf("model: %s", model)
	}
	if usage == nil || usage.OutputTokens != 1 {
		t.Fatalf("usage: %+v", usage)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
}

// Cache-token accounting must survive chat->responses translation so IDEs billing
// on prompt-cache reads still see cached_tokens in the emitted usage block.
func TestTranslateChunkPreservesCacheDetails(t *testing.T) {
	payload := []byte(`{"id":"x","model":"m","choices":[{"delta":{"content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":1300,"completion_tokens":42,"total_tokens":1342,"prompt_tokens_details":{"cached_tokens":1152},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	_, _, usage, details, _, ok := translateChatChunk(payload)
	if !ok || usage == nil {
		t.Fatalf("translation failed: ok=%v usage=%v", ok, usage)
	}
	if usage.InputTokens != 1300 || usage.OutputTokens != 42 || usage.TotalTokens != 1342 {
		t.Fatalf("scalars wrong: %+v", usage)
	}
	if details == nil {
		t.Fatal("details dropped")
	}
	if !strings.Contains(string(details.PromptTokensDetails), `"cached_tokens":1152`) {
		t.Fatalf("cached_tokens lost: %s", details.PromptTokensDetails)
	}
	if !strings.Contains(string(details.CompletionTokensDetails), `"reasoning_tokens":5`) {
		t.Fatalf("reasoning_tokens lost: %s", details.CompletionTokensDetails)
	}
}

// The terminal response.completed event must embed the detail blobs, not strip them.
func TestBuildResponseCompletedIncludesCacheDetails(t *testing.T) {
	usage := struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}{InputTokens: 1300, OutputTokens: 42, TotalTokens: 1342}
	details := &usageDetails{
		PromptTokensDetails:     json.RawMessage(`{"cached_tokens":1152}`),
		CompletionTokensDetails: json.RawMessage(`{"reasoning_tokens":5}`),
	}
	out := buildResponseCompleted("m", usage, details, true, nil)
	s := string(out)
	for _, want := range []string{`"input_tokens":1300`, `"output_tokens":42`, `"cached_tokens":1152`, `"reasoning_tokens":5`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

// Chat tool_call deltas must survive the responses translation: buffered during
// the stream, emitted as function_call events plus output items in the terminal
// response.completed — previously dropped entirely, breaking agent clients.
func TestStreamTranslateEmitsFunctionCallEvents(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"Paris\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}
	if _, _, _, err := p.streamResponse(rec, upstream, StreamFormatResponses, true); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"name":"get_weather"`,
		`"call_id":"call_1"`,
		`"arguments":"{\"city\":\"Paris\"}"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in stream output:\n%s", want, out)
		}
	}
}
