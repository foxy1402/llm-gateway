package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

const testPassword = "correct-horse-battery-staple"

// authServer builds a dashboard with real authentication wired up, plus an HTTP
// client holding a valid session cookie. Both are needed to tell "the handler
// rejected this" apart from "the auth middleware rejected this" — the earlier
// tests ran with Auth: nil and skipped as soon as they saw a 401.
func authServer(t *testing.T) (srv *httptest.Server, authed *http.Client, st *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatalf("reload: %v", err)
	}
	dash := auth.NewDashboard(testPassword, "test-secret-value")
	mux := http.NewServeMux()
	Mount(mux, &Deps{Store: st, Reg: reg, Auth: dash, Env: &config.Env{}})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	authed = &http.Client{Jar: jar}
	resp, err := authed.Post(srv.URL+"/dashboard/api/login", "application/json",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed with %d", resp.StatusCode)
	}
	return srv, authed, st
}

// Every dashboard API route must require a session. A single unguarded route
// leaks every stored upstream API key, so this enumerates them rather than
// spot-checking one.
func TestAllDashboardRoutesRequireAuth(t *testing.T) {
	srv, _, _ := authServer(t)
	routes := []struct{ method, path string }{
		{"GET", "/dashboard/api/providers"},
		{"POST", "/dashboard/api/providers"},
		{"GET", "/dashboard/api/providers/p1"},
		{"PUT", "/dashboard/api/providers/p1"},
		{"DELETE", "/dashboard/api/providers/p1"},
		{"POST", "/dashboard/api/providers/p1/test"},
		{"POST", "/dashboard/api/models/list"},
		{"POST", "/dashboard/api/providers/p1/accounts"},
		{"DELETE", "/dashboard/api/providers/p1/accounts/a1"},
		{"PUT", "/dashboard/api/providers/p1/accounts"},
		{"GET", "/dashboard/api/providers/p1/models"},
		{"POST", "/dashboard/api/providers/p1/models/fetch"},
		{"GET", "/dashboard/api/proxies"},
		{"POST", "/dashboard/api/proxies"},
		{"PUT", "/dashboard/api/proxies/x1"},
		{"DELETE", "/dashboard/api/proxies/x1"},
		{"GET", "/dashboard/api/combos"},
		{"POST", "/dashboard/api/combos"},
		{"PUT", "/dashboard/api/combos/c1"},
		{"DELETE", "/dashboard/api/combos/c1"},
		{"POST", "/dashboard/api/combos/c1/test"},
		{"GET", "/dashboard/api/logs"},
		{"GET", "/dashboard/api/logs/chart"},
		{"GET", "/dashboard/api/logs/1"},
		{"POST", "/dashboard/api/logs/clear"},
		{"GET", "/dashboard/api/settings"},
		{"PUT", "/dashboard/api/settings"},
		{"GET", "/dashboard/api/export"},
		{"POST", "/dashboard/api/import"},
		{"GET", "/dashboard/api/health"},
		{"GET", "/dashboard/api/overview"},
		{"GET", "/dashboard/api/endpoint"},
	}
	bare := &http.Client{}
	for _, rt := range routes {
		req, err := http.NewRequest(rt.method, srv.URL+rt.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := bare.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rt.method, rt.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d without a session, want 401", rt.method, rt.path, resp.StatusCode)
		}
	}
}

// SameSite=Strict was the only CSRF defense. A state-changing request carrying a
// foreign Origin must be refused even when the session cookie is valid.
func TestCrossOriginWriteRejected(t *testing.T) {
	srv, authed, st := authServer(t)

	resp := doJSON(t, authed, "POST", srv.URL+"/dashboard/api/providers",
		`{"id":"evil","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`,
		"https://attacker.example")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST returned %d, want 403", resp.StatusCode)
	}
	provs, err := st.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 0 {
		t.Fatalf("the cross-origin write landed anyway: %+v", provs)
	}
}

