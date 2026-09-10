package proxy

import (
	"encoding/json"
	"testing"
)

// decodeChatMessages pulls the translated chat-completions messages array out of
// a ResponsesToChatRequest result.
func decodeChatMessages(t *testing.T, out []byte) []map[string]json.RawMessage {
	t.Helper()
	var m struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	return m.Messages
}

func msgString(t *testing.T, msg map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := msg[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// Responses-only input items (reasoning traces, built-in tool calls) carry no
// role and no content. The old catch-all default branch turned each one into a
// {role:"user",content:""} message: strict upstreams 400 an empty user turn, and
// lenient ones let the ghost message displace real context.
func TestResponsesToChatSkipsRoleLessItems(t *testing.T) {
	in := []byte(`{
		"model":"x",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"reasoning","id":"rs_1","summary":[]},
			{"type":"web_search_call","id":"ws_1","status":"completed"},
			{"type":"code_interpreter_call","id":"ci_1"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}
		]
	}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeChatMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (the reasoning/web_search/code_interpreter items must be dropped), got %d: %s", len(msgs), out)
	}
	if got := msgString(t, msgs[0], "role"); got != "user" {
		t.Errorf("message 0 role = %q, want user", got)
	}
	if got := msgString(t, msgs[0], "content"); got != "hi" {
		t.Errorf("message 0 content = %q, want hi", got)
	}
	if got := msgString(t, msgs[1], "role"); got != "assistant" {
		t.Errorf("message 1 role = %q, want assistant", got)
	}
	for _, m := range msgs {
		if msgString(t, m, "content") == "" {
			t.Errorf("empty-content ghost message survived translation: %s", out)
		}
	}
}

// An unrecognized item type that DOES carry a role is a real conversation turn
// (vendor extension, or a spec addition postdating this code). Dropping it would
// throw away the user's text, so it must still translate as a message.
func TestResponsesToChatKeepsUnknownItemWithRole(t *testing.T) {
	in := []byte(`{"model":"x","input":[{"type":"some_future_turn","role":"user","content":"keep me"}]}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeChatMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d: %s", len(msgs), out)
	}
	if got := msgString(t, msgs[0], "content"); got != "keep me" {
		t.Errorf("content = %q, want %q", got, "keep me")
	}
}

// input_audio is spelled identically in both APIs; before the fix the part was
// silently dropped and the model was asked to transcribe silence.
func TestResponsesToChatPreservesAudioPart(t *testing.T) {
	in := []byte(`{
		"model":"x",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"transcribe this"},
			{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}
		]}]
	}`)
	out, err := ResponsesToChatRequest(in, "x")
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeChatMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0]["content"], &parts); err != nil {
		t.Fatalf("audio message must use the array content form, got %s", msgs[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 content parts, got %d: %s", len(parts), msgs[0]["content"])
	}
	var typ string
	json.Unmarshal(parts[1]["type"], &typ)
	if typ != "input_audio" {
		t.Fatalf("part 1 type = %q, want input_audio", typ)
	}
	var audio struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(parts[1]["input_audio"], &audio); err != nil {
		t.Fatalf("input_audio payload lost: %v", err)
	}
	if audio.Data != "QUJD" || audio.Format != "wav" {
		t.Errorf("audio payload = %+v, want data=QUJD format=wav", audio)
	}
}

// Same in the reroute direction: a voice request routed chat->responses keeps
// its audio.
func TestChatToResponsesPreservesAudioPart(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"input_audio","input_audio":{"data":"QUJD","format":"mp3"}}
	]}]}`)
	out, err := ChatRequestToResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 1 || len(got.Input[0].Content) != 2 {
		t.Fatalf("want 1 item with 2 parts, got %s", out)
	}
	var typ string
	json.Unmarshal(got.Input[0].Content[1]["type"], &typ)
	if typ != "input_audio" {
		t.Fatalf("part 1 type = %q, want input_audio: %s", typ, out)
	}
	if _, ok := got.Input[0].Content[1]["input_audio"]; !ok {
		t.Errorf("input_audio payload dropped: %s", out)
	}
}

// A chat refusal has null content, so before the fix the translated Responses
// payload was an empty assistant message with no explanation.
func TestChatToResponsesSurfacesRefusal(t *testing.T) {
	chat := []byte(`{"id":"c1","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`)
	out, err := ChatToResponsesResponse(chat, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range got.Output {
		for _, c := range item.Content {
			if c.Type == "refusal" && c.Refusal == "I can't help with that." {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("refusal not surfaced as a refusal content part: %s", out)
	}
}

// reasoning_content (DeepSeek-R1-style upstreams) becomes a Responses reasoning
// item so clients that render thinking still see it.
func TestChatToResponsesSurfacesReasoning(t *testing.T) {
	chat := []byte(`{"id":"c1","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":"42","reasoning_content":"thinking hard"},"finish_reason":"stop"}]}`)
	out, err := ChatToResponsesResponse(chat, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Output) < 2 {
		t.Fatalf("want reasoning item plus message, got %s", out)
	}
	if got.Output[0].Type != "reasoning" {
		t.Fatalf("output[0].type = %q, want reasoning (it must precede the answer): %s", got.Output[0].Type, out)
	}
	if len(got.Output[0].Summary) != 1 || got.Output[0].Summary[0].Text != "thinking hard" {
		t.Errorf("reasoning summary lost: %s", out)
	}
}

// A content_filter stop must not look "completed" to a Responses client, or a
// blocked generation is indistinguishable from a finished one.
func TestChatToResponsesMapsFinishReasonToStatus(t *testing.T) {
	cases := []struct {
		finish     string
		wantStatus string
		wantReason string
	}{
		{"stop", "completed", ""},
		{"length", "incomplete", "max_output_tokens"},
		{"content_filter", "incomplete", "content_filter"},
	}
	for _, tc := range cases {
		chat := []byte(`{"id":"c1","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.finish + `"}]}`)
		out, err := ChatToResponsesResponse(chat, "client-model")
		if err != nil {
			t.Fatalf("%s: %v", tc.finish, err)
		}
		var got struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v", tc.finish, err)
		}
		if got.Status != tc.wantStatus {
			t.Errorf("finish_reason=%s: status = %q, want %q", tc.finish, got.Status, tc.wantStatus)
		}
		if tc.wantReason == "" {
			if got.IncompleteDetails != nil {
				t.Errorf("finish_reason=%s: unexpected incomplete_details %+v", tc.finish, got.IncompleteDetails)
			}
			continue
		}
		if got.IncompleteDetails == nil || got.IncompleteDetails.Reason != tc.wantReason {
			t.Errorf("finish_reason=%s: incomplete_details.reason = %+v, want %q", tc.finish, got.IncompleteDetails, tc.wantReason)
		}
	}
}

// metadata/service_tier exist verbatim in both APIs. The reroute used to drop
// them, silently downgrading a caller's billing tier and losing their tags.
func TestChatToResponsesRerouteForwardsMetadataAndServiceTier(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"trace":"abc"},"service_tier":"priority"}`)
	out, err := ChatRequestToResponsesRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Metadata    map[string]string `json:"metadata"`
		ServiceTier string            `json:"service_tier"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata["trace"] != "abc" {
		t.Errorf("metadata dropped: %s", out)
	}
	if got.ServiceTier != "priority" {
		t.Errorf("service_tier = %q, want priority: %s", got.ServiceTier, out)
	}
}

// A refusal coming back from a Responses-native upstream must reach the chat
// client on the first-class "refusal" field, not vanish.
func TestResponsesToChatResponseSurfacesRefusal(t *testing.T) {
	resp := []byte(`{"id":"resp_1","created_at":1,"model":"m","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"nope"}]}]}`)
	out, err := ResponsesResponseToChatResponse(resp, "client-model")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content any    `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("want 1 choice: %s", out)
	}
	if got.Choices[0].Message.Refusal != "nope" {
		t.Errorf("refusal = %q, want nope: %s", got.Choices[0].Message.Refusal, out)
	}
	if got.Choices[0].Message.Content != nil {
		t.Errorf("a pure refusal must carry null content, got %#v", got.Choices[0].Message.Content)
	}
}
