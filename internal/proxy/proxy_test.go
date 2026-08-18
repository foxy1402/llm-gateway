package proxy

import (
	"context"
	"encoding/json"
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
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"model":"m","stream":true}`))
	info, err := peekBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "m" || !info.Stream {
		t.Fatalf("peek: %+v", info)
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
