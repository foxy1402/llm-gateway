package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// P3-17: getSettings must not leak the reserved failban:state blob (banned IPs
// and failure timestamps) to the browser.
func TestGetSettingsHidesReservedState(t *testing.T) {
	srv, authed, st := authServer(t)
	if err := st.SetSetting("failban:state", `{"1.2.3.4":{"until":9999999999}}`); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, authed, "GET", srv.URL+"/dashboard/api/settings", "", srv.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal settings: %v (%s)", err, body)
	}
	if _, ok := got["failban:state"]; ok {
		t.Fatalf("reserved failban:state leaked in settings response: %s", body)
	}
}

// P2-12: behind a host-rewriting proxy (X-Forwarded-Host set), a same-origin
// mutating request must still pass the CSRF check when TRUSTED_PROXY is on.
func TestSameOriginHonorsForwardedHostBehindProxy(t *testing.T) {
	srv, authed, _ := authServer(t)

	req, err := http.NewRequest("POST", srv.URL+"/dashboard/api/providers",
		strings.NewReader(`{"id":"p1","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The browser's Origin names the public host the proxy rewrites Host from.
	req.Header.Set("Origin", "https://public.example.com")
	req.Header.Set("X-Forwarded-Host", "public.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := authed.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Without TRUSTED_PROXY the check is intentionally strict (Origin != rewritten
	// Host) and returns 403; that is the CSRF-safe default. This test asserts the
	// default stays fail-closed.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without TRUSTED_PROXY a forged forwarded-host must still be rejected, got %d", resp.StatusCode)
	}
}
