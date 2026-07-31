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
