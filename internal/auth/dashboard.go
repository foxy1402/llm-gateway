package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "llm_gateway_session"
	sessionTTL        = 24 * time.Hour
)

// Dashboard manages login sessions signed with DASHBOARD_SECRET.
type Dashboard struct {
	password string
	secret   []byte

	mu       sync.RWMutex
	sessions map[string]time.Time // sessionID -> expiry
}

func NewDashboard(password, secret string) *Dashboard {
	return &Dashboard{
		password: password,
		secret:   []byte(secret),
		sessions: map[string]time.Time{},
	}
}

// LoginHandler validates the posted password and sets a session cookie.
func (d *Dashboard) LoginHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	// Allow both JSON and form-encoded.
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		body.Password = r.FormValue("password")
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(d.password)) != 1 {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	sid, err := d.createSession()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/dashboard",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   r.TLS != nil,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// LogoutHandler clears the session cookie and forgets the session.
func (d *Dashboard) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if sid, ok := d.verifyToken(c.Value); ok {
			d.mu.Lock()
			delete(d.sessions, sid)
			d.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/dashboard",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Middleware protects dashboard routes; HTML requests redirect to /dashboard/login.
func (d *Dashboard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		// JSON API callers get 401; page navigations get a redirect to login.
		accept := r.Header.Get("Accept")
		if strings.Contains(accept, "application/json") || strings.HasPrefix(r.URL.Path, "/dashboard/api/") {
			writeOpenAIError(w, http.StatusUnauthorized, "not authenticated", "auth_required")
			return
		}
		http.Redirect(w, r, "/dashboard/login", http.StatusFound)
	})
}

func (d *Dashboard) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	sid, ok := d.verifyToken(c.Value)
	if !ok {
		return false
	}
	d.mu.RLock()
	expiry, found := d.sessions[sid]
	expired := found && time.Now().After(expiry)
	d.mu.RUnlock()
	if !found {
		return false
	}
	if expired {
		// Upgrade to write-lock only to evict the stale entry.
		d.mu.Lock()
		delete(d.sessions, sid)
		d.mu.Unlock()
		return false
	}
	return true
}

// createSession generates a token of form nonce.expiryHex.hmacHex.
func (d *Dashboard) createSession() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sid := base64.RawURLEncoding.EncodeToString(nonce)
	expiry := time.Now().Add(sessionTTL)
	payload := sid + "." + strconv.FormatInt(expiry.Unix(), 16)
	mac := hmac.New(sha256.New, d.secret)
	mac.Write([]byte(payload))
	token := payload + "." + hex.EncodeToString(mac.Sum(nil))

	d.mu.Lock()
	d.sessions[sid] = expiry
	// Sweep expired sessions opportunistically.
	now := time.Now()
	for k, v := range d.sessions {
		if now.After(v) {
			delete(d.sessions, k)
		}
	}
	d.mu.Unlock()
	return token, nil
}

func (d *Dashboard) verifyToken(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, d.secret)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", false
	}
	expUnix, err := strconv.ParseInt(parts[1], 16, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return "", false
	}
	return parts[0], true
}

// MustBeValidSecret enforces a minimum secret length at startup.
func MustBeValidSecret(secret string) error {
	if len(secret) < 32 {
		return fmt.Errorf("DASHBOARD_SECRET must be at least 32 characters (got %d)", len(secret))
	}
	return nil
}
