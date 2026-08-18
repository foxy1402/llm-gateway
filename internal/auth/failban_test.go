package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// testGate builds a FailBan with a controllable clock and canned config.
func testGate(cfg config.BanConfig) (*FailBan, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	f := NewFailBan(cfg, "", nil)
	f.now = func() time.Time { return now }
	return f, &now
}

// drive sends one request through the guard from the given IP and returns the
// status; handler replies with the scripted status code.
func drive(f *FailBan, ip string, innerStatus int, xff string) (int, int) {
	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(innerStatus)
	})
	req := httptest.NewRequest("POST", "/dashboard/api/login", nil)
	req.RemoteAddr = ip + ":1234"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	f.Guard(inner).ServeHTTP(rec, req)
	return rec.Code, calls
}

func TestFailBanBansAfterMaxFail(t *testing.T) {
	f, _ := testGate(config.BanConfig{MaxFail: 3, FindTime: 2 * time.Minute, BanTime: 30 * time.Minute})
	for i := 0; i < 3; i++ {
		code, calls := drive(f, "1.2.3.4", http.StatusUnauthorized, "")
		if code != http.StatusUnauthorized || calls != 1 {
			t.Fatalf("attempt %d: got %d calls=%d, want 401 forwarded", i, code, calls)
		}
	}
	code, calls := drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	if code != http.StatusTooManyRequests {
		t.Fatalf("4th attempt: got %d, want 429", code)
	}
	if calls != 0 {
		t.Fatal("banned request reached the login handler")
	}
	// A different IP is unaffected.
	if code, _ := drive(f, "9.9.9.9", http.StatusUnauthorized, ""); code != http.StatusUnauthorized {
		t.Fatalf("other IP: got %d, want 401", code)
	}
}

