package proxy

import (
	"context"
	"errors"
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

// flushHidingWriter mimics the middleware wrappers (request logger's statusWriter,
// fail-ban's statusCapture): it does NOT implement http.Flusher itself, but exposes
// the underlying writer via Unwrap. A direct w.(http.Flusher) assertion returns nil
// through this wrapper; only http.ResponseController can traverse the chain.
type flushHidingWriter struct {
	http.ResponseWriter
}

func (w *flushHidingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SSE must flush through middleware wrappers that hide the Flusher interface —
// otherwise tokens buffer upstream and arrive in multi-KB bursts.
func TestStreamFlushesThroughWrapper(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\ndata: [DONE]\n\n"))
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
	px.ServeHTTP(&flushHidingWriter{rec}, req, registry.EndpointChatCompletions)

	if !rec.Flushed {
		t.Fatal("stream was never flushed through the middleware wrapper")
	}
	if out := rec.Body.String(); !strings.Contains(out, `"content":"tok"`) {
		t.Fatalf("missing streamed content; got:\n%s", out)
	}
}

// errBody fails on the first read, simulating an upstream that 200s then dies.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errBoom }
func (errBody) Close() error             { return nil }

var errBoom = errors.New("connection reset")

func TestStreamReadErrorReported(t *testing.T) {
	upstream := &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       errBody{},
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}
	if _, _, _, err := p.streamResponse(rec, upstream, StreamFormatChat, false); err == nil {
		t.Fatal("expected a mid-stream read error to be reported")
	}
}
