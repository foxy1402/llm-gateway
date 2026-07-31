package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
	"context"
)

func TestStreamingEarlyErrorRotates(t *testing.T) {
	// Bad upstream: 429 immediately.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer bad.Close()
	// Good upstream: streams a short completion.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer good.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "bad", BaseURL: bad.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertProvider(config.Provider{ID: "good", BaseURL: good.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Members: []string{"bad", "good"}, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	out := rec.Body.String()
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("stream should have rotated to good upstream; got:\n%s", out)
	}
}

// #1: A streaming response must not be killed by the short header timeout.
func TestStreamOutlivesHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(250 * time.Millisecond) // exceeds the 150ms header timeout
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 150*time.Millisecond)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	out := rec.Body.String()
	if !strings.Contains(out, `"content":"a"`) || !strings.Contains(out, `"content":"b"`) {
		t.Fatalf("stream was killed by header timeout; got:\n%s", out)
	}
}

// After the stream commits (200 + first byte), later upstream errors are passed through.
func TestStreamingMidStreamErrorPassesThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		w.Write([]byte("data: {\"error\":{\"message\":\"upstream blew up\"}}\n\n"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	out := rec.Body.String()
	if !strings.Contains(out, "upstream blew up") {
		t.Fatalf("mid-stream error should pass through; got:\n%s", out)
	}
}
