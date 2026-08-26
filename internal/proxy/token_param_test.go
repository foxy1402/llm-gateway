package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

// Real-world wording captured from live upstreams (see conversation history):
// OpenAI's reasoning-tier models and any OpenAI-compatible provider mirroring
// that restriction (observed live against Lightning AI's "openai/gpt-5.6-sol").
const (
	openAIWording      = "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."
	lightningAIWording = "this model is not supported MaxTokens, please use MaxCompletionTokens"
)

func TestLooksLikeTokenParamMismatch(t *testing.T) {
	positive := []string{
		openAIWording,
		lightningAIWording,
		`{"error":{"message":"` + openAIWording + `","param":"max_tokens","code":"unsupported_parameter"}}`,
	}
	for _, body := range positive {
		if !looksLikeTokenParamMismatch([]byte(body)) {
			t.Errorf("expected match for: %s", body)
		}
	}
	negative := []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"rate limit exceeded"}}`,
		"internal server error",
		"", // empty body must not false-positive
		`{"error":{"message":"max_tokens must be a positive integer"}}`, // only one keyword present
	}
	for _, body := range negative {
		if looksLikeTokenParamMismatch([]byte(body)) {
			t.Errorf("unexpected match for: %s", body)
		}
	}
}

func TestSwapTokenParamField(t *testing.T) {
	t.Run("max_tokens to max_completion_tokens", func(t *testing.T) {
		out, field, ok := swapTokenParamField([]byte(`{"model":"m","max_tokens":16,"messages":[]}`))
		if !ok {
			t.Fatal("expected swap to succeed")
		}
		if field != TokenParamMaxCompletion {
			t.Fatalf("resultField = %q, want %q", field, TokenParamMaxCompletion)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if _, has := m["max_tokens"]; has {
			t.Fatal("max_tokens should have been removed")
		}
		if string(m["max_completion_tokens"]) != "16" {
			t.Fatalf("max_completion_tokens = %s, want 16", m["max_completion_tokens"])
		}
		if string(m["model"]) != `"m"` {
			t.Fatalf("unrelated field model corrupted: %s", m["model"])
		}
	})

	t.Run("max_completion_tokens to max_tokens (reverse direction)", func(t *testing.T) {
		out, field, ok := swapTokenParamField([]byte(`{"model":"m","max_completion_tokens":32}`))
		if !ok {
			t.Fatal("expected swap to succeed")
		}
		if field != TokenParamMaxTokens {
			t.Fatalf("resultField = %q, want %q", field, TokenParamMaxTokens)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		if _, has := m["max_completion_tokens"]; has {
			t.Fatal("max_completion_tokens should have been removed")
		}
		if string(m["max_tokens"]) != "32" {
			t.Fatalf("max_tokens = %s, want 32", m["max_tokens"])
		}
	})

	t.Run("neither field present: no swap", func(t *testing.T) {
		_, _, ok := swapTokenParamField([]byte(`{"model":"m","messages":[]}`))
		if ok {
			t.Fatal("expected no swap when neither field is present")
		}
	})

	t.Run("both fields present: ambiguous, no swap", func(t *testing.T) {
		_, _, ok := swapTokenParamField([]byte(`{"model":"m","max_tokens":16,"max_completion_tokens":32}`))
		if ok {
			t.Fatal("expected no swap when both fields are present (ambiguous)")
		}
	})

	t.Run("malformed body: no swap, no panic", func(t *testing.T) {
		_, _, ok := swapTokenParamField([]byte(`not json`))
		if ok {
			t.Fatal("expected no swap for malformed JSON")
		}
	})
}

func TestApplyTokenParamMode(t *testing.T) {
	t.Run("forces max_completion_tokens, renaming max_tokens", func(t *testing.T) {
		out, ok := applyTokenParamMode([]byte(`{"max_tokens":16}`), TokenParamMaxCompletion)
		if !ok {
			t.Fatal("expected apply to succeed")
		}
		if strings.Contains(string(out), "max_tokens\"") && !strings.Contains(string(out), "max_completion_tokens") {
			t.Fatalf("field not renamed: %s", out)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		if _, has := m["max_tokens"]; has {
			t.Fatal("max_tokens should be gone")
		}
		if string(m["max_completion_tokens"]) != "16" {
			t.Fatalf("max_completion_tokens = %s, want 16", m["max_completion_tokens"])
		}
	})

	t.Run("already the target field: no-op", func(t *testing.T) {
		_, ok := applyTokenParamMode([]byte(`{"max_completion_tokens":16}`), TokenParamMaxCompletion)
		if ok {
			t.Fatal("expected no-op when already the target field")
		}
	})

	t.Run("neither field present: no-op", func(t *testing.T) {
		_, ok := applyTokenParamMode([]byte(`{"messages":[]}`), TokenParamMaxCompletion)
		if ok {
			t.Fatal("expected no-op when neither field is present")
		}
	})

	t.Run("invalid mode: no-op", func(t *testing.T) {
		_, ok := applyTokenParamMode([]byte(`{"max_tokens":16}`), "bogus")
		if ok {
			t.Fatal("expected no-op for an unrecognized mode")
		}
	})
}

// TestAutoHealMaxTokensParam reproduces the exact live failure: a model rejects
// max_tokens with the dual-keyword error (mirroring Lightning AI's wording and
// its unusual 500 status), and accepts max_completion_tokens. A single client
// request using max_tokens must succeed transparently — no error surfaced,
// no config required.
func TestAutoHealMaxTokensParam(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		if _, ok := b["max_tokens"]; ok {
			calls = append(calls, "max_tokens")
		} else if _, ok := b["max_completion_tokens"]; ok {
			calls = append(calls, "max_completion_tokens")
		}
		mu.Unlock()
		if _, ok := b["max_tokens"]; ok {
			w.WriteHeader(http.StatusInternalServerError) // mirrors Lightning AI's real status
			_, _ = w.Write([]byte(lightningAIWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"lightning","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected transparent success, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 upstream calls (original + healed retry), got %d: %v", len(calls), calls)
	}
	if calls[0] != "max_tokens" || calls[1] != "max_completion_tokens" {
		t.Fatalf("expected [max_tokens, max_completion_tokens], got %v", calls)
	}
}

// TestAutoHealDoesNotBurnAccountOrRotationBudget: with only ONE key on the
// provider (maxAttempts == 1 for a direct-provider call), the healed retry
// must still succeed — proving the heal redispatch happens INSIDE the same
// attempt rather than consuming the account-rotation budget. If it consumed a
// rotation attempt instead, this single-key provider would have nothing left
// to retry with and the request would fail.
func TestAutoHealDoesNotBurnAccountOrRotationBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["max_tokens"]; ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningAIWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, reg := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"max_tokens":16}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("single-key provider should still self-heal via field swap, got %d: %s", rec.Code, rec.Body.String())
	}
	if !reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("the account must NOT be put in cooldown by a field-name mismatch — it's not an account-scoped failure")
	}
}

// lightningTooSmallWording is the real wording captured live from Lightning
// AI's "openai/gpt-5.6-sol" when max_tokens/max_completion_tokens is too small
// for a reasoning-tier model to emit any output (e.g. a client's max_tokens: 1
// "does this key work" connection probe).
const lightningTooSmallWording = "Could not finish the message because max_tokens or model output limit was reached. Please try again with higher max_tokens."

func TestLooksLikeTokenBudgetTooSmall(t *testing.T) {
	positive := []string{
		lightningTooSmallWording,
		`{"error":"max_tokens or model output limit was reached"}`,
		`{"error":"please try again with higher max_completion_tokens"}`,
	}
	for _, body := range positive {
		if !looksLikeTokenBudgetTooSmall([]byte(body)) {
			t.Errorf("expected match for: %s", body)
		}
	}
	negative := []string{
		lightningAIWording, // field-name mismatch, not a budget problem
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"rate limit exceeded"}}`,
		"",
	}
	for _, body := range negative {
		if looksLikeTokenBudgetTooSmall([]byte(body)) {
			t.Errorf("unexpected match for: %s", body)
		}
	}
}

