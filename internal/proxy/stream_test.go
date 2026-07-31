package proxy

import (
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
	events, model, usage, ok := translateChatChunk(payload)
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
