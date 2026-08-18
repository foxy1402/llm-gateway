package auth

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llm-gateway/internal/config"
)

// BanSettingsKey is the settings-table entry under which the ban state is
// persisted, so a redeploy doesn't reset brute-force progress.
const BanSettingsKey = "failban:state"

// ipState is the per-client record. Fails holds unix-second timestamps of
// recent failures; Until is the ban deadline; Level is the escalation strike
// count, kept across bans so slow brute force locks itself out harder.
type ipState struct {
	Fails []int64 `json:"fails,omitempty"`
	Until int64   `json:"until,omitempty"`
	Level int     `json:"level,omitempty"`
}

// FailBan counts failed requests per client IP and temporarily rejects
// further attempts once the threshold is crossed.
type FailBan struct {
	cfg  config.BanConfig
	save func(string) error // nil disables persistence
	now  func() time.Time   // test hook

	mu  sync.Mutex
	ips map[string]*ipState
}

// NewFailBan builds a gate; `initial` is a previously persisted state blob
// (from GetSetting(BanSettingsKey)), and save persists future state changes.
func NewFailBan(cfg config.BanConfig, initial string, save func(string) error) *FailBan {
	f := &FailBan{
		save: save,
		now:  time.Now,
		ips:  map[string]*ipState{},
	}
	if cfg.MaxFail <= 0 {
		cfg.MaxFail = 5
	}
	if cfg.FindTime <= 0 {
		cfg.FindTime = 10 * time.Minute
	}
	if cfg.BanTime <= 0 {
		cfg.BanTime = 30 * time.Minute
	}
	if cfg.MaxBan < cfg.BanTime {
		cfg.MaxBan = 24 * time.Hour
	}
	f.cfg = cfg
	if initial != "" {
		_ = json.Unmarshal([]byte(initial), &f.ips) // corrupt state starts fresh
	}
	return f
}

// Guard wraps a handler: banned IPs get 429 + Retry-After without touching the
// handler; otherwise the response status feeds the counter — 401/403 count as
// failures, success clears the failure window (escalation level is kept).
func (f *FailBan) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := f.ClientIP(r)
		if until, banned := f.check(ip); banned {
			secs := until.Unix() - f.now().Unix() + 1
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			http.Error(w, fmt.Sprintf("too many failed attempts; try again in %d minutes", (secs+59)/60), http.StatusTooManyRequests)
			return
		}
		sw := &statusCapture{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		switch {
		case sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden:
			f.fail(ip)
		case sw.status >= 200 && sw.status < 300:
			// Only a real login success clears the window — a 400 for a
			// malformed body must not reset an attacker's failure counter.
			f.success(ip)
		}
		// 4xx format errors and 5xx neither count nor reset.
	})
}

// ClientIP resolves the caller's IP. Direct deployments use RemoteAddr; behind
// a PaaS load balancer (BehindProxy) the platform's X-Forwarded-For carries the
// real client — trust it only there, or spoofed headers would evade bans.
func (f *FailBan) ClientIP(r *http.Request) string {
	if f.cfg.BehindProxy {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			ip, _, _ := strings.Cut(v, ",")
			if p := net.ParseIP(strings.TrimSpace(ip)); p != nil {
				return p.String()
			}
		}
		if p := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); p != nil {
			return p.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if p := net.ParseIP(host); p != nil {
			return p.String()
		}
		return host
	}
	if p := net.ParseIP(r.RemoteAddr); p != nil {
		return p.String()
	}
	return r.RemoteAddr
}

// check reports whether ip is currently banned and prunes stale state so the
// map stays bounded by recently-active addresses.
func (f *FailBan) check(ip string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.ips[ip]
	if !ok {
		return time.Time{}, false
	}
	now := f.now()
	st.prune(now, f.cfg.FindTime)
	if st.Until > now.Unix() {
		return time.Unix(st.Until, 0), true
	}
	st.Until = 0
	if len(st.Fails) == 0 && st.Level == 0 {
		delete(f.ips, ip)
		f.persistLocked()
	}
	return time.Time{}, false
}

func (f *FailBan) fail(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	st := f.ips[ip]
	if st == nil {
		st = &ipState{}
		f.ips[ip] = st
	}
	st.prune(now, f.cfg.FindTime)
	st.Fails = append(st.Fails, now.Unix())
	if len(st.Fails) >= f.cfg.MaxFail {
		st.Level++
		st.Until = now.Add(f.banDuration(st.Level)).Unix()
		st.Fails = st.Fails[:0]
	}
	f.persistLocked()
}

func (f *FailBan) success(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.ips[ip]
	if st == nil {
		return
	}
	if len(st.Fails) > 0 {
		st.Fails = st.Fails[:0]
		f.persistLocked()
	}
}

// banDuration doubles the base duration per strike (30m, 1h, 2h…), capped.
func (f *FailBan) banDuration(level int) time.Duration {
	d := f.cfg.BanTime
	for i := 1; i < level && d < f.cfg.MaxBan; i++ {
		d *= 2
		if d > f.cfg.MaxBan {
			return f.cfg.MaxBan
		}
	}
	return d
}

func (f *FailBan) persistLocked() {
	if f.save == nil {
		return
	}
	b, err := json.Marshal(f.ips)
	if err != nil {
		return
	}
	_ = f.save(string(b)) // persistence is best-effort; in-memory state still bans
}

func (s *ipState) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window).Unix()
	kept := s.Fails[:0]
	for _, ts := range s.Fails {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	s.Fails = kept
}

// statusCapture records the response status so Guard can classify the outcome.
// Handlers that only call Write() leave status 0 until WriteHeader fires.
type statusCapture struct {
	http.ResponseWriter
	status int
}

func (s *statusCapture) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flusher etc.).
func (s *statusCapture) Unwrap() http.ResponseWriter { return s.ResponseWriter }
