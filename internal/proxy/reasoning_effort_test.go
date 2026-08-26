package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

// genericReasoningEffortWording simulates a hypothetical provider that
// reports the same class of conflict as Lightning AI's "openai/gpt-5.6-sol"
// but, unlike Lightning, does NOT (incorrectly) suggest /v1/responses as an
// alternative — i.e. a provider where "set reasoning_effort to none" is
// actually the correct, sufficient fix. Kept distinct from
// lightningReasoningEffortWording (responses_reroute_test.go) so these two
// heals' tests don't collide: tryResponsesRerouteHeal is tried first and
// takes priority whenever the message mentions "/v1/responses".
const genericReasoningEffortWording = "reasoning_effort is not compatible with tool calls for this model."

func TestLooksLikeReasoningEffortToolsConflict(t *testing.T) {
	positive := []string{
		genericReasoningEffortWording,
		lightningReasoningEffortWording, // also matches; reroute heal just takes priority
		`{"error":"reasoning_effort is not compatible with tool calls"}`,
	}
	for _, body := range positive {
		if !looksLikeReasoningEffortToolsConflict([]byte(body)) {
			t.Errorf("expected match for: %s", body)
		}
	}
	negative := []string{
		lightningAIWording,       // field-name mismatch, unrelated
		lightningTooSmallWording, // budget-too-small, unrelated
		`{"error":"rate limited"}`,
		"",
	}
	for _, body := range negative {
		if looksLikeReasoningEffortToolsConflict([]byte(body)) {
			t.Errorf("unexpected match for: %s", body)
		}
	}
}

func TestApplyReasoningEffortNone(t *testing.T) {
	t.Run("adds reasoning_effort=none when tools present", func(t *testing.T) {
		out, ok := applyReasoningEffortNone([]byte(`{"model":"m","tools":[{"type":"function"}],"messages":[]}`))
		if !ok {
			t.Fatal("expected apply to succeed")
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		if string(m["reasoning_effort"]) != `"none"` {
			t.Fatalf("reasoning_effort = %s, want \"none\"", m["reasoning_effort"])
		}
		if string(m["model"]) != `"m"` {
			t.Fatalf("unrelated field corrupted: %s", m["model"])
		}
	})
	t.Run("overwrites a non-none reasoning_effort when tools present", func(t *testing.T) {
		out, ok := applyReasoningEffortNone([]byte(`{"tools":[{}],"reasoning_effort":"medium"}`))
		if !ok {
			t.Fatal("expected apply to succeed")
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		if string(m["reasoning_effort"]) != `"none"` {
			t.Fatalf("reasoning_effort = %s, want \"none\"", m["reasoning_effort"])
		}
	})
	t.Run("no tools field: no-op", func(t *testing.T) {
		_, ok := applyReasoningEffortNone([]byte(`{"messages":[]}`))
		if ok {
			t.Fatal("expected no-op when there's no tools array")
		}
	})
	t.Run("already none: no-op", func(t *testing.T) {
		_, ok := applyReasoningEffortNone([]byte(`{"tools":[{}],"reasoning_effort":"none"}`))
		if ok {
			t.Fatal("expected no-op when reasoning_effort is already none")
		}
	})
}

// TestReasoningEffortHealForToolCalls reproduces the exact live failure: a
// tool-call request with no reasoning_effort field is rejected by a
// reasoning-tier model. The gateway must inject reasoning_effort: "none" and
// retry transparently, on the same account, without surfacing an error.
func TestReasoningEffortHealForToolCalls(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		defer mu.Unlock()
		if _, has := b["reasoning_effort"]; !has {
			calls = append(calls, "no_reasoning_effort")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(genericReasoningEffortWording))
			return
		}
		var s string
		_ = json.Unmarshal(b["reasoning_effort"], &s)
		calls = append(calls, "reasoning_effort="+s)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"lightning","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected transparent success, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "no_reasoning_effort" || calls[1] != "reasoning_effort=none" {
		t.Fatalf("unexpected call sequence: %v", calls)
	}
}

// TestReasoningEffortLearnedAvoidsSecondRoundTrip: after a live heal, the next
// tool-call request from the SAME account must apply reasoning_effort: "none"
// proactively, in a single upstream call.
func TestReasoningEffortLearnedAvoidsSecondRoundTrip(t *testing.T) {
	var mu sync.Mutex
	callCounts := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		defer mu.Unlock()
		if _, has := b["reasoning_effort"]; !has {
			callCounts["without"]++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(genericReasoningEffortWording))
			return
		}
		callCounts["with"]++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	toolReq := `{"model":"lightning","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`

	// First request: pays the failing round trip and learns the fix.
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(toolReq))
	rec1 := httptest.NewRecorder()
	px.ServeHTTP(rec1, req1, registry.EndpointChatCompletions)
	if rec1.Code != 200 {
		t.Fatalf("first request: expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// Second request: must skip straight to reasoning_effort: "none".
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(toolReq))
	rec2 := httptest.NewRecorder()
	px.ServeHTTP(rec2, req2, registry.EndpointChatCompletions)
	if rec2.Code != 200 {
		t.Fatalf("second request: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if callCounts["without"] != 1 {
		t.Fatalf("expected exactly 1 failing call (from the first request only), got %d", callCounts["without"])
	}
	if callCounts["with"] != 2 {
		t.Fatalf("expected 2 successful calls (heal retry + proactive second request), got %d", callCounts["with"])
	}
}

// TestReasoningEffortHealDoesNotBurnRotationBudget: single-key provider (no
// sibling to rotate to) must still self-heal, proving this is request-shape,
// not account-scoped.
func TestReasoningEffortHealDoesNotBurnRotationBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, has := b["reasoning_effort"]; !has {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(genericReasoningEffortWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, reg := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"solo","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("single-key provider should still self-heal, got %d: %s", rec.Code, rec.Body.String())
	}
	if !reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("the account must NOT be put in cooldown by a tool/reasoning_effort conflict — it's not an account-scoped failure")
	}
}

// TestReasoningEffortHealDoesNotFireWithoutTools: a non-tool-call request
// hitting an unrelated 500 that happens to mention neither keyword together
// must not trigger a spurious retry loop or field injection.
func TestReasoningEffortHealDoesNotFireWithoutTools(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[]}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code == 200 {
		t.Fatal("expected the unrelated error to surface, not a false-positive heal")
	}
}
