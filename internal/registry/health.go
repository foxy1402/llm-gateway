package registry

import (
	"sort"
	"strings"
	"sync"
	"time"

	"llm-gateway/internal/config"
)

type providerHealth struct {
	mu            sync.Mutex
	failures      int
	cooldownUntil time.Time
	// unsupported is a per-endpoint set so we can skip endpoints a provider lacks
	// (completions, embeddings, etc.) rather than only handling completions (#5).
	unsupported map[string]bool
}

// HealthTracker tracks per-provider failure state and cooldown windows.
type HealthTracker struct {
	mu         sync.Mutex
	states     map[string]*providerHealth
	cooldown   time.Duration
	errorCodes map[int]bool
	codeMu     sync.RWMutex

	// tokenParam caches, per account, which max_tokens/max_completion_tokens
	// field name was proven to work by a live smart-mode auto-heal (see
	// proxy.tryTokenParamHeal). This is in-memory only and reset on restart —
	// it's a pure latency optimization (skip the known-failing first attempt),
	// never a correctness requirement, since the reactive heal is still checked
	// on every response regardless.
	tokenParamMu sync.RWMutex
	tokenParam   map[string]string

	// reasoningEffortNone caches, per account, that requests carrying a "tools"
	// array need reasoning_effort forced to "none" for this account's model to
	// accept them (see proxy.tryReasoningEffortHeal). Same in-memory,
	// reset-on-restart, latency-only nature as tokenParam above.
	reasoningEffortMu sync.RWMutex
	reasoningEffort   map[string]bool
}