func TestBumpTokenFloor(t *testing.T) {
	t.Run("bumps a too-small max_tokens", func(t *testing.T) {
		out, old, ok := bumpTokenFloor([]byte(`{"model":"m","max_tokens":1}`), 16)
		if !ok || old != 1 {
			t.Fatalf("expected bump from 1, got old=%v ok=%v", old, ok)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		if string(m["max_tokens"]) != "16" {
			t.Fatalf("max_tokens = %s, want 16", m["max_tokens"])
		}
	})
	t.Run("bumps a too-small max_completion_tokens", func(t *testing.T) {
		out, old, ok := bumpTokenFloor([]byte(`{"max_completion_tokens":1}`), 16)
		if !ok || old != 1 {
			t.Fatalf("expected bump from 1, got old=%v ok=%v", old, ok)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		if string(m["max_completion_tokens"]) != "16" {
			t.Fatalf("max_completion_tokens = %s, want 16", m["max_completion_tokens"])
		}
	})
	t.Run("already at/above floor: no-op", func(t *testing.T) {
		_, _, ok := bumpTokenFloor([]byte(`{"max_tokens":16}`), 16)
		if ok {
			t.Fatal("expected no-op when already at the floor")
		}
	})
	t.Run("neither field present: no-op", func(t *testing.T) {
		_, _, ok := bumpTokenFloor([]byte(`{"messages":[]}`), 16)
		if ok {
			t.Fatal("expected no-op when neither field is present")
		}
	})
	t.Run("both fields present: ambiguous, no-op", func(t *testing.T) {
		_, _, ok := bumpTokenFloor([]byte(`{"max_tokens":1,"max_completion_tokens":1}`), 16)
		if ok {
			t.Fatal("expected no-op when both fields are present")
		}
	})
}

// TestChainedHealFieldNameThenTooSmallBudget reproduces the exact live bug: a
// client probes with max_tokens: 1 (a common "does this key work" connection
// test). The field-name heal fires first (max_tokens -> max_completion_tokens)
// but the model STILL can't finish because 1 is too small for a reasoning-tier
// model to emit anything — a second, independent heal must bump the value and
// retry again, all within one attempt, so the client sees a transparent 200.
func TestChainedHealFieldNameThenTooSmallBudget(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		defer mu.Unlock()
		if raw, ok := b["max_tokens"]; ok {
			calls = append(calls, "max_tokens="+string(raw))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningAIWording))
			return
		}
		var n float64
		_ = json.Unmarshal(b["max_completion_tokens"], &n)
		calls = append(calls, fmt.Sprintf("max_completion_tokens=%v", n))
		if n < 16 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningTooSmallWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"gpt-5.6-sol","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "openai/gpt-5.6-sol", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"lightning","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected transparent success after chained heal, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("expected exactly 3 upstream calls (original + field heal + budget heal), got %d: %v", len(calls), calls)
	}
	if calls[0] != "max_tokens=1" || calls[1] != "max_completion_tokens=1" || calls[2] != "max_completion_tokens=16" {
		t.Fatalf("unexpected call sequence: %v", calls)
	}
}

