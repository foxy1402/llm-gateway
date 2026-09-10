package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func newSSEUpstream(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Many providers emit the usage-bearing frame AFTER data: [DONE] (or in the same
// TCP segment right behind it). Returning the instant [DONE] arrives lost that
// frame, so those requests logged 0 prompt/completion tokens.
func TestStreamCapturesUsageAfterDone(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n"
	p := &Proxy{}
	pt, ct, _, err := p.streamResponse(httptest.NewRecorder(), newSSEUpstream(body), StreamFormatChat, false, context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if pt == nil || ct == nil {
		t.Fatalf("usage after [DONE] was dropped: pt=%v ct=%v", pt, ct)
	}
	if *pt != 11 || *ct != 7 {
		t.Errorf("usage = %d/%d, want 11/7", *pt, *ct)
	}
}

// A trailing all-zero usage frame (some mirrors emit one) must not clobber the
// real numbers captured earlier in the stream.
func TestStreamZeroUsageDoesNotClobber(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n" +
		"data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"
	p := &Proxy{}
	pt, ct, _, err := p.streamResponse(httptest.NewRecorder(), newSSEUpstream(body), StreamFormatChat, false, context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if pt == nil || ct == nil {
		t.Fatal("usage lost entirely")
	}
	if *pt != 100 || *ct != 20 {
		t.Errorf("usage = %d/%d, want 100/20 (a zero frame must not overwrite real usage)", *pt, *ct)
	}
}

// Plenty of upstreams just close the connection instead of sending [DONE]. In
// responses-translation mode the terminal response.completed event (and any
// buffered tool calls) still has to be emitted, or the client hangs waiting for
// a completion it will never get.
func TestStreamEOFWithoutDoneEmitsTerminalEvents(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n"
	// No [DONE], no trailing marker — the body simply ends.
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, _, _, err := p.streamResponse(rec, newSSEUpstream(body), StreamFormatResponses, true, context.Background()); err != nil {
		t.Fatalf("a clean EOF is not an error: %v", err)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.completed",
		"event: response.output_item.done",
		`"name":"lookup"`,
		`"call_id":"call_9"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q after EOF-without-[DONE]:\n%s", want, out)
		}
	}
}

// Some providers omit the tool-call id on the opening delta. A generated id must
// be assigned ONCE and reused across every event for that call, or the client
// sees output_item.added and function_call_arguments.done referring to different
// calls and can never match the result back.
func TestStreamGeneratedToolCallIDIsStable(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, _, _, err := p.streamResponse(rec, newSSEUpstream(body), StreamFormatResponses, true, context.Background()); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	ids := regexp.MustCompile(`"call_id":"([^"]+)"`).FindAllStringSubmatch(out, -1)
	if len(ids) < 2 {
		t.Fatalf("want at least 2 call_id occurrences, got %d:\n%s", len(ids), out)
	}
	first := ids[0][1]
	if first == "" {
		t.Fatal("empty generated call_id")
	}
	for _, m := range ids[1:] {
		if m[1] != first {
			t.Fatalf("call_id changed mid-stream: %q vs %q\n%s", first, m[1], out)
		}
	}
}

// The SSE spec allows "data:[DONE]" with no space after the colon, and real
// providers send it. Byte-exact matching on "data: [DONE]" treated that as an
// ordinary payload and kept reading until the stall timer or EOF.
func TestStreamTolerantDoneSentinel(t *testing.T) {
	for _, sentinel := range []string{"data:[DONE]\n\n", "data: [done]\n\n", "data:  [DONE]  \n\n"} {
		body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" + sentinel
		p := &Proxy{}
		rec := httptest.NewRecorder()
		pt, _, _, err := p.streamResponse(rec, newSSEUpstream(body), StreamFormatResponses, true, context.Background())
		if err != nil {
			t.Fatalf("%q: %v", sentinel, err)
		}
		if pt == nil || *pt != 3 {
			t.Errorf("%q: usage = %v, want 3", sentinel, pt)
		}
		if !strings.Contains(rec.Body.String(), "event: response.completed") {
			t.Errorf("%q was not recognized as the end of stream:\n%s", sentinel, rec.Body.String())
		}
	}
}

// Passthrough mode must reproduce the SSE frame boundary: an upstream line
// lacking the blank-line terminator still has to reach the client as a complete
// frame, or a strict client buffers [DONE] forever.
func TestStreamPassthroughDoneKeepsFrameBoundary(t *testing.T) {
	body := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n"
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, _, _, err := p.streamResponse(rec, newSSEUpstream(body), StreamFormatChat, false, context.Background()); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("[DONE] frame is missing its blank-line terminator: %q", out)
	}
}

// A nil upstream body (some transports hand back a bodyless response on a 200
// with no content) must not panic the stream path.
func TestStreamNilUpstreamBody(t *testing.T) {
	upstream := &http.Response{StatusCode: 200, Header: make(http.Header)}
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, _, _, err := p.streamResponse(rec, upstream, StreamFormatChat, false, context.Background()); err != nil {
		t.Fatalf("nil body should be an empty stream, not an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// Only choice 0 belongs in a single Responses stream. Before the index guard, an
// n>1 upstream interleaved every choice's deltas into one text field, producing
// garbled output.
func TestTranslateChunkSkipsNonZeroChoiceIndex(t *testing.T) {
	payload := []byte(`{"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"FIRST"}},{"index":1,"delta":{"content":"SECOND"}}]}`)
	events, _, _, _, _, ok := translateChatChunk(payload)
	if !ok {
		t.Fatal("translation failed")
	}
	joined := strings.Join(eventStrings(events), "")
	if !strings.Contains(joined, "FIRST") {
		t.Fatalf("choice 0 text missing: %s", joined)
	}
	if strings.Contains(joined, "SECOND") {
		t.Fatalf("choice index 1 leaked into the responses stream: %s", joined)
	}
}

// A delta carrying only reasoning_content must surface as a reasoning summary
// delta rather than vanishing.
func TestTranslateChunkEmitsReasoningDelta(t *testing.T) {
	payload := []byte(`{"id":"x","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`)
	events, _, _, _, _, ok := translateChatChunk(payload)
	if !ok {
		t.Fatal("translation failed")
	}
	joined := strings.Join(eventStrings(events), "")
	if !strings.Contains(joined, "response.reasoning_summary_text.delta") || !strings.Contains(joined, "thinking") {
		t.Fatalf("reasoning delta dropped: %s", joined)
	}
}

// A streamed refusal must surface as response.refusal.delta; as output_text it
// would be indistinguishable from a normal answer.
func TestTranslateChunkEmitsRefusalDelta(t *testing.T) {
	payload := []byte(`{"id":"x","model":"m","choices":[{"index":0,"delta":{"refusal":"I can't"}}]}`)
	events, _, _, _, _, ok := translateChatChunk(payload)
	if !ok {
		t.Fatal("translation failed")
	}
	joined := strings.Join(eventStrings(events), "")
	if !strings.Contains(joined, "response.refusal.delta") || !strings.Contains(joined, "I can't") {
		t.Fatalf("refusal delta dropped: %s", joined)
	}
}

func eventStrings(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.event+" "+string(e.data))
	}
	return out
}

// Sanity check that the emitted terminal event is valid JSON per frame — a
// malformed terminal payload breaks strict SSE clients even when the text is
// right.
func TestStreamTerminalEventIsValidJSON(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	p := &Proxy{}
	rec := httptest.NewRecorder()
	if _, _, _, err := p.streamResponse(rec, newSSEUpstream(body), StreamFormatResponses, true, context.Background()); err != nil {
		t.Fatalf("stream: %v", err)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Errorf("emitted non-JSON SSE payload %q: %v", payload, err)
		}
	}
}