func TestFailBanUnbansAfterDuration(t *testing.T) {
	f, nowp := testGate(config.BanConfig{MaxFail: 2, FindTime: time.Minute, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	if code, _ := drive(f, "1.2.3.4", http.StatusOK, ""); code != http.StatusTooManyRequests {
		t.Fatalf("want 429 while banned, got %d", code)
	}
	*nowp = nowp.Add(31 * time.Minute)
	if code, _ := drive(f, "1.2.3.4", http.StatusOK, ""); code != http.StatusOK {
		t.Fatalf("want request through after ban expiry, got %d", code)
	}
}

func TestFailBanSuccessResetsCounter(t *testing.T) {
	f, _ := testGate(config.BanConfig{MaxFail: 3, FindTime: time.Minute, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	if code, _ := drive(f, "1.2.3.4", http.StatusOK, ""); code != http.StatusOK {
		t.Fatalf("want success through, got %d", code)
	}
	// Window was cleared: two more failures must not tip the ban.
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	if code, _ := drive(f, "1.2.3.4", http.StatusUnauthorized, ""); code != http.StatusUnauthorized {
		t.Fatalf("counter not reset by success, got %d", code)
	}
}

func TestFailBanEscalates(t *testing.T) {
	f, nowp := testGate(config.BanConfig{MaxFail: 2, FindTime: time.Minute, BanTime: 30 * time.Minute, MaxBan: 24 * time.Hour})
	banIP := "1.2.3.4"
	burnIn := func() {
		*nowp = nowp.Add(f.cfg.MaxBan) // jump past any active ban
		drive(f, banIP, http.StatusUnauthorized, "")
		drive(f, banIP, http.StatusUnauthorized, "")
	}
	burnIn()
	st := f.ips[banIP]
	first := time.Unix(st.Until, 0).Sub(*nowp)
	if first < 29*time.Minute || first > 30*time.Minute {
		t.Fatalf("first ban %v, want ~30m", first)
	}
	burnIn()
	st = f.ips[banIP]
	second := time.Unix(st.Until, 0).Sub(*nowp)
	if second < 59*time.Minute || second > 60*time.Minute {
		t.Fatalf("second ban %v, want ~60m", second)
	}
}

// 400s (malformed JSON) must neither count toward the ban nor wipe an
// attacker's accumulated failures — otherwise interleaving one bad request
// after every N-1 wrong passwords would evade the gate forever.
func TestFailBanMalformedBodyDoesNotReset(t *testing.T) {
	f, _ := testGate(config.BanConfig{MaxFail: 3, FindTime: time.Minute, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusBadRequest, "") // junk body: no count, no reset
	if code, calls := drive(f, "1.2.3.4", http.StatusUnauthorized, ""); code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("third failure should reach handler, got %d calls=%d", code, calls)
	}
	if code, calls := drive(f, "1.2.3.4", http.StatusOK, ""); code != http.StatusTooManyRequests || calls != 0 {
		t.Fatalf("want ban after 3 real failures despite the 400, got %d calls=%d", code, calls)
	}
}

func TestFailBanRetriesCounterResetWhenStale(t *testing.T) {
	f, nowp := testGate(config.BanConfig{MaxFail: 3, FindTime: time.Minute, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	*nowp = nowp.Add(2 * time.Minute) // beyond FindTime
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	if code, _ := drive(f, "1.2.3.4", http.StatusUnauthorized, ""); code != http.StatusUnauthorized {
		t.Fatalf("stale failures must not ban, got %d", code)
	}
	if code, calls := drive(f, "1.2.3.4", http.StatusOK, ""); code != http.StatusTooManyRequests || calls != 0 {
		t.Fatalf("want ban after 3 fresh failures, got %d calls=%d", code, calls)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name        string
		behindProxy bool
		remote      string
		xff         string
		want        string
	}{
		{"direct uses RemoteAddr", false, "203.0.113.7:9000", "", "203.0.113.7"},
		{"direct ignores spoof xff", false, "203.0.113.7:9000", "6.6.6.6", "203.0.113.7"},
		{"proxy uses first xff hop", true, "10.0.0.1:9000", "198.51.100.9, 10.0.0.1", "198.51.100.9"},
		{"proxy falls back on malformed xff", true, "10.0.0.1:9000", "garbage", "10.0.0.1"},
		{"proxy no headers falls back", true, "10.0.0.1:9000", "", "10.0.0.1"},
		{"ipv6 remote", false, "[2001:db8::1]:443", "", "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFailBan(config.BanConfig{MaxFail: 5, BehindProxy: tc.behindProxy}, "", nil)
			req := httptest.NewRequest("POST", "/", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := f.ClientIP(req); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestFailBanPersistenceRoundTrip(t *testing.T) {
	f, _ := testGate(config.BanConfig{MaxFail: 2, FindTime: time.Minute, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	drive(f, "1.2.3.4", http.StatusUnauthorized, "") // banned now
	blob, err := json.Marshal(f.ips)
	if err != nil {
		t.Fatal(err)
	}
	// Simulated restart: a fresh instance loads the persisted blob.
	now := time.Unix(1_700_000_000, 0)
	restarted := NewFailBan(config.BanConfig{MaxFail: 2}, string(blob), nil)
	restarted.now = func() time.Time { return now }
	if code, calls := drive(restarted, "1.2.3.4", http.StatusOK, ""); code != http.StatusTooManyRequests || calls != 0 {
		t.Fatalf("ban lost across restart: got %d calls=%d", code, calls)
	}
	// Corrupt state starts clean rather than crashing.
	corrupt := NewFailBan(config.BanConfig{MaxFail: 2}, "{not json", nil)
	if code, _ := drive(corrupt, "1.2.3.4", http.StatusOK, ""); code != http.StatusOK {
		t.Fatalf("corrupt state should not block logins, got %d", code)
	}
}

func TestGuardRetryAfterHeader(t *testing.T) {
	f, _ := testGate(config.BanConfig{MaxFail: 1, BanTime: 30 * time.Minute})
	drive(f, "1.2.3.4", http.StatusUnauthorized, "")
	req := httptest.NewRequest("POST", "/dashboard/api/login", nil)
	req.RemoteAddr = "1.2.3.4:1"
	rec := httptest.NewRecorder()
	f.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || !strings.Contains(rec.Body.String(), "try again") {
		t.Fatalf("missing Retry-After/message: header=%q body=%q", ra, rec.Body.String())
	}
}
