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
}

func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		states:     map[string]*providerHealth{},
		cooldown:   60 * time.Second,
		errorCodes: map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true},
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
