package registry

import (
	"sort"
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
		states:     map[string]*providerHealth{},
		cooldown:   60 * time.Second,
		errorCodes: m,
		tokenParam: map[string]string{},
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

// MarkUnsupportedCompletions flags the provider as not supporting /v1/completions.
func (h *HealthTracker) MarkUnsupportedCompletions(id string) {
	h.MarkUnsupported(id, EndpointCompletions)
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
// "providerID::accountID" so a burned key (429 / auth fail on one account) only
// takes that account out of rotation, not its siblings on the same endpoint.
func accountKey(providerID, accountID string) string {
	if accountID == "" {
		return providerID
	}
	return providerID + "::" + accountID
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

// Snapshot returns health state for all tracked providers.
func (h *HealthTracker) Snapshot() []config.HealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]config.HealthSnapshot, 0, len(h.states))
	now := time.Now()
	for id, s := range h.states {
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
