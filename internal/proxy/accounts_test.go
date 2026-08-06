package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

// TestAlternatingKeysAcrossRequests: one provider with two accounts; successive
// requests should cycle keys round-robin, proven by the Authorization header the
// upstream sees.
func TestAlternatingKeysAcrossRequests(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"x","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "p:a1", ProviderID: "p", Label: "one", AuthKey: "key-1", Enabled: true, Weight: 1},
			{ID: "p:a2", ProviderID: "p", Label: "two", AuthKey: "key-2", Enabled: true, Weight: 1},
		},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	// Reload so the registry picks up the account pool.
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"p","messages":[],"stream":false}`
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("expected 4 upstream calls, got %d", len(seen))
	}
	first, second := seen[0], seen[1]
	if first == second {
		t.Fatalf("expected alternating keys, got same twice: %v", seen)
	}
	if seen[2] != first || seen[3] != second {
		t.Fatalf("keys not round-robin: %v", seen)
	}
}

// TestMemberModelHonored: a combo member pins model "member-model" — the upstream
// request body must carry that exact model, not the provider default.
func TestMemberModelHonored(t *testing.T) {
	var gotModel string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		gotModel = b.Model
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{
		{ID: "p", BaseURL: upstream.URL, AuthKey: "k", Model: "provider-default", Weight: 1, Enabled: true},
	}
	combos := []config.Combo{{
		ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p", Model: "member-model"}},
	}}
	px, _, _ := newTestStack(t, upstream, provs, combos)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotModel != "member-model" {
		t.Fatalf("upstream got model %q, want member-model", gotModel)
	}
}

// TestPerKeyModelRotation: one aggregator-style endpoint with two keys, each
// pinned to a different upstream model. Successive requests must rotate BOTH
// credentials and model: key1+kimi, key2+qwen, key1+kimi, ...
func TestPerKeyModelRotation(t *testing.T) {
	type seenCall struct{ auth, model string }
	var mu sync.Mutex
	var calls []seenCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		calls = append(calls, seenCall{r.Header.Get("Authorization"), b.Model})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "vercel", BaseURL: upstream.URL, Model: "provider-default", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "vercel:k1", ProviderID: "vercel", Label: "k1", AuthKey: "key-1", Model: "moonshotai/kimi-k3", Enabled: true, Weight: 1},
			{ID: "vercel:k2", ProviderID: "vercel", Label: "k2", AuthKey: "key-2", Model: "qwen/qwen3", Enabled: true, Weight: 1},
		},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("vercel", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"vercel","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("expected 4 upstream calls, got %d", len(calls))
	}
	// Binding must be exact — a key must never serve a different model — and the
	// two bindings must strictly alternate. The rotation's starting phase is
	// arbitrary, so assert pairing + alternation rather than a fixed order.
	bindings := map[string]string{"Bearer key-1": "moonshotai/kimi-k3", "Bearer key-2": "qwen/qwen3"}
	for i, c := range calls {
		if bindings[c.auth] != c.model {
			t.Fatalf("call %d: key %q served model %q (wrong binding)", i, c.auth, c.model)
		}
		if i > 0 && c.auth == calls[i-1].auth {
			t.Fatalf("calls %d and %d used the same key %q (no alternation)", i-1, i, c.auth)
		}
	}
	// Both keys must have been exercised.
	for k := range bindings {
		found := false
		for _, c := range calls {
			if c.auth == k {
				found = true
			}
		}
		if !found {
			t.Fatalf("key %q never used across %d calls", k, len(calls))
		}
	}
}

// TestMemberModelBeatsAccountModel: when a combo member pins model X and the
// serving key has its own model pin Y, X wins — the combo member is the most
// specific deliberately-configured layer ("vercel key2 → gpt-oss"). The account
// pin applies only wherever no combo member model overrides it (direct provider
// calls; members without a model).
func TestMemberModelBeatsAccountModel(t *testing.T) {
	var mu sync.Mutex
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		gotModel = b.Model
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "v", BaseURL: upstream.URL, Model: "provider-default", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "v:k", ProviderID: "v", Label: "k", AuthKey: "key", Model: "key-pinned-model", Enabled: true, Weight: 1},
		},
	}}
	combos := []config.Combo{{
		ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "v", Model: "member-model"}},
	}}
	px, st, _ := newTestStack(t, upstream, provs, combos)
	if err := st.ReplaceAccounts("v", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotModel != "member-model" {
		t.Fatalf("member model should win, got %q", gotModel)
	}
}

// TestPinnedMemberUsesExactKeyAndModel: the user's target flow — a combo routes
// across *specific* (provider, key, model) triples rather than providers.
// Combo "c" = [vercel key2 → gpt-oss, nvidia key3 → kimi-k3]. Successive requests
// must alternate those exact key+model pairs, never rotating to other keys.
func TestPinnedMemberUsesExactKeyAndModel(t *testing.T) {
	type seenCall struct{ upstream, auth, model string }
	var mu sync.Mutex
	var calls []seenCall

	mkUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			calls = append(calls, seenCall{name, r.Header.Get("Authorization"), b.Model})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
		}))
	}
	vercel := mkUpstream("vercel")
	defer vercel.Close()
	nvidia := mkUpstream("nvidia")
	defer nvidia.Close()

	provs := []config.Provider{
		{ID: "vercel", BaseURL: vercel.URL, Model: "vercel-default", Weight: 1, Enabled: true},
		{ID: "nvidia", BaseURL: nvidia.URL, Model: "nvidia-default", Weight: 1, Enabled: true},
	}
	// Accounts exist only in the DB (UpsertProvider doesn't touch the account
	// tables); the combo pins key2 of vercel and key3 of nvidia by ID.
	vercelAccounts := []config.Account{
		{ID: "vercel:k1", ProviderID: "vercel", Label: "key1", AuthKey: "v-key-1", Enabled: true, Weight: 1},
		{ID: "vercel:k2", ProviderID: "vercel", Label: "key2", AuthKey: "v-key-2", Enabled: true, Weight: 1},
	}
	nvidiaAccounts := []config.Account{
		{ID: "nvidia:k1", ProviderID: "nvidia", Label: "key1", AuthKey: "n-key-1", Enabled: true, Weight: 1},
		{ID: "nvidia:k2", ProviderID: "nvidia", Label: "key2", AuthKey: "n-key-2", Enabled: true, Weight: 1},
		{ID: "nvidia:k3", ProviderID: "nvidia", Label: "key3", AuthKey: "n-key-3", Enabled: true, Weight: 1},
	}
	// IMPORTANT: accounts must exist before the combo references them — the
	// combo_members.account_id FK is enforced by SQLite.
	px, st, _ := newTestStack(t, vercel, provs, nil)
	if err := st.ReplaceAccounts("vercel", vercelAccounts); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAccounts("nvidia", nvidiaAccounts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "test", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{
			{ProviderID: "vercel", AccountID: "vercel:k2", Model: "openai/gpt-oss-120b"},
			{ProviderID: "nvidia", AccountID: "nvidia:k3", Model: "moonshotai/kimi-k3"},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 upstream calls (pinned = no probing), got %d: %+v", len(calls), calls)
	}
	// Rotation start phase is an implementation detail; what matters is each call
	// hit one exact (provider, key, model) triple and the two calls differ.
	want := map[seenCall]bool{
		{"vercel", "Bearer v-key-2", "openai/gpt-oss-120b"}: true,
		{"nvidia", "Bearer n-key-3", "moonshotai/kimi-k3"}:  true,
	}
	if calls[0] == calls[1] {
		t.Fatalf("both calls hit the same member %v (combo rotation broken)", calls[0])
	}
	for _, c := range calls {
		if !want[c] {
			t.Fatalf("unexpected call (upstream=%s auth=%s model=%s)", c.upstream, c.auth, c.model)
		}
	}
}

// TestPinnedSiblingFallthrough: two members of the SAME provider pinned to
// different keys. When the first key 429s, the rotation must reach the sibling
// member pinned to the second key — burning a member must not burn the provider.
// Exercises both failure branches (status-retry burn and, on the next request,
// PinnedAccount denial thanks to the leftover account cooldown).
func TestPinnedSiblingFallthrough(t *testing.T) {
	var mu sync.Mutex
	var calls []string // auth headers seen, in arrival order

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		calls = append(calls, auth)
		mu.Unlock()
		if auth == "Bearer k2" {
			w.WriteHeader(http.StatusTooManyRequests) // the priority-first pinned key is quota-dead
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "acme", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("acme", []config.Account{
		{ID: "acme:k2", ProviderID: "acme", Label: "dead", AuthKey: "k2", Enabled: true, Weight: 1},
		{ID: "acme:k3", ProviderID: "acme", Label: "live", AuthKey: "k3", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{
			{ProviderID: "acme", AccountID: "acme:k2", Model: "gpt-oss"},
			{ProviderID: "acme", AccountID: "acme:k3", Model: "kimi"},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	// Request 1: k2 burns with a 429 — the SAME request must survive via k3.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("request 1 should rotate k2→k3, got status %d: %s", rec.Code, rec.Body.String())
	}

	// Request 2: k2 is still in cooldown, so PinnedAccount denies it outright —
	// member must burn, sibling must still serve.
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec = httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("request 2 should skip the cooled member, got status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	// Expected: [k2 burn (request 1 attempt), k3 serve (request 1), k3 serve
	// (request 2)]. Request 2 never touches k2 — its own cooldown denies it.
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls (k2 burn + two k3 serves), got %d: %v", len(calls), calls)
	}
	if calls[0] != "Bearer k2" {
		t.Fatalf("call 0 should be the k2 burn attempt, got %s", calls[0])
	}
	for i := 1; i < len(calls); i++ {
		if calls[i] != "Bearer k3" {
			t.Fatalf("call %d used %s; after the k2 burn every served call must use k3", i, calls[i])
		}
	}
}

// TestModelAliasesRouteUnknownNames emulates agents that send their provider's
// name as the model (observed with ironclaw/openclaw): with the alias installed
// the request routes through; without it, a 404 keeps exact names authoritative.
func TestModelAliasesRouteUnknownNames(t *testing.T) {
	var mu sync.Mutex
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		gotModel = b.Model
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "vercel", BaseURL: upstream.URL, Model: "gpt-upstream", Weight: 1, Enabled: true,
		Accounts: []config.Account{{ID: "vercel:k", ProviderID: "vercel", AuthKey: "key", Enabled: true, Weight: 1}}}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}
	px.SetModelAliases(map[string]string{"ironclaw": "vercel"})

	call := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"`+model+`","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		return rec
	}

	if rec := call("ironclaw"); rec.Code != 200 {
		t.Fatalf("aliased model must route, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if gotModel != "gpt-upstream" {
		t.Fatalf("upstream should receive the provider's model, got %q", gotModel)
	}
	mu.Unlock()

	if rec := call("unaliased-name"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown names without an alias must stay 404, got %d", rec.Code)
	}
	if rec := call("vercel"); rec.Code != 200 {
		t.Fatalf("exact provider names must keep working, got %d", rec.Code)
	}
}

// TestModelAliasCyclesDeterministic: chains resolve until a concrete provider
// (b-alias → a → b works), and alias-only loops/self-references 404 cleanly
// instead of spinning.
func TestModelAliasCyclesDeterministic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()
	provs := []config.Provider{{ID: "b", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{{ID: "b:k", ProviderID: "b", AuthKey: "key", Enabled: true, Weight: 1}}}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}
	px.SetModelAliases(map[string]string{
		"a":       "b",
		"b-alias": "a", // chain: lands on real provider b
		"x":       "y",
		"y":       "x",    // pure alias loop, no concrete target
		"loop":    "loop", // self-reference
	})

	call := func(model string) int {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"`+model+`","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		return rec.Code
	}
	if code := call("a"); code != 200 {
		t.Fatalf("a should resolve to provider b, got %d", code)
	}
	if code := call("b-alias"); code != 200 {
		t.Fatalf("b-alias should chain to provider b, got %d", code)
	}
	if code := call("x"); code != http.StatusNotFound {
		t.Fatalf("alias-only loop must 404, got %d", code)
	}
	if code := call("loop"); code != http.StatusNotFound {
		t.Fatalf("self-alias must 404, got %d", code)
	}
}

// TestAccountCooldownFallsToSibling: key-1 starts returning 429; the SAME request
// must immediately retry key-2 and succeed, while key-1 enters cooldown.
func TestAccountCooldownFallsToSibling(t *testing.T) {
	var mu sync.Mutex
	burned := map[string]bool{"key-1": true}
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		badf := burned[strings.TrimPrefix(auth, "Bearer ")]
		mu.Unlock()
		if badf {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"x","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "p:a1", ProviderID: "p", Label: "one", AuthKey: "key-1", Enabled: true, Weight: 1},
			{ID: "p:a2", ProviderID: "p", Label: "two", AuthKey: "key-2", Enabled: true, Weight: 1},
		},
	}}
	combos := []config.Combo{{
		ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p"}},
	}}
	px, st, reg := newTestStack(t, upstream, provs, combos)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	reg.Health().Configure(1, []int{429}) // 1s cooldown
	if err := reg.Reload(st); err != nil {
		t.Fatal(err)
	}

	// Two requests: each starts a fresh rotation, so each will hit key-1 → 429 →
	// key-2 → 200. Both must ultimately succeed.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	sawKey1, sawKey2 := false, false
	for _, s := range seen {
		if s == "Bearer key-1" {
			sawKey1 = true
		}
		if s == "Bearer key-2" {
			sawKey2 = true
		}
	}
	if !sawKey1 || !sawKey2 {
		t.Fatalf("expected both keys exercised (fallback), got %v", seen)
	}
	// key-1 should now be cooled.
	if reg.Health().IsAccountAvailable("p", "p:a1") {
		t.Fatal("key-1 should be in cooldown after repeated 429s")
	}
	if !reg.Health().IsAccountAvailable("p", "p:a2") {
		t.Fatal("key-2 should stay available")
	}
	time.Sleep(0) // allow cooldown goroutines to settle
}

// TestProviderNameModelWithToolsAndStreaming replicates the ironclaw request
// pattern: client addresses the gateway provider by name ("vercel") with a
// tools payload + streaming, while the provider rewrites to a slug upstream
// model. The gateway must route this, not 404 — if ironclaw gets 404s, the
// request it actually sends differs from what this test sends.
func TestProviderNameModelWithToolsAndStreaming(t *testing.T) {
	var mu sync.Mutex
	var gotModel string
	var gotTools bool
	var gotRaw []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]json.RawMessage
		_ = json.Unmarshal(raw, &b)
		var model string
		_ = json.Unmarshal(b["model"], &model)
		mu.Lock()
		gotModel = model
		_, gotTools = b["tools"]
		gotRaw = raw
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "vercel", BaseURL: upstream.URL,
		Model: "meta/muse-spark-1.2-contributor", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "vercel:k1", ProviderID: "vercel", AuthKey: "k1", Enabled: true, Weight: 1},
			{ID: "vercel:k2", ProviderID: "vercel", AuthKey: "k2", Enabled: true, Weight: 1},
			{ID: "vercel:k3", ProviderID: "vercel", AuthKey: "k3", Enabled: true, Weight: 1},
			{ID: "vercel:k4", ProviderID: "vercel", AuthKey: "k4", Enabled: true, Weight: 1},
		}}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"vercel","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"builtin__echo","parameters":{}}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("provider-name model with tools+stream must route (not 404), got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotModel != "meta/muse-spark-1.2-contributor" {
		t.Fatalf("upstream must receive the rewritten slug, got model=%q body=%s", gotModel, gotRaw)
	}
	if !gotTools {
		t.Fatal("tools array must be forwarded to the upstream untouched")
	}
}