// TestMinTokensHealDoesNotBurnRotationBudget proves this is a request-shape
// fix, not an account-scoped one: a single-key provider (no sibling to rotate
// to) must still self-heal, and the key must not be cooled down afterward.
func TestMinTokensHealDoesNotBurnRotationBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		var n float64
		_ = json.Unmarshal(b["max_tokens"], &n)
		if n < 16 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningTooSmallWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, reg := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"max_tokens":1}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("single-key provider should still self-heal via budget bump, got %d: %s", rec.Code, rec.Body.String())
	}
	if !reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("the account must NOT be put in cooldown by a too-small budget — it's not an account-scoped failure")
	}
}

// TestMinCompletionTokensZeroDisablesHeal: an operator who wants raw upstream
// errors surfaced instead of a silently-bumped budget can opt out entirely.
func TestMinCompletionTokensZeroDisablesHeal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(lightningTooSmallWording))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "solo", BaseURL: upstream.URL, AuthKey: "only-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)
	px.SetMinCompletionTokens(0)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"max_tokens":1}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code == 200 {
		t.Fatal("expected the heal to be disabled and the raw error to surface")
	}
}

// TestManualTokenParamModeSkipsHealRoundTrip proves the "tick a box" half of
// smart mode: with Provider.TokenParamMode explicitly set, the very FIRST
// upstream call already uses the right field — no failing round trip, no
// added latency, ever.
func TestManualTokenParamModeSkipsHealRoundTrip(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["max_tokens"]; ok {
			calls = append(calls, "max_tokens")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningAIWording))
			return
		}
		calls = append(calls, "max_completion_tokens")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true,
		TokenParamMode: TokenParamMaxCompletion,
	}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"lightning","messages":[],"max_tokens":16}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("expected success, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("manual override must avoid the heal round trip entirely: expected exactly 1 upstream call, got %d: %v", len(calls), calls)
	}
	if calls[0] != "max_completion_tokens" {
		t.Fatalf("expected the single call to already use max_completion_tokens, got %v", calls)
	}
}

