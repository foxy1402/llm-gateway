package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"llm-gateway/internal/config"
)

// Egress proxy pool: some upstreams rate-limit per source IP rather than per
// key, so a pool of 10 keys behind one IP still hits one shared ceiling.
// Providers/combos flagged ProxyRotate dial out through the shared pool
// (internal/registry.NextProxy), advancing one proxy per upstream attempt, so
// a retry after a 429 leaves from a different address. Everything else keeps
// using the direct client — an extra hop is pure downside when the upstream
// counts per key.

// proxySchemes are the URL schemes http.Transport can dial through. socks5h
// defers DNS to the proxy (the usual choice when the proxy is the only host
// that can resolve or should see the upstream hostname); socks5 resolves
// locally. No third-party dependency is involved: net/http speaks both.
var proxySchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks5h": true,
}

// ValidateProxyURL reports why raw is unusable as a pool entry, or nil if it is
// fine. Rejecting at save time matters: a bad URL here doesn't fail loudly at
// startup, it fails on some future request that was supposed to be routed
// around a rate limit.
func ValidateProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("proxy url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("proxy url %q is not a valid URL: %w", raw, err)
	}
	if !proxySchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf("proxy url %q: scheme must be http, https, socks5 or socks5h", raw)
	}
	// Hostname (not just Host): "socks5://:1080" parses with a non-empty
	// Host (":1080") but no actual hostname to dial — reject that too.
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("proxy url %q: missing host", raw)
	}
	// A SOCKS proxy has no default port and a typo'd one dials nowhere; HTTP
	// proxies rarely run on 80/443 either, so require the port in both cases
	// rather than let net/http guess.
	if u.Port() == "" {
		return fmt.Errorf("proxy url %q: missing port (e.g. %s://host:1080)", raw, u.Scheme)
	}
	return nil
}

// newUpstreamTransport builds the shared upstream transport. proxyURL nil = dial
// direct. Every knob is identical across direct and proxied clients so routing
// through the pool can't quietly change streaming/timeout behavior.
//
// Connection warmth: Go's Transport keeps idle keep-alive connections warm
// automatically — the pool clients below are cached per proxy URL, so repeat
// requests reuse the already-open TCP/TLS tunnel to the proxy (and through it,
// the upstream) instead of paying connect+handshake per attempt. The idle
// knobs here size that warm set: enough idle sockets per proxy to absorb
// normal concurrency, a bounded idle lifetime so a dead NAT binding or a
// rotated proxy-side IP is re-dialed rather than served stale, and cleanup of
// fully-idle transports.
func newUpstreamTransport(timeout time.Duration, proxyURL *url.URL) *http.Transport {
	tr := &http.Transport{
		DisableCompression: true,
		// Header timeout only: covers connect → first response bytes. Once headers
		// arrive, the stream lives on the client's request context (cancel = Esc) and
		// the per-chunk stall detector in stream.go. Applied as a transport timeout
		// (not a context) so canceling the request context later cleans up the body
		// without the timeout ever firing mid-stream.
		ResponseHeaderTimeout: timeout,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: min(timeout, 10*time.Second),
		// Warm-pool idle tuning. MaxIdleConnsPerHost caps warm sockets PER
		// ORIGIN (per upstream host through a given proxy), not per proxy —
		// one proxy fronting many upstreams keeps a warm socket per upstream,
		// which is exactly the set a rotation-heavy workload reuses. Defaults
		// (2 per host, no idle timeout) would churn TLS handshakes under any
		// concurrency; these values hold the warm set without hoarding fds.
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if proxyURL != nil {
		tr.Proxy = http.ProxyURL(proxyURL)
	}
	return tr
}

// clientFor returns the cached client for one pool entry, building it on first
// use. Clients are cached per proxy URL rather than per request so connections
// (and TLS sessions) to a given proxy are reused across requests — building a
// transport per call would leak sockets under load.
func (p *Proxy) clientFor(rawURL string) (*http.Client, error) {
	p.proxyMu.Lock()
	defer p.proxyMu.Unlock()
	if c, ok := p.proxyClients[rawURL]; ok {
		return c, nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		// url.Error's own Error() string embeds the full input URL verbatim,
		// userinfo (user:pass) included — never propagate it as-is, since the
		// caller logs this error and that's exactly the credential leak
		// proxyLabel exists to prevent. Only the underlying parse reason is safe.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("parse proxy url: %w", urlErr.Err)
		}
		return nil, fmt.Errorf("parse proxy url: invalid")
	}
	c := &http.Client{Transport: newUpstreamTransport(p.timeout, u)}
	if p.proxyClients == nil {
		p.proxyClients = map[string]*http.Client{}
	}
	p.proxyClients[rawURL] = c
	return c, nil
}

// egressEntry is one selected egress route: either the direct client or a pool
// proxy's cached client. proxyID is "" for direct; the dispatch path records
// passive health against it (see ServeHTTP).
type egressEntry struct {
	client  *http.Client
	label   string // "" = direct, else the proxy's log label
	proxyID string // "" = direct, else the pool entry ID
}

// egressFor picks the client for one upstream attempt: the next pool proxy when
// either the combo being served or the provider itself opts in, otherwise the
// direct client.
//
// combo may be nil (direct provider routing). A combo's toggle wins over its
// members': turning it on there is a statement about the whole route, and a
// provider that needs a proxy needs it however it was reached.
func (p *Proxy) egressFor(upstream *config.Provider, combo *config.Combo) egressEntry {
	if upstream == nil || (!upstream.ProxyRotate && (combo == nil || !combo.ProxyRotate)) {
		return egressEntry{client: p.client}
	}
	entry, ok := p.registry.NextProxy()
	if !ok {
		// Opted in with nothing usable to rotate through (empty pool, fully
		// disabled, or every entry cooling down). Falling back to a direct dial
		// is the lesser evil (the request still gets served, just from the
		// gateway's own IP) but it silently defeats the reason the toggle is on,
		// so say so.
		slog.Warn("proxy rotation enabled but no proxy is usable — dialing upstream directly",
			"provider", upstream.ID)
		return egressEntry{client: p.client}
	}
	client, err := p.clientFor(entry.URL)
	if err != nil {
		slog.Warn("unusable proxy in pool, dialing upstream directly",
			"provider", upstream.ID, "proxy", entry.Label, "err", err)
		return egressEntry{client: p.client}
	}
	return egressEntry{client: client, label: proxyLabel(entry), proxyID: entry.ID}
}

// egressForLog turns the label egressFor returned into something readable in a
// log line, so "which IP did this 429 come from" is answerable at a glance.
func egressForLog(label string) string {
	if label == "" {
		return "direct"
	}
	return label
}

// proxyLabel renders a pool entry for logs WITHOUT its credentials — the raw URL
// carries user:pass in userinfo and logs are read (and pasted into issues) far
// more casually than a config page.
func proxyLabel(e config.ProxyEntry) string {
	if e.Label != "" {
		return e.Label
	}
	if u, err := url.Parse(e.URL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return e.ID
}
