package proxy

import (
	"context"
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

// ValidateProxyURL is the save-time guard: a typo'd entry would otherwise sit
// in the pool looking healthy and only surface as a failed dial mid-request.
func TestValidateProxyURL(t *testing.T) {
	for _, good := range []string{
		"http://user:pass@1.2.3.4:8080",
		"https://proxy.example.com:3128",
		"socks5://user:pass@1.2.3.4:1080",
		"socks5h://user:pass@1.2.3.4:1080",
	} {
		if err := ValidateProxyURL(good); err != nil {
			t.Errorf("valid URL %q rejected: %v", good, err)
		}
	}
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"bad scheme", "ftp://host:21"},
		{"no scheme", "1.2.3.4:1080"},
		{"missing host", "socks5://:1080"},
		{"missing port", "socks5://user:pass@1.2.3.4"},
	}
	for _, c := range cases {
		if err := ValidateProxyURL(c.raw); err == nil {
			t.Errorf("%s (%q): expected rejection, got nil", c.name, c.raw)
		}
	}
}

// egressFor must not route anything through the pool unless the provider or
// the combo explicitly opted in — the extra hop is pure downside otherwise.
func TestEgressForOptInOnly(t *testing.T) {
	upstream := httptest.NewServer(nil)
	defer upstream.Close()
	provs := []config.Provider{{ID: "p", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true}}
	px, _, _ := newTestStack(t, upstream, provs, nil)

	plain := &config.Provider{ID: "p"}
	got := px.egressFor(plain, nil)
	if got.client != px.client || got.label != "" || got.proxyID != "" {
		t.Fatal("opted-out provider must dial out directly")
	}

	// Opted in, but nothing in the pool: direct fallback (the request still
	// gets served, just from the gateway's own IP).
	optIn := &config.Provider{ID: "p", ProxyRotate: true}
	got = px.egressFor(optIn, nil)
	if got.client != px.client || got.label != "" || got.proxyID != "" {
		t.Fatal("empty pool must fall back to direct dial")
	}

	// Opt in via the combo flag alone — the member provider's own setting
	// doesn't matter then.
	combo := &config.Combo{ID: "c", ProxyRotate: true}
	_ = px.egressFor(optIn, combo) // pool assertions live in TestNextProxyRotation
}

// NextProxy must cycle through enabled entries, skipping disabled ones, and
// report !ok when there is nothing usable.
func TestNextProxyRotation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := registry.New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.NextProxy(); ok {
		t.Fatal("empty pool must report ok=false")
	}

	for _, e := range []config.ProxyEntry{
		{ID: "px-1", Label: "one", URL: "http://1.2.3.4:8080", Enabled: true, Position: 0},
		{ID: "px-2", Label: "two", URL: "http://5.6.7.8:8080", Enabled: false, Position: 1},
		{ID: "px-3", Label: "three", URL: "socks5://user:pass@9.9.9.9:1080", Enabled: true, Position: 2},
	} {
		if err := st.UpsertProxy(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	// Disabled entry never surfaces; enabled ones alternate.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		e, ok := r.NextProxy()
		if !ok {
			t.Fatal("pool with enabled entries must report ok=true")
		}
		seen[e.ID]++
		if e.ID == "px-2" {
			t.Fatal("disabled proxy must never be handed out")
		}
	}
	if seen["px-1"] != 2 || seen["px-3"] != 2 {
		t.Fatalf("expected even rotation across px-1/px-3, got %v", seen)
	}
}

