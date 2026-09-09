package proxy

import (
	"context"
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

// When every key of a provider is exhausted on retryable statuses, the terminal
// log row must name each attempt's outcome — not just the bare last status.
func TestExhaustionLogCarriesAttemptJournal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "quota", http.StatusTooManyRequests)
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

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 passthrough after exhaustion; got %d: %s", rec.Code, rec.Body.String())
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ProviderID: "solo", ErrorsOnly: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if strings.Contains(row.Error, "all accounts exhausted, last status 429; attempts: solo: upstream returned 429") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the attempt journal in the exhaustion log row; got %+v", rows)
	}
}

// The opaque 502 "all upstreams failed" (zero HTTP responses across all
// attempts) must carry the per-attempt journal so the dashboard shows whether
// each upstream died dialing (endpoint/network) or was skipped (health state).
func TestAllFailed502LogCarriesAttemptJournal(t *testing.T) {
	dead1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead1.Close() // port released → connection refused
	dead2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead2.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: dead1.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertProvider(config.Provider{ID: "b", BaseURL: dead2.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}}, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502; got %d: %s", rec.Code, rec.Body.String())
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ErrorsOnly: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Status == http.StatusBadGateway && strings.Contains(row.Error, "all upstreams failed:") &&
			strings.Contains(row.Error, "a: transport error via direct:") &&
			strings.Contains(row.Error, "b: transport error via direct:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected per-attempt transport detail in the 502 log row; got %+v", rows)
	}
}

// A cooldown storm — every member provider-cooling before the first dispatch —
// breaks out of the rotation loop with zero per-attempt lines. The terminal row
// must still name the cause (cooling members) instead of a bare
// "all upstreams failed: " with nothing after it.
func TestCooldownStormLogNamesCoolingMembers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertProvider(config.Provider{ID: "b", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}}, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	defer px.WaitLogs()

	// Cool both providers at the provider level (as sustained transport failures
	// would), so plan.next has nothing eligible on the first iteration.
	reg.Health().RecordFailure("a", 0)
	reg.Health().RecordFailure("b", 0)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502; got %d: %s", rec.Code, rec.Body.String())
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ErrorsOnly: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Status == http.StatusBadGateway &&
			strings.Contains(row.Error, "all upstreams failed: no eligible member (2 cooling down (a, b))") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the cooling summary in the 502 log row; got %+v", rows)
	}
}

// A combo with no enabled members 502s with a log row (previously silent).
func TestEmptyCombo502IsLogged(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	// The member's provider exists but is disabled, so the member insert passes
	// the provider_id FK while newRotationPlan filters it out → empty plan.
	st.UpsertProvider(config.Provider{ID: "off", BaseURL: "http://127.0.0.1:1", AuthKey: "k", Model: "m", Weight: 1, Enabled: false})
	st.UpsertCombo(config.Combo{ID: "empty", Rotation: config.Priority, Members: []config.ComboMember{{ProviderID: "off"}}, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"empty","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502; got %d: %s", rec.Code, rec.Body.String())
	}
	px.WaitLogs() // the log write is async — drain it before querying
	rows, err := st.QueryLogs(config.LogFilter{ErrorsOnly: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Status == http.StatusBadGateway &&
			strings.Contains(row.Error, "all upstreams failed: combo has no enabled members") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the empty-combo 502 log row; got %+v", rows)
	}
}
