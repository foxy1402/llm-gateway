package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

func newTestStack(t *testing.T, upstream *httptest.Server, provs []config.Provider, combos []config.Combo) (*Proxy, *store.Store, *registry.Registry) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, p := range provs {
		if err := st.UpsertProvider(p); err != nil {
			t.Fatalf("upsert provider: %v", err)
		}
	}
	for _, c := range combos {
		if err := st.UpsertCombo(c); err != nil {
			t.Fatalf("upsert combo: %v", err)
		}
	}
	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatalf("reload: %v", err)
	}
	px := New(reg, st, 5*time.Second)
	return px, st, reg
}

func TestRewriteModelSplice(t *testing.T) {
	cases := []struct{ in, model, want string }{
		{`{"model":"a","messages":[]}`, "b", `{"model":"b","messages":[]}`},
		{`{"model"  :  "a" , "x":1}`, "b", `{"model"  :  "b" , "x":1}`},
		{`{"prompt":"keep me","model":"a"}`, "b", `{"prompt":"keep me","model":"b"}`},
	}
	for _, c := range cases {
		out, err := rewriteModel([]byte(c.in), c.model)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if string(out) != c.want {
			t.Fatalf("in=%s got=%s want=%s", c.in, out, c.want)
		}
	}
	big := `{"model":"a","prompt":"` + strings.Repeat("x", 1<<20) + `"}`
	out, err := rewriteModel([]byte(big), "b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), strings.Repeat("x", 1<<20)) || !strings.Contains(string(out), `"model":"b"`) {
		t.Fatal("large prompt corrupted")
	}
}