// The pool must survive an export/import round trip — proxies were previously
// missing from ExportSQL's column/table lists, silently resetting pool +
// proxy_rotate flags on every restore.
func TestProxyPoolExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertProvider(config.Provider{ID: "p", BaseURL: "https://x", AuthKey: "k", Model: "m", Weight: 1, Enabled: true, ProxyRotate: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProxy(config.ProxyEntry{ID: "px-1", Label: "one", URL: "socks5://user:pass@1.2.3.4:1080", Enabled: true, Position: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.RoundRobin, Members: []config.ComboMember{{ProviderID: "p"}}, Enabled: true, ProxyRotate: true}); err != nil {
		t.Fatal(err)
	}

	dump, err := st.ExportSQL()
	if err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(context.Background(), filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if err := st2.ImportSQL(dump); err != nil {
		t.Fatal(err)
	}
	proxies, err := st2.ListProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0].URL != "socks5://user:pass@1.2.3.4:1080" {
		t.Fatalf("proxy pool lost on export/import: %+v", proxies)
	}
	p, err := st2.GetProvider("p")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || !p.ProxyRotate {
		t.Fatalf("provider proxy_rotate lost on export/import: %+v", p)
	}
	c, err := st2.GetCombo("c")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || !c.ProxyRotate {
		t.Fatalf("combo proxy_rotate lost on export/import: %+v", c)
	}
}

// TestEgressProxyEndToEnd: a provider with ProxyRotate on must actually dial
// the upstream through a CONNECT tunnel — a local HTTP proxy that records the
// raw target proves the request leaves via the pool, not directly.
func TestEgressProxyEndToEnd(t *testing.T) {
	var directHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHost = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-px","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	var gotConnect string
	fwd := &http.Transport{DisableCompression: true}
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.Transport speaks plain-HTTP forward-proxying (absolute URI),
		// not CONNECT, for http:// targets.
		if r.Method == http.MethodConnect {
			gotConnect = r.Host
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL == nil || r.URL.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotConnect = r.URL.Host
		out, err := fwd.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer out.Body.Close()
		w.WriteHeader(out.StatusCode)
		_, _ = io.Copy(w, out.Body)
	}))
	defer tunnel.Close()

	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "px.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertProvider(config.Provider{ID: "p", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true, ProxyRotate: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProxy(config.ProxyEntry{ID: "px-1", Label: "local", URL: tunnel.URL, Enabled: true, Position: 0}); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatal(err)
	}
	px := New(reg, st, 5*time.Second)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if directHost != "" && gotConnect == "" {
		t.Fatal("request went direct to upstream — proxied attempt never hit the tunnel")
	}
	if gotConnect == "" {
		t.Fatalf("proxy tunnel saw no request (direct=%q proxied=%q)", directHost, gotConnect)
	}
}

// TestProxyPassiveHealthSkipsCooling: a transport failure through one entry
// cools it down so rotation skips it, while a sibling keeps serving — and an
// HTTP status (even 429) must NOT cool the proxy, since the proxy demonstrably
// forwarded the request.
func TestProxyPassiveHealthSkipsCooling(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, e := range []config.ProxyEntry{
		{ID: "px-1", URL: "http://1.2.3.4:8080", Enabled: true, Position: 0},
		{ID: "px-2", URL: "http://5.6.7.8:8080", Enabled: true, Position: 1},
	} {
		if err := st.UpsertProxy(e); err != nil {
			t.Fatal(err)
		}
	}
	r := registry.New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	r.Health().Configure(60, []int{429})

	// Transport failure through px-1: rotation must skip it until cooldown ends.
	r.Health().RecordProxyFailure("px-1")
	for i := 0; i < 4; i++ {
		got, ok := r.NextProxy()
		if !ok {
			t.Fatal("sibling px-2 must keep serving while px-1 cools down")
		}
		if got.ID != "px-2" {
			t.Fatalf("cooling proxy must be skipped, got %q", got.ID)
		}
	}

	// Unknown (never-tried) proxies are eligible — no data means no verdict.
	if !r.Health().IsProxyAvailable("px-never-seen") {
		t.Fatal("unknown proxy must be eligible")
	}
	if _, _, _, seen := r.Health().ProxyStatus("px-never-seen"); seen {
		t.Fatal("unknown proxy must report seen=false for the dashboard")
	}

	// Success clears the cooldown: px-1 rejoins rotation.
	r.Health().RecordProxySuccess("px-1")
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got, ok := r.NextProxy()
		if !ok {
			t.Fatal("pool should serve again after success clears cooldown")
		}
		seen[got.ID] = true
	}
	if !seen["px-1"] || !seen["px-2"] {
		t.Fatalf("both proxies should rotate again after recovery, got %v", seen)
	}
}