// defaultRetryableCodes: 429 (rate limited) and 5xx (upstream trouble) always
// warrant rotation. 402 is included too — several OpenAI-compatible gateways
// (e.g. some Vercel AI Gateway upstreams) report an exhausted-credits key as
// "402 Payment Required" instead of 429, and that failure mode is exactly as
// account-scoped and retryable as a rate limit: a sibling key on the same
// provider is very likely to still have credit.
var defaultRetryableCodes = map[int]bool{402: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// defaultRetryableCodesList renders defaultRetryableCodes as a slice for
// callers (Reload's fallback) that need the "no setting stored yet" default
// in list form. Kept in one place so the two defaults can't drift apart.
func defaultRetryableCodesList() []int {
	out := make([]int, 0, len(defaultRetryableCodes))
	for c := range defaultRetryableCodes {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

func NewHealthTracker() *HealthTracker {
	m := make(map[int]bool, len(defaultRetryableCodes))
	for k, v := range defaultRetryableCodes {
		m[k] = v
	}
	return &HealthTracker{
		states:          map[string]*providerHealth{},
		cooldown:        60 * time.Second,
		errorCodes:      m,
		tokenParam:      map[string]string{},
		reasoningEffort: map[string]bool{},
	}
}

// Configure updates cooldown duration and retryable error codes from settings.
func (h *HealthTracker) Configure(cooldownSeconds int, errorCodes []int) {
	h.codeMu.Lock()
	defer h.codeMu.Unlock()
	if cooldownSeconds > 0 {
		h.cooldown = time.Duration(cooldownSeconds) * time.Second
	}
	if len(errorCodes) > 0 {
		m := map[int]bool{}
		for _, c := range errorCodes {
			m[c] = true
		}
		h.errorCodes = m
	}
}

// IsRetryable reports whether an HTTP status code should trigger rotation.
func (h *HealthTracker) IsRetryable(code int) bool {
	h.codeMu.RLock()
	defer h.codeMu.RUnlock()
	return h.errorCodes[code]
}

func (h *HealthTracker) state(id string) *providerHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.states[id]
	if !ok {
		s = &providerHealth{}
		h.states[id] = s
	}
	return s
}

// RecordFailure increments the failure counter and starts a cooldown window.
func (h *HealthTracker) RecordFailure(id string, code int) {
	s := h.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.cooldownUntil = time.Now().Add(h.currentCooldown())
}

func (h *HealthTracker) currentCooldown() time.Duration {
	h.codeMu.RLock()
	defer h.codeMu.RUnlock()
	return h.cooldown
}

// RecordSuccess resets the failure counter and clears cooldown.
func (h *HealthTracker) RecordSuccess(id string) {
	s := h.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	s.cooldownUntil = time.Time{}
}

// MarkUnsupported flags the provider as not supporting an arbitrary endpoint (#5).
func (h *HealthTracker) MarkUnsupported(id, endpoint string) {
	s := h.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsupported == nil {
		s.unsupported = map[string]bool{}
	}
	s.unsupported[endpoint] = true
}

// IsAvailable reports whether the provider is out of cooldown.
func (h *HealthTracker) IsAvailable(id string) bool {
	s := h.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.cooldownUntil) || s.cooldownUntil.IsZero()
}

// accountKey builds the cooldown-scoping key for a single account. Accounts use
// "account::providerID::accountID" so a burned key (429 / auth fail on one
// account) only takes that account out of rotation, not its siblings on the same
// endpoint. The "account::" namespace matters because state(id) CREATES an entry
// on first read, so merely considering a key for rotation registers it in the
// shared states map — without a prefix those entries were indistinguishable from
// providers and every account rendered as a phantom "Disabled provider" row in
// the dashboard health table.
func accountKey(providerID, accountID string) string {
	if accountID == "" {
		return providerID
	}
	return accountStatePrefix + providerID + "::" + accountID
}

// accountStatePrefix namespaces per-account health state inside the shared
// states map, mirroring proxyStateKey's "proxy::".
const accountStatePrefix = "account::"

// isAccountStateKey reports whether a states-map key belongs to a single account
// rather than a provider.
func isAccountStateKey(k string) bool {
	return strings.HasPrefix(k, accountStatePrefix)
}

// RecordAccountFailure cools down a single account key. Subsequent candidates
// for this provider skip it until the cooldown elapses.
func (h *HealthTracker) RecordAccountFailure(providerID, accountID string) {
	h.RecordFailure(accountKey(providerID, accountID), 0)
}

// RecordAccountSuccess clears cooldown/failure state for a single account.
func (h *HealthTracker) RecordAccountSuccess(providerID, accountID string) {
	h.RecordSuccess(accountKey(providerID, accountID))
}

// IsAccountAvailable reports whether a single account is currently eligible.
func (h *HealthTracker) IsAccountAvailable(providerID, accountID string) bool {
	return h.IsAvailable(accountKey(providerID, accountID))
}

// LearnTokenParam records that fieldName ("max_tokens" or
// "max_completion_tokens") is the one this account's model actually accepts,
// discovered live by a smart-mode auto-heal. Future requests for the same
// account apply it proactively instead of re-discovering it every time.
func (h *HealthTracker) LearnTokenParam(providerID, accountID, fieldName string) {
	h.tokenParamMu.Lock()
	defer h.tokenParamMu.Unlock()
	if h.tokenParam == nil {
		h.tokenParam = map[string]string{}
	}
	h.tokenParam[accountKey(providerID, accountID)] = fieldName
}

// LearnedTokenParam returns the previously learned field name for an account,
// or "" if nothing has been learned yet (the common case: most models accept
// whichever field the client sent, so there's nothing to remember).
func (h *HealthTracker) LearnedTokenParam(providerID, accountID string) string {
	h.tokenParamMu.RLock()
	defer h.tokenParamMu.RUnlock()
	return h.tokenParam[accountKey(providerID, accountID)]
}

// LearnReasoningEffortNone records that this account's model rejects tool
// calls unless reasoning_effort is explicitly "none", discovered live by
// tryReasoningEffortHeal. Future requests-with-tools for the same account
// apply it proactively instead of paying the failing round trip every time.
func (h *HealthTracker) LearnReasoningEffortNone(providerID, accountID string) {
	h.reasoningEffortMu.Lock()
	defer h.reasoningEffortMu.Unlock()
	if h.reasoningEffort == nil {
		h.reasoningEffort = map[string]bool{}
	}
	h.reasoningEffort[accountKey(providerID, accountID)] = true
}

// LearnedReasoningEffortNone reports whether this account was previously
// learned to need reasoning_effort: "none" forced on tool-call requests.
func (h *HealthTracker) LearnedReasoningEffortNone(providerID, accountID string) bool {
	h.reasoningEffortMu.RLock()
	defer h.reasoningEffortMu.RUnlock()
	return h.reasoningEffort[accountKey(providerID, accountID)]
}

// ForgetReasoningEffortNone drops a learned reasoning_effort pin — the
// un-learn path that keeps the cache self-correcting: when a tool request
// succeeds WITHOUT the forced field, the model no longer needs it and the
// stale pin (which degrades reasoning quality) must go. Token-param needs no
// such method: its reactive heal re-verifies the symmetric swap on every
// error, so a wrong value flips itself.
func (h *HealthTracker) ForgetReasoningEffortNone(providerID, accountID string) {
	h.reasoningEffortMu.Lock()
	defer h.reasoningEffortMu.Unlock()
	delete(h.reasoningEffort, accountKey(providerID, accountID))
}

// PruneLearnedAccounts drops learned-cache entries whose (provider, account)
// no longer exists, mirroring Reload's rrCounter pruning so dashboard churn
// (delete/rename) doesn't grow the maps without bound. accountKeys is the set
// of live keys in accountKey(providerID, accountID) form.
func (h *HealthTracker) PruneLearnedAccounts(accountKeys map[string]bool) {
	h.tokenParamMu.Lock()
	for k := range h.tokenParam {
		if !accountKeys[k] {
			delete(h.tokenParam, k)
		}
	}
	h.tokenParamMu.Unlock()
	h.reasoningEffortMu.Lock()
	for k := range h.reasoningEffort {
		if !accountKeys[k] {
			delete(h.reasoningEffort, k)
		}
	}
	h.reasoningEffortMu.Unlock()
	// Per-account cooldown state lives in the shared states map and is created on
	// first read, so it needs the same pruning or a long-lived process accumulates
	// an entry for every account that ever existed.
	h.mu.Lock()
	for k := range h.states {
		if isAccountStateKey(k) && !accountKeys[k] {
			delete(h.states, k)
		}
	}
	h.mu.Unlock()
}

// ClearUnsupported forgets every learned "provider does not implement this
// endpoint" mark. Called from Reload because those marks have no TTL and nothing
// else ever clears them: a single 404 (often from a mistyped base_url or a wrong
// responses_native flag) permanently removed the provider from rotation for that
// endpoint, and correcting the configuration in the dashboard could not recover
// it — only a process restart could. A reload is the operator stating the config
// changed, so re-discovery is exactly the right behavior.
func (h *HealthTracker) ClearUnsupported() {
	h.mu.Lock()
	states := make([]*providerHealth, 0, len(h.states))
	for _, s := range h.states {
		states = append(states, s)
	}
	h.mu.Unlock()
	for _, s := range states {
		s.mu.Lock()
		s.unsupported = nil
		s.mu.Unlock()
	}
}

// proxyStateKey namespaces per-proxy health inside the shared states map.
// Provider IDs are user-chosen and could theoretically collide with a proxy
// ID ("px-..."), so the prefix keeps the two namespaces disjoint while reusing
// the exact same cooldown machinery (and therefore the same configurable
// health.cooldown window) as provider health.
func proxyStateKey(id string) string { return "proxy::" + id }

// RecordProxyFailure cools down a single pool entry. Called ONLY on
// transport-level dispatch errors through that proxy (dial/connect refused,
// timeout) — an HTTP status, even a retryable 429/5xx, proves the proxy
// successfully forwarded the request and says nothing about proxy liveness,
// so it must NOT penalize the proxy (only the account, as before).
func (h *HealthTracker) RecordProxyFailure(id string) {
	h.RecordFailure(proxyStateKey(id), 0)
}

// RecordProxySuccess clears a proxy's cooldown/failure state. Called whenever
// ANY HTTP response (even a 429/5xx) arrives through that proxy — receiving a
// response proves the TCP/TLS path to and through the proxy is alive.
func (h *HealthTracker) RecordProxySuccess(id string) {
	h.RecordSuccess(proxyStateKey(id))
}

// IsProxyAvailable reports whether a pool entry is currently eligible (out of
// cooldown). Unknown proxies (never tried) report true WITHOUT creating state —
// no data means no reason to skip, and the dashboard's "Unknown" verdict must
// survive until the first real attempt.
func (h *HealthTracker) IsProxyAvailable(id string) bool {
	h.mu.Lock()
	s, ok := h.states[proxyStateKey(id)]
	h.mu.Unlock()
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.cooldownUntil) || s.cooldownUntil.IsZero()
}

