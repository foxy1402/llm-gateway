package auth

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// P2-7: the persistence write must happen OUTSIDE the ban mutex, so a
// brute-force spray (each 401 triggering a save) can't serialize every ban
// decision behind a SQLite write. This proves the write is off-lock by blocking
// save() and confirming a concurrent critical-section entry still completes.
func TestFailBanPersistDoesNotHoldLock(t *testing.T) {
	inSave := make(chan struct{}, 1)
	release := make(chan struct{})
	var saves atomic.Int64

	f := NewFailBan(config.BanConfig{MaxFail: 5, FindTime: time.Minute, BanTime: time.Minute},
		"", func(string) error {
			saves.Add(1)
			select {
			case inSave <- struct{}{}:
			default:
			}
			<-release
			return nil
		})
	f.now = time.Now

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.fail("1.2.3.4") // triggers persist, which blocks inside save()
	}()

	select {
	case <-inSave:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("save was never called")
	}

	// With save() blocked, acquiring f.mu must still succeed promptly — proving
	// the mutex is not held across the DB write.
	checked := make(chan struct{})
	go func() {
		f.mu.Lock()
		_ = len(f.ips)
		f.mu.Unlock()
		close(checked)
	}()
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a ban decision blocked while persistence was in flight — save still holds the mutex")
	}

	close(release)
	<-done
	if saves.Load() == 0 {
		t.Fatal("expected at least one persist")
	}
}

// Persistence is still best-effort and state-changing: after a ban the snapshot
// passed to save must contain the banned IP.
func TestFailBanPersistPayload(t *testing.T) {
	var last atomic.Value
	f := NewFailBan(config.BanConfig{MaxFail: 1, FindTime: time.Minute, BanTime: time.Minute},
		"", func(s string) error {
			last.Store(s)
			return nil
		})
	f.now = time.Now
	f.fail("9.9.9.9")
	if s, _ := last.Load().(string); !strings.Contains(s, "9.9.9.9") {
		t.Fatalf("persisted snapshot missing the banned ip: %q", s)
	}
}

// P2-13: the Secure-cookie decision must be a fixed configuration choice, not a
// per-request read of the client-spoofable X-Forwarded-Proto header.
func TestSecureCookieIgnoresForwardedProto(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	plain := NewDashboard("pw", "0123456789abcdef0123456789abcdef", false)
	if plain.secureCookie(req) {
		t.Fatal("X-Forwarded-Proto must not force a Secure cookie")
	}

	proxied := NewDashboard("pw", "0123456789abcdef0123456789abcdef", true)
	if !proxied.secureCookie(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("configured secure cookie should be Secure regardless of XFP")
	}
}