// TestProxyHealthPrunedOnReload: deleting a pool entry must drop its health
// state too, so the states map can't grow without bound across dashboard churn.
func TestProxyHealthPrunedOnReload(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertProxy(config.ProxyEntry{ID: "px-1", URL: "http://1.2.3.4:8080", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := registry.New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	r.Health().RecordProxyFailure("px-1")
	if r.Health().IsProxyAvailable("px-1") {
		t.Fatal("px-1 should be cooling down")
	}
	if err := st.DeleteProxy("px-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	if _, _, _, seen := r.Health().ProxyStatus("px-1"); seen {
		t.Fatal("deleted proxy's health state must be pruned on reload")
	}
}

// TestProxyTransportWarmsTransport: pool clients must reuse one cached client
// per proxy URL (warm TCP/TLS/keep-alive sockets), not rebuild a Transport per
// request — and the idle knobs that keep those sockets warm must be set.
func TestProxyTransportWarmsTransport(t *testing.T) {
	px := New(registry.New(), nil, 5*time.Second)
	a, err := px.clientFor("http://user:pass@1.2.3.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	b, err := px.clientFor("http://user:pass@1.2.3.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("clientFor must return the cached client for the same URL")
	}
	tr, ok := a.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("pool client transport is %T, want *http.Transport", a.Transport)
	}
	if tr.MaxIdleConnsPerHost < 2 {
		t.Fatalf("MaxIdleConnsPerHost=%d: warm pool needs >1 idle socket per origin", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout must be set so dead NAT bindings get re-dialed")
	}
}

// TestEgressRetriesDeadProxyWithSibling: a direct (non-combo), single-account
// provider has no other fallback if a transport-level failure through a pool
// proxy is treated as "upstream unreachable" — but the failure is proxy-
// specific, not upstream-specific, and a healthy sibling proxy should let the
// request succeed instead of surfacing a 502 to the caller.
func TestEgressRetriesDeadProxyWithSibling(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-px","created":1,"model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// A live forward proxy that actually reaches upstream.
	var aliveHits int
	fwd := &http.Transport{DisableCompression: true}
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil || r.URL.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		aliveHits++
		out, err := fwd.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer out.Body.Close()
		w.WriteHeader(out.StatusCode)
		_, _ = io.Copy(w, out.Body)
	}))
	defer alive.Close()

	// A "dead" proxy: a server we bind then immediately close, so dialing it
	// hard-fails at the transport level (connection refused), exactly like a
	// downed forward proxy.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "px.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertProvider(config.Provider{ID: "p", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true, ProxyRotate: true}); err != nil {
		t.Fatal(err)
	}
	// Position the dead entry first so the very first pick fails and must retry.
	if err := st.UpsertProxy(config.ProxyEntry{ID: "px-dead", Label: "dead", URL: deadURL, Enabled: true, Position: 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProxy(config.ProxyEntry{ID: "px-alive", Label: "alive", URL: alive.URL, Enabled: true, Position: 1}); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatal(err)
	}
	px := New(reg, st, 5*time.Second)
	defer px.WaitLogs()

	// Single-account, non-combo (plan == nil) request: this is exactly the
	// path that previously wrote a 502 on the first transport failure with no
	// retry, even with a perfectly healthy sibling proxy in the pool.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	if rec.Code != 200 {
		t.Fatalf("status: %d body=%s (dead proxy must not surface as a hard failure when a healthy sibling exists)", rec.Code, rec.Body.String())
	}
	if aliveHits == 0 {
		t.Fatal("request never made it through the sibling proxy")
	}
	if reg.Health().IsProxyAvailable("px-dead") {
		t.Fatal("the dead proxy's failure must be recorded so it cools down")
	}
}

// clientFor must cache per URL (connection reuse) and never leak credentials
// into the log label.
func TestProxyClientCacheAndLabel(t *testing.T) {
	px := New(registry.New(), nil, 5*time.Second)
	a, err := px.clientFor("http://user:pass@1.2.3.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	b, err := px.clientFor("http://user:pass@1.2.3.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("clientFor must return the cached client for the same URL")
	}
	if got := proxyLabel(config.ProxyEntry{ID: "px-1", Label: "one", URL: "http://user:pass@1.2.3.4:8080"}); got != "one" {
		t.Fatalf("label should win when set, got %q", got)
	}
	got := proxyLabel(config.ProxyEntry{ID: "px-1", URL: "http://user:pass@1.2.3.4:8080"})
	if got != "http://1.2.3.4:8080" {
		t.Fatalf("host label leaked credentials or wrong shape: %q", got)
	}
	// URLs that can't be parsed for a host fall back to the entry ID.
	if got := proxyLabel(config.ProxyEntry{ID: "px-9", URL: "not a url"}); got != "px-9" {
		t.Fatalf("expected ID fallback, got %q", got)
	}
}