func TestSingleProviderPassthrough(t *testing.T) {
	var sawModel, sawAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		var m map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&m)
		sawModel = string(m["model"])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","created":1,"model":"llama3","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer upstream.Close()

	px, st, _ := newTestStack(t, upstream, []config.Provider{
		{ID: "groq", BaseURL: upstream.URL, AuthKey: "upstream-key", Model: "llama3", Weight: 1, Enabled: true},
	}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"groq","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if sawModel != `"llama3"` {
		t.Fatalf("upstream saw model %s", sawModel)
	}
	if sawAuth != "Bearer upstream-key" {
		t.Fatalf("upstream saw auth %q", sawAuth)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		logs, _ := st.QueryLogs(config.LogFilter{Limit: 5})
		if len(logs) == 1 {
			if logs[0].Status != 200 {
				t.Fatalf("logged status: %d", logs[0].Status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("request log not written")
}

func TestPeekBody(t *testing.T) {
	px := &Proxy{maxBodyBytes: defaultMaxBodyBytes}
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":"m","stream":true}`))
	info, err := px.peekBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "m" || !info.Stream {
		t.Fatalf("peek: %+v", info)
	}
}

// TestPeekBodyRejectsOversizedBody: a body over the configured cap must fail
// cleanly (413 upstream in ServeHTTP) rather than silently truncating.
func TestPeekBodyRejectsOversizedBody(t *testing.T) {
	px := &Proxy{maxBodyBytes: 1024}
	big := `{"model":"m","x":"` + strings.Repeat("a", 2048) + `"}`
	req := httptest.NewRequest("POST", "/x", strings.NewReader(big))
	if _, err := px.peekBody(req); err == nil {
		t.Fatal("expected error for oversized body")
	}
}

// TestMaxBodyBytesConfigurable: SetMaxBodyBytes must raise the cap so a
// realistic base64-encoded image payload (bigger than the old 4MiB default)
// is accepted instead of rejected with 413.
func TestMaxBodyBytesConfigurable(t *testing.T) {
	px := &Proxy{maxBodyBytes: defaultMaxBodyBytes}
	px.SetMaxBodyBytes(10 << 20) // 10 MiB
	if px.maxBodyBytes != 10<<20 {
		t.Fatalf("maxBodyBytes: %d", px.maxBodyBytes)
	}
	// Values <= 0 must be ignored, not disable the guard.
	px.SetMaxBodyBytes(0)
	if px.maxBodyBytes != 10<<20 {
		t.Fatalf("SetMaxBodyBytes(0) must be a no-op, got %d", px.maxBodyBytes)
	}
	px.SetMaxBodyBytes(-5)
	if px.maxBodyBytes != 10<<20 {
		t.Fatalf("SetMaxBodyBytes(negative) must be a no-op, got %d", px.maxBodyBytes)
	}

	big := `{"model":"m","image":"` + strings.Repeat("a", 6<<20) + `"}` // ~6MiB body
	req := httptest.NewRequest("POST", "/x", strings.NewReader(big))
	if _, err := px.peekBody(req); err != nil {
		t.Fatalf("6MiB body should fit under the raised 10MiB cap: %v", err)
	}
}

// TestMaxAccountsPerProviderCapConfigurable: SetMaxAccountsPerProviderCap must
// raise/lower the ceiling used by accountBudget, and reject non-positive
// overrides (0/negative would either disable or invert the cap).
func TestMaxAccountsPerProviderCapConfigurable(t *testing.T) {
	px := &Proxy{maxAccountsPerProviderCap: defaultMaxAccountsPerProviderCap}
	px.SetMaxAccountsPerProviderCap(20)
	if px.maxAccountsPerProviderCap != 20 {
		t.Fatalf("maxAccountsPerProviderCap: %d", px.maxAccountsPerProviderCap)
	}
	px.SetMaxAccountsPerProviderCap(0)
	if px.maxAccountsPerProviderCap != 20 {
		t.Fatalf("SetMaxAccountsPerProviderCap(0) must be a no-op, got %d", px.maxAccountsPerProviderCap)
	}
	px.SetMaxAccountsPerProviderCap(-1)
	if px.maxAccountsPerProviderCap != 20 {
		t.Fatalf("SetMaxAccountsPerProviderCap(negative) must be a no-op, got %d", px.maxAccountsPerProviderCap)
	}
}

// TestAccountBudgetScalesWithPoolSize: the whole point of the fix — a
// provider's attempt budget must equal its enabled account count (not a flat
// constant), capped at maxAccountsPerProviderCap.
func TestAccountBudgetScalesWithPoolSize(t *testing.T) {
	px := &Proxy{maxAccountsPerProviderCap: 3}

	legacy := &config.Provider{ID: "legacy"} // no Accounts rows: single implicit key
	if got := px.accountBudget(legacy); got != 1 {
		t.Fatalf("legacy single-key provider budget = %d, want 1", got)
	}

	twoKeys := &config.Provider{ID: "p", Accounts: []config.Account{
		{ID: "p:a", Enabled: true}, {ID: "p:b", Enabled: true},
	}}
	if got := px.accountBudget(twoKeys); got != 2 {
		t.Fatalf("2-key provider budget = %d, want 2 (below cap)", got)
	}

	fiveKeysOneCap := &Proxy{maxAccountsPerProviderCap: defaultMaxAccountsPerProviderCap}
	fiveKeys := &config.Provider{ID: "p", Accounts: []config.Account{
		{ID: "p:a", Enabled: true}, {ID: "p:b", Enabled: true}, {ID: "p:c", Enabled: true},
		{ID: "p:d", Enabled: true}, {ID: "p:e", Enabled: true},
	}}
	if got := fiveKeysOneCap.accountBudget(fiveKeys); got != 5 {
		t.Fatalf("5-key provider budget = %d, want 5 (the old flat cap of 3 undershot this)", got)
	}

	manyKeys := &config.Provider{ID: "p"}
	for i := 0; i < 50; i++ {
		manyKeys.Accounts = append(manyKeys.Accounts, config.Account{ID: "p:k", Enabled: true})
	}
	if got := px.accountBudget(manyKeys); got != 3 {
		t.Fatalf("50-key provider budget = %d, want capped at 3", got)
	}

	disabledOnly := &config.Provider{ID: "p", Accounts: []config.Account{{ID: "p:a", Enabled: false}}}
	if got := px.accountBudget(disabledOnly); got != 1 {
		t.Fatalf("provider with 0 enabled accounts budget = %d, want 1 (lets the loop surface a proper error)", got)
	}
}

func TestUnknownModel(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	px, _, _ := newTestStack(t, upstream, nil, nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"nope","messages":[]}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- usage extraction (cached tokens) ---
//
// These back the dashboard's Cache/TPS columns and stat chips, so a swapped
// field or wrong JSON path here silently corrupts the numbers shown in the UI
// without failing any request — worth pinning down directly.

func TestExtractChatUsage(t *testing.T) {
	cases := []struct {
		name             string
		body             string
		wantPrompt, want int
		wantCached       *int
	}{
		{
			name:       "with cached tokens",
			body:       `{"usage":{"prompt_tokens":1300,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":1152}}}`,
			wantPrompt: 1300, want: 42, wantCached: intPtr(1152),
		},
		{
			name:       "no usage block",
			body:       `{"id":"x"}`,
			wantCached: nil,
		},
		{
			name:       "usage without cache details",
			body:       `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			wantPrompt: 10, want: 5, wantCached: nil,
		},
		{
			name:       "explicit zero cached tokens stays nil",
			body:       `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantPrompt: 10, want: 5, wantCached: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, comp, cached := extractChatUsage([]byte(c.body))
			if c.wantCached == nil {
				if cached != nil {
					t.Fatalf("cached: want nil, got %d", *cached)
				}
				if p == nil && comp == nil {
					return // no-usage case
				}
			}
			if p == nil || *p != c.wantPrompt {
				t.Fatalf("prompt: want %d, got %v", c.wantPrompt, p)
			}
			if comp == nil || *comp != c.want {
				t.Fatalf("completion: want %d, got %v", c.want, comp)
			}
			if c.wantCached != nil {
				if cached == nil || *cached != *c.wantCached {
					t.Fatalf("cached: want %d, got %v", *c.wantCached, cached)
				}
			}
		})
	}
}

func TestExtractResponsesUsage(t *testing.T) {
	body := `{"usage":{"input_tokens":1300,"output_tokens":42,"input_tokens_details":{"cached_tokens":1152}}}`
	p, comp, cached := extractResponsesUsage([]byte(body))
	if p == nil || *p != 1300 {
		t.Fatalf("input tokens: %v", p)
	}
	if comp == nil || *comp != 42 {
		t.Fatalf("output tokens: %v", comp)
	}
	if cached == nil || *cached != 1152 {
		t.Fatalf("cached tokens: %v", cached)
	}
}

func TestExtractChunkUsage(t *testing.T) {
	pt, ct, cached := extractChunkUsage([]byte(`{"usage":{"prompt_tokens":1300,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":1152}}}`))
	if pt == nil || *pt != 1300 || ct == nil || *ct != 42 {
		t.Fatalf("prompt/completion: pt=%v ct=%v", pt, ct)
	}
	if cached == nil || *cached != 1152 {
		t.Fatalf("cached: %v", cached)
	}
	// A chunk with no usage block at all (typical mid-stream delta) must report
	// all-nil rather than zero values, so the caller's "sticky" merge in
	// streamResponse doesn't clobber a cached value seen in an earlier chunk.
	if pt, ct, cached := extractChunkUsage([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`)); pt != nil || ct != nil || cached != nil {
		t.Fatalf("delta-only chunk must yield all nil, got pt=%v ct=%v cached=%v", pt, ct, cached)
	}
}

func intPtr(v int) *int { return &v }

// TestCachedTokensLoggedEndToEnd exercises the full non-streaming path: upstream
// reports prompt_tokens_details.cached_tokens, and the value must land in the
// persisted request_log row exactly as reported.
func TestCachedTokensLoggedEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","created":1,"model":"llama3","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1300,"completion_tokens":42,"total_tokens":1342,"prompt_tokens_details":{"cached_tokens":1152}}}`))
	}))
	defer upstream.Close()

	px, st, _ := newTestStack(t, upstream, []config.Provider{
		{ID: "groq", BaseURL: upstream.URL, AuthKey: "upstream-key", Model: "llama3", Weight: 1, Enabled: true},
	}, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"groq","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		logs, _ := st.QueryLogs(config.LogFilter{Limit: 5})
		if len(logs) == 1 {
			if logs[0].CachedTokens == nil || *logs[0].CachedTokens != 1152 {
				t.Fatalf("logged cached_tokens: %+v", logs[0].CachedTokens)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("request log not written")
}

// TestResponsesVisionEndToEnd: a /v1/responses request with an input_image
// part, routed to a provider that isn't ResponsesNative (so it goes through
// ResponsesToChatRequest), must actually deliver the image bytes to the
// upstream chat-completions call instead of silently dropping them.
func TestResponsesVisionEndToEnd(t *testing.T) {
	var sawBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","created":1,"model":"llava","choices":[{"message":{"role":"assistant","content":"A cat."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	px, _, _ := newTestStack(t, upstream, []config.Provider{
		{ID: "vision-provider", BaseURL: upstream.URL, AuthKey: "k", Model: "llava", Weight: 1, Enabled: true},
	}, nil) // ResponsesNative defaults to false → forces the translation path.

	reqBody := `{"model":"vision-provider","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"What is in this image?"},
			{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}
		]}
	]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointResponses)

	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(sawBody), "iVBORw0KGgo=") {
		t.Fatalf("upstream never received the image data; body=%s", sawBody)
	}
	if !strings.Contains(string(sawBody), `"image_url"`) {
		t.Fatalf("upstream body missing image_url content part; body=%s", sawBody)
	}
	var m map[string]any
	if err := json.Unmarshal(sawBody, &m); err != nil {
		t.Fatalf("upstream body not valid JSON: %v", err)
	}
	msgs := m["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want text+image parts forwarded upstream, got %v", content)
	}
}
