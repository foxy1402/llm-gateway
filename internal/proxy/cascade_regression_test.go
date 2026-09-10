package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llm-gateway/internal/config"
)

// An UNPINNED combo member (no account_id) whose provider has several accounts
// used to burn only the account when it failed, leaving the member eligible
// forever: the attempt loop kept re-selecting the same member, exhausted its
// budget on one dead provider, and returned 502 "all upstreams failed" even
// though a perfectly healthy second member was sitting right there. The whole
// request must still succeed on the sibling member.
func TestUnpinnedMemberFailureFallsThroughToSibling(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		// Every key on the "dead" provider 500s; the "live" provider always serves.
		if strings.HasPrefix(auth, "Bearer dead") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{
		{ID: "dead", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true},
		{ID: "live", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true},
	}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("dead", []config.Account{
		{ID: "dead:a", ProviderID: "dead", Label: "a", AuthKey: "dead-a", Enabled: true, Weight: 1},
		{ID: "dead:b", ProviderID: "dead", Label: "b", AuthKey: "dead-b", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAccounts("live", []config.Account{
		{ID: "live:a", ProviderID: "live", Label: "a", AuthKey: "live-a", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Both members UNPINNED (AccountID empty) — this is the shape that broke.
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{
			{ProviderID: "dead"},
			{ProviderID: "live"},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("an unpinned dead member must fall through to the live sibling, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[len(seen)-1] != "Bearer live-a" {
		t.Fatalf("expected the request to be served by the live member, calls: %v", seen)
	}
}

// A client that hangs up while the non-streaming response body is being read is
// the client's problem, not the upstream's. Recording an account failure there
// let a burst of user cancellations cool down every healthy account in the pool
// — the mechanism behind the production 502 cascade.
func TestNonStreamClientAbortDoesNotPenalizeAccount(t *testing.T) {
	// Upstream advertises more bytes than it sends, then closes: the body read
	// fails partway through, exactly like a client-side abort mid-copy.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hold until the client (the gateway) goes away
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{{ID: "p:a", ProviderID: "p", Label: "a", AuthKey: "k", Enabled: true, Weight: 1}},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p","messages":[],"stream":false}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		px.ServeHTTP(rec, req, "chat.completions")
	}()
	cancel()
	<-done

	// The account must remain usable: a client abort is not upstream misbehavior.
	if !px.registry.Health().IsAccountAvailable("p", "p:a") {
		t.Fatal("client abort cooled down the account — this is what cascaded into 502s in production")
	}
}

// The heal ladder hands the caller back a response whose Body it must not leak.
// tryResponsesRerouteHeal supersedes the original chat rejection with an
// in-memory response, so the original upstream body has to be closed or one
// socket leaks per rerouted request (and this heal caches nothing, so every tool
// call pays it).
func TestResponsesRerouteClosesSupersededBody(t *testing.T) {
	var closed atomic.Bool
	original := &http.Response{
		StatusCode: 400,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &trackingBody{Reader: strings.NewReader(`{"error":{"message":"tool use is not supported on this endpoint; use /v1/responses instead"}}`), closed: &closed},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"resp_1","created_at":1,"model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{{ID: "p:a", ProviderID: "p", Label: "a", AuthKey: "k", Enabled: true, Weight: 1}},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"p","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"stream":false}`)
	healed, _, _, ok := px.tryResponsesRerouteHeal(context.Background(), http.DefaultClient, original, upstream.URL, "k", body, "p", "p:a", false, "p")
	if !ok || healed == nil {
		t.Fatal("reroute heal did not fire on a tool-call rejection")
	}
	if !closed.Load() {
		t.Fatal("the superseded chat-completions response body was never closed — one leaked socket per rerouted request")
	}
}

// trackingBody records whether Close was called.
type trackingBody struct {
	*strings.Reader
	closed *atomic.Bool
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}