// TestTokenParamModePrecedence: a combo member's explicit pin beats the
// account's, which beats the provider's — mirrors the existing Model
// precedence (member > account > provider).
func TestTokenParamModePrecedence(t *testing.T) {
	var lastField string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["max_tokens"]; ok {
			lastField = "max_tokens"
		} else if _, ok := b["max_completion_tokens"]; ok {
			lastField = "max_completion_tokens"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	accounts := []config.Account{
		{ID: "p:a1", ProviderID: "p", Label: "a1", AuthKey: "k1", Enabled: true, Weight: 1, TokenParamMode: TokenParamMaxTokens}, // account overrides to "old field"
	}
	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		TokenParamMode: TokenParamMaxCompletion, // provider says "new field"
		Accounts:       accounts,
	}}
	combos := []config.Combo{{
		ID: "c", DisplayName: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p", AccountID: "p:a1", TokenParamMode: TokenParamMaxCompletion}}, // member overrides back to "new field"
	}}
	px, st, reg := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", accounts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(combos[0]); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"max_tokens":16}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("expected success, got %d: %s", rec.Code, rec.Body.String())
	}
	if lastField != "max_completion_tokens" {
		t.Fatalf("member pin must win over account and provider settings, got field=%q", lastField)
	}
}

// TestLearnedTokenParamAvoidsSecondRoundTrip: after the FIRST request heals
// (no config set anywhere, pure auto-detection), the SECOND request to the
// same account must go straight to the correct field with only ONE upstream
// call — proving the learned cache actually eliminates the added latency the
// user asked about, not just the first request but every one after it too.
func TestLearnedTokenParamAvoidsSecondRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		if _, ok := b["max_tokens"]; ok {
			calls = append(calls, "max_tokens")
		} else {
			calls = append(calls, "max_completion_tokens")
		}
		mu.Unlock()
		if _, ok := b["max_tokens"]; ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(lightningAIWording))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "lightning", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	do := func() int {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"lightning","messages":[],"max_tokens":16}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
		return rec.Code
	}

	if code := do(); code != 200 {
		t.Fatalf("first request should succeed via reactive heal, got %d", code)
	}
	mu.Lock()
	firstRoundCalls := len(calls)
	mu.Unlock()
	if firstRoundCalls != 2 {
		t.Fatalf("first request should cost 2 upstream calls (fail + heal), got %d: %v", firstRoundCalls, calls)
	}

	if code := do(); code != 200 {
		t.Fatalf("second request should succeed, got %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	totalAfterSecond := len(calls)
	if totalAfterSecond-firstRoundCalls != 1 {
		t.Fatalf("second request should cost exactly 1 upstream call (learned cache applied proactively), got %d more calls: %v", totalAfterSecond-firstRoundCalls, calls)
	}
	if calls[len(calls)-1] != "max_completion_tokens" {
		t.Fatalf("second request's only call must already use the learned field, got %v", calls)
	}
}

// TestAutoHealDoesNotLoopWhenSwapNotApplicable: if the error message doesn't
// match, or the body already has both/neither field, no swap happens and the
// real upstream error (or lack of one) passes through untouched — no infinite
// retry, no silently swallowed unrelated error.
func TestAutoHealDoesNotLoopWhenSwapNotApplicable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "p", BaseURL: upstream.URL, AuthKey: "bad-key", Model: "m", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p","messages":[],"max_tokens":16}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unrelated 401 must pass through untouched, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid api key") {
		t.Fatalf("real error body must be preserved, got: %s", rec.Body.String())
	}
}
