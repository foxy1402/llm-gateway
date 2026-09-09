package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

// signalWriter wraps a ResponseWriter and closes first on the first Write, so
// the test can prove the gateway reached the streaming loop before canceling —
// a fixed sleep can't distinguish "stream committed" from "gateway stalled
// before dispatch", and canceling too early takes the dispatch-abort path
// (no log row) instead of the mid-stream path under test.
type signalWriter struct {
	http.ResponseWriter
	first chan struct{}
	once  sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	return w.ResponseWriter.Write(p)
}

// A client abort mid-stream (Esc in an IDE) must NOT put the account into
// cooldown: the body read fails with context.Canceled even though the upstream
// is perfectly healthy. Poisoning one healthy key per abort used to cascade —
// enough aborted streams cooled down an entire combo into "all upstreams
// failed" 502s.
func TestClientAbortDoesNotPoisonAccount(t *testing.T) {
	sent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(sent) // first chunk is on the wire
		<-r.Context().Done()
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	defer px.WaitLogs() // drain async log writes before the deferred st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	// Prove the gateway forwarded bytes downstream (not just that the upstream
	// wrote a chunk) before simulating the user hitting Esc. The upstream never
	// sends [DONE]/EOF, so the stream cannot finish pre-cancel and vacate the
	// path under test.
	sw := &signalWriter{ResponseWriter: rec, first: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		px.ServeHTTP(sw, req, registry.EndpointChatCompletions)
	}()
	select {
	case <-sw.first:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("gateway never forwarded a stream chunk")
	}
	cancel()
	<-done

	if !reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("client abort must not put the account into cooldown")
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ProviderID: "solo", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Status == 200 && row.ProviderUsed == "solo" &&
			strings.Contains(row.Error, "client disconnected mid-stream") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a client-disconnect log row; got %+v", rows)
	}
}

// The guard must not over-suppress: a stream that dies because the UPSTREAM
// aborted (connection reset after a 200) still books the account failure.
func TestGenuineStreamFailureStillPenalizes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // kill the connection mid-stream
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if reg.Health().IsAccountAvailable("solo", "solo:default") {
		t.Fatal("upstream dying mid-stream must put the account into cooldown")
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ProviderID: "solo", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if strings.Contains(row.Error, "upstream stream read error") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the stream error in the log row; got %+v", rows)
	}
}

// sentinelBody serves its bytes then blocks on Read until Close releases it —
// an upstream that keeps the connection open after [DONE]. Close is recorded
// so the test can verify the body is released, and it unblocks Read so neither
// the production reader goroutine nor the test leaks (a bare hour-long sleep
// would linger in the test binary for the whole run).
type sentinelBody struct {
	data   []byte
	pos    int
	closed bool
	done   chan struct{}
	once   sync.Once
}

func (b *sentinelBody) Read(p []byte) (int, error) {
	if b.pos < len(b.data) {
		n := copy(p, b.data[b.pos:])
		b.pos += n
		return n, nil
	}
	// Hold the connection open, never EOF — until Close. done is created with
	// the body (never lazily), so there is no init-vs-Close data race.
	<-b.done
	return 0, io.EOF
}

func (b *sentinelBody) Close() error {
	b.closed = true
	b.once.Do(func() { close(b.done) })
	return nil
}

// [DONE] is the protocol-level end of stream: streamResponse must return as
// soon as the sentinel is forwarded, NOT keep waiting for EOF — upstreams that
// hold the connection open used to trip the 90s stall timer, which then marked
// the (healthy) account failed.
func TestStreamStopsAtDoneSentinel(t *testing.T) {
	body := &sentinelBody{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"), done: make(chan struct{})}
	upstream := &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       body,
	}
	rec := httptest.NewRecorder()
	p := &Proxy{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, _, _, err := p.streamResponse(rec, upstream, StreamFormatChat, false, ctx)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("streamResponse kept reading after [DONE] for %s", elapsed)
	}
	if err != nil {
		t.Fatalf("clean [DONE] stream returned an error: %v", err)
	}
	if !body.closed {
		t.Fatal("upstream body was not closed")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("sentinel not forwarded; got:\n%s", rec.Body.String())
	}
}
