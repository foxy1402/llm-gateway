package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardSessionRoundTrip(t *testing.T) {
	d := NewDashboard("pw", "0123456789abcdef0123456789abcdef")
	token, err := d.createSession()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", sessionCookieName+"="+token)
	if !d.validSession(req) {
		t.Fatal("session should be valid")
	}
}

func TestDashboardRejectsBadToken(t *testing.T) {
	d := NewDashboard("pw", "0123456789abcdef0123456789abcdef")
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", sessionCookieName+"=bogus.invalid.sig")
	if d.validSession(req) {
		t.Fatal("bad token should be rejected")
	}
}

func TestMustBeValidSecret(t *testing.T) {
	if err := MustBeValidSecret("short"); err == nil {
		t.Fatal("expected error for short secret")
	}
	if err := MustBeValidSecret("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
}

func TestAPIKeyGuard(t *testing.T) {
	g := APIKey("sekret")
	ok := false
	h := g(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok = true; w.WriteHeader(200) }))

	// Missing key.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d", rec.Code)
	}

	// Correct key.
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	h.ServeHTTP(rec2, req)
	if rec2.Code != 200 || !ok {
		t.Fatalf("valid key: status %d ok=%v", rec2.Code, ok)
	}
}