// ProxyStatus returns the dashboard-facing health of one pool entry. seen is
// false when the proxy has never been tried since restart — the dashboard
// renders that as "Unknown" rather than "Alive", so a freshly added proxy
// isn't misread as proven.
func (h *HealthTracker) ProxyStatus(id string) (failures int, available bool, cooldownMs int64, seen bool) {
	h.mu.Lock()
	s, ok := h.states[proxyStateKey(id)]
	h.mu.Unlock()
	if !ok {
		return 0, true, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	avail := now.After(s.cooldownUntil) || s.cooldownUntil.IsZero()
	var ms int64
	if !avail {
		ms = s.cooldownUntil.Sub(now).Milliseconds()
	}
	return s.failures, avail, ms, true
}

// PruneProxies drops health state for pool entries that no longer exist,
// mirroring PruneLearnedAccounts so dashboard churn (delete/rename) doesn't
// grow the states map without bound. liveIDs is the set of current proxy IDs.
func (h *HealthTracker) PruneProxies(liveIDs map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.states {
		if id, ok := disentangleProxyKey(k); ok {
			if !liveIDs[id] {
				delete(h.states, k)
			}
		}
	}
}

func disentangleProxyKey(k string) (string, bool) {
	if id, ok := strings.CutPrefix(k, "proxy::"); ok {
		return id, true
	}
	return "", false
}

// SupportsEndpoint reports whether the provider can handle the given endpoint.
func (h *HealthTracker) SupportsEndpoint(id, endpoint string) bool {
	s := h.state(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unsupported[endpoint] {
		return false
	}
	// Providers without native responses still accept /v1/responses via translation.
	return true
}

// Snapshot returns health state for all tracked providers. Proxy pool entries and
// individual accounts share the same underlying states map (namespaced via
// proxyStateKey / accountKey so their cooldowns reuse the provider machinery) but
// are NOT providers — every consumer of this snapshot (dashboard "Provider
// health" table, /admin/status) looks each ProviderID up against the provider
// list, so a leaked "proxy::px-1" or "account::p1::p1:k2" key would render as a
// bogus disabled "provider". Proxy health has its own accessor (ProxyStatus) for
// the dedicated Proxies dashboard table.
func (h *HealthTracker) Snapshot() []config.HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]config.HealthSnapshot, 0, len(h.states))
	now := time.Now()
	for id, s := range h.states {
		if _, isProxy := disentangleProxyKey(id); isProxy {
			continue
		}
		if isAccountStateKey(id) {
			continue
		}
		s.mu.Lock()
		snap := config.HealthSnapshot{
			ProviderID:            id,
			Failures:              s.failures,
			Available:             now.After(s.cooldownUntil) || s.cooldownUntil.IsZero(),
			UnsupportedCompletion: s.unsupported[EndpointCompletions],
		}
		if !snap.Available {
			snap.CooldownRemainingMs = s.cooldownUntil.Sub(now).Milliseconds()
		}
		s.mu.Unlock()
		out = append(out, snap)
	}
	// Stable order, O(N log N).
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}