// A same-origin write (the real dashboard) must still work — the CSRF check must
// not break the app it protects.
func TestSameOriginWriteAccepted(t *testing.T) {
	srv, authed, st := authServer(t)
	req, err := http.NewRequest("POST", srv.URL+"/dashboard/api/providers",
		strings.NewReader(`{"id":"p1","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL) // the dashboard's own origin
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin POST returned %d, want success", resp.StatusCode)
	}
	provs, err := st.ListProviders()
	if err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 || provs[0].ID != "p1" {
		t.Fatalf("provider was not created: %+v", provs)
	}
}

// A GET is not state-changing, so a foreign Origin must not block it (browsers
// send Origin on some GETs, and blocking them would break normal navigation).
func TestCrossOriginReadAllowed(t *testing.T) {
	srv, authed, _ := authServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/dashboard/api/providers", nil)
	req.Header.Set("Origin", "https://attacker.example")
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cross-origin GET returned %d, want 200 (reads are not state-changing)", resp.StatusCode)
	}
}

// A private/loopback base_url turns the gateway into an SSRF proxy into whatever
// the container can reach. It must be refused unless ALLOW_PRIVATE_BASE_URL is
// explicitly set.
func TestPrivateBaseURLRejected(t *testing.T) {
	srv, authed, _ := authServer(t)
	for _, bad := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:9000",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5",
		"http://192.168.1.1",
		"ftp://api.example.com",
		"not-a-url",
	} {
		body := `{"id":"ssrf","base_url":"` + bad + `","auth_key":"k","model":"m","weight":1,"enabled":true}`
		resp := doJSON(t, authed, "POST", srv.URL+"/dashboard/api/providers", body, srv.URL)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("base_url %q returned %d, want 400", bad, resp.StatusCode)
		}
	}
}

// Only the three operational settings may be written through the API. An
// arbitrary key would let a session poison anything the gateway reads from the
// settings table.
func TestSettingsWriteAllowlist(t *testing.T) {
	srv, authed, st := authServer(t)

	// Not on the allowlist.
	resp := doJSON(t, authed, "PUT", srv.URL+"/dashboard/api/settings", `{"dashboard.password":"pwned"}`, srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("writing a non-allowlisted setting returned %d, want 400", resp.StatusCode)
	}
	if v, _ := st.GetSetting("dashboard.password"); v != "" {
		t.Errorf("non-allowlisted setting was written: %q", v)
	}

	// Allowlisted but invalid values.
	for _, bad := range []string{
		`{"health.cooldown":"-5"}`,
		`{"health.cooldown":"abc"}`,
		`{"log.retention_days":"0"}`,
		`{"health.error_codes":"429,not-a-code"}`,
		`{"health.error_codes":"429,99"}`,
	} {
		resp := doJSON(t, authed, "PUT", srv.URL+"/dashboard/api/settings", bad, srv.URL)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("settings payload %s returned %d, want 400", bad, resp.StatusCode)
		}
	}

	// A valid write lands.
	resp = doJSON(t, authed, "PUT", srv.URL+"/dashboard/api/settings", `{"health.cooldown":"90","health.error_codes":"429,503","log.retention_days":"7"}`, srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid settings write returned %d", resp.StatusCode)
	}
	if v, _ := st.GetSetting("health.cooldown"); v != "90" {
		t.Errorf("health.cooldown = %q, want 90", v)
	}
	if v, _ := st.GetSetting("log.retention_days"); v != "7" {
		t.Errorf("log.retention_days = %q, want 7", v)
	}
}

// Deleting something that isn't there used to report success, so the SPA removed
// the row from its local state and the operator believed a delete happened that
// never did.
func TestDeleteMissingReturns404(t *testing.T) {
	srv, authed, _ := authServer(t)
	for _, path := range []string{
		"/dashboard/api/providers/ghost",
		"/dashboard/api/combos/ghost",
		"/dashboard/api/proxies/ghost",
	} {
		resp := doJSON(t, authed, "DELETE", srv.URL+path, "", srv.URL)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s returned %d, want 404", path, resp.StatusCode)
		}
	}
}

// A provider and a combo share one routing namespace: whichever is looked up
// first wins, so a duplicate ID silently shadows an existing route. Both
// directions must be refused at creation time.
func TestDuplicateAndShadowedIDsRejected(t *testing.T) {
	srv, authed, _ := authServer(t)

	resp := doJSON(t, authed, "POST", srv.URL+"/dashboard/api/providers",
		`{"id":"dup","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`, srv.URL)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("initial provider create failed with %d", resp.StatusCode)
	}

	// Same provider ID twice.
	resp = doJSON(t, authed, "POST", srv.URL+"/dashboard/api/providers",
		`{"id":"dup","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`, srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate provider ID returned %d, want 409", resp.StatusCode)
	}

	// A combo taking the provider's ID would shadow it in routing.
	resp = doJSON(t, authed, "POST", srv.URL+"/dashboard/api/combos",
		`{"id":"dup","display_name":"D","rotation":"priority","enabled":true,"members":[{"provider_id":"dup"}]}`, srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("combo shadowing a provider ID returned %d, want 409", resp.StatusCode)
	}
}

// IDs land in URL paths and in the model-routing namespace; whitespace, slashes
// and empty strings produce routes that can never be addressed again.
func TestInvalidIDsRejected(t *testing.T) {
	srv, authed, _ := authServer(t)
	for _, bad := range []string{"", " ", "a/b", "a b", "a\tb", strings.Repeat("x", 200)} {
		payload, err := json.Marshal(map[string]any{
			"id": bad, "base_url": "https://api.example.com", "auth_key": "k",
			"model": "m", "weight": 1, "enabled": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		resp := doJSON(t, authed, "POST", srv.URL+"/dashboard/api/providers", string(payload), srv.URL)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("provider ID %q returned %d, want 400", bad, resp.StatusCode)
		}
	}
}

// A negative ?hours= put the cutoff in the FUTURE, so the chart came back empty
// with nothing to explain it; a huge one scanned the whole table for the same
// empty result. Both must clamp into [1, 24*90]. Proven with a real row: an
// unclamped negative value excludes it, a clamped one includes it.
func TestLogsChartClampsHours(t *testing.T) {
	srv, authed, st := authServer(t)
	if err := st.LogRequest(config.LogEntry{
		ModelIn: "m", ProviderUsed: "p", Endpoint: "chat.completions", Status: 200, LatencyMs: 5,
	}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"hours=0", "hours=-5", "hours=abc", "hours=", "hours=99999999"} {
		resp := doJSON(t, authed, "GET", srv.URL+"/dashboard/api/logs/chart?"+q, "", srv.URL)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("chart with %s returned %d: %s", q, resp.StatusCode, body)
			continue
		}
		var points []struct {
			Bucket int64  `json:"bucket"`
			Count  int64  `json:"count"`
			Prov   string `json:"provider"`
		}
		if err := json.Unmarshal(body, &points); err != nil {
			t.Errorf("chart with %s returned unparseable body %s: %v", q, body, err)
			continue
		}
		if len(points) == 0 {
			t.Errorf("chart with %s returned no buckets — hours was not clamped to at least 1, so the cutoff landed in the future", q)
		}
	}
}

// API responses carry provider auth keys, so they must never be cached.
func TestAPIResponsesAreNoStore(t *testing.T) {
	srv, authed, _ := authServer(t)
	resp := doJSON(t, authed, "GET", srv.URL+"/dashboard/api/providers", "", srv.URL)
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func doJSON(t *testing.T, c *http.Client, method, rawurl, body, origin string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, rawurl, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawurl, err)
	}
	return resp
}
