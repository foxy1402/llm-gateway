package registry

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"llm-gateway/internal/config"
	"llm-gateway/internal/store"
)

const (
	EndpointChatCompletions = "chat.completions"
	EndpointCompletions     = "completions"
	EndpointResponses       = "responses"
	EndpointEmbeddings      = "embeddings"
)

type wrrEntry struct {
	providerID string
	weight     int
	current    int
}

type Registry struct {
	mu        sync.RWMutex
	providers map[string]*config.Provider
	combos    map[string]*config.Combo

	health *HealthTracker

	// rotation state
	rrCounters map[string]*atomic.Int64
	acctRR     map[string]*atomic.Int64 // per-provider account round-robin pointers
	wrrState   map[string][]wrrEntry
	wrrMu      sync.Mutex
}

func New() *Registry {
	return &Registry{
		providers:  map[string]*config.Provider{},
		combos:     map[string]*config.Combo{},
		health:     NewHealthTracker(),
		rrCounters: map[string]*atomic.Int64{},
		acctRR:     map[string]*atomic.Int64{},
		wrrState:   map[string][]wrrEntry{},
	}
}

// Reload replaces the in-memory snapshot from the store.
// Rotation counters are preserved across reloads so dashboards edits don't reset position.
func (r *Registry) Reload(st *store.Store) error {
	provs, err := st.ListProviders()
	if err != nil {
		return err
	}
	combos, err := st.ListCombos()
	if err != nil {
		return err
	}
	// Load health settings.
	cooldownSec, _ := getSettingInt(st, "health.cooldown", 60)
	errCodes, _ := getSettingCSVInts(st, "health.error_codes", defaultRetryableCodesList())
	r.health.Configure(cooldownSec, errCodes)

	provMap := map[string]*config.Provider{}
	for i := range provs {
		p := provs[i]
		provMap[p.ID] = &p
	}
	comboMap := map[string]*config.Combo{}
	for i := range combos {
		c := combos[i]
		comboMap[c.ID] = &c
	}

	r.mu.Lock()
	r.providers = provMap
	r.combos = comboMap
	r.mu.Unlock()

	// Rebuild WRR state for combos (keep existing rrCounters). Also rebuild the
	// per-provider account rotator state so a provider's key set changes are picked up.
	r.wrrMu.Lock()
	newWRR := map[string][]wrrEntry{}
	for _, c := range combos {
		entries := make([]wrrEntry, 0, len(c.Members))
		for _, m := range c.Members {
			if p, ok := provMap[m.ProviderID]; ok {
				w := p.Weight
				if w < 1 {
					w = 1
				}
				entries = append(entries, wrrEntry{providerID: m.ProviderID, weight: w})
			}
		}
		// Preserve current weights if the member set is unchanged.
		if old, ok := r.wrrState[c.ID]; ok && sameMembers(old, entries) {
			newWRR[c.ID] = old
		} else {
			newWRR[c.ID] = entries
		}
		if _, ok := r.rrCounters[c.ID]; !ok {
			r.rrCounters[c.ID] = &atomic.Int64{}
		}
	}
	r.wrrState = newWRR
	// Per-provider round-robin account pointers: reset where account set changed,
	// preserve where identical so rotation isn't reset on every dashboard save.
	newAcctRR := map[string]*atomic.Int64{}
	for _, p := range provs {
		if len(p.Accounts) > 0 {
			if ctr, ok := r.acctRR[p.ID]; ok {
				newAcctRR[p.ID] = ctr
			} else {
				newAcctRR[p.ID] = &atomic.Int64{}
			}
		}
	}
	r.acctRR = newAcctRR
	// Drop rotation state for combos/accounts that no longer exist so stale
	// counters don't linger when a config is removed (#6).
	live := map[string]bool{}
	for _, c := range combos {
		live[c.ID] = true
	}
	for id := range r.rrCounters {
		if !live[id] {
			delete(r.rrCounters, id)
		}
	}
	r.wrrMu.Unlock()
	return nil
}

func sameMembers(a, b []wrrEntry) bool {
	if len(a) != len(b) {
		return false
	}
	ka := map[string]int{}
	for _, e := range a {
		ka[e.providerID] = e.weight
	}
	for _, e := range b {
		if ka[e.providerID] != e.weight {
			return false
		}
	}
	return true
}

func (r *Registry) GetProvider(id string) *config.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.providers[id]; ok {
		cp := *p
		return &cp
	}
	return nil
}

func (r *Registry) GetCombo(id string) *config.Combo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.combos[id]; ok {
		cp := *c
		cp.Members = append([]config.ComboMember(nil), c.Members...)
		return &cp
	}
	return nil
}

// ListEnabledCombos returns enabled combos sorted by ID.
func (r *Registry) ListEnabledCombos() []*config.Combo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*config.Combo{}
	for _, c := range r.combos {
		if c.Enabled {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListEnabledProviders returns enabled providers sorted by ID.
func (r *Registry) ListEnabledProviders() []*config.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*config.Provider{}
	for _, p := range r.providers {
		if p.Enabled {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListAllProviders returns every provider (for the dashboard).
func (r *Registry) ListAllProviders() []*config.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*config.Provider{}
	for _, p := range r.providers {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) ListAllCombos() []*config.Combo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*config.Combo{}
	for _, c := range r.combos {
		cp := *c
		cp.Members = append([]config.ComboMember(nil), c.Members...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Health returns the shared tracker so proxy/admin can record/observe.
func (r *Registry) Health() *HealthTracker { return r.health }

func (r *Registry) IncrementRR(comboID string) int64 {
	// Guarded by wrrMu: Reload also mutates rrCounters under the same lock. The
	// atomic.Int64 once fetched doesn't need the lock to increment, but the map
	// lookup/insert absolutely does (#6).
	r.wrrMu.Lock()
	defer r.wrrMu.Unlock()
	if ctr, ok := r.rrCounters[comboID]; ok {
		return ctr.Add(1)
	}
	ctr := &atomic.Int64{}
	r.rrCounters[comboID] = ctr
	return ctr.Add(1)
}

// NextAccount picks the next account for a provider using weight-aware round-robin
// across enabled accounts, skipping the ones the caller marks `skip` (already
// tried this request) and the ones currently in proactive cooldown.
//
// Returns ok=false only when every enabled account is ineligible for this call —
// the proxy then treats it as "provider down for this attempt" and rotates to the
// next combo member.
//
// Weight handling: account.Weight influences only the *likely ordering* — each
// weight class gets its own internal round-robin so a weight=3 account hops
// roughly 3 × as often as a weight=1 sibling. No cross-provider guessing.
func (r *Registry) NextAccount(p *config.Provider, health *HealthTracker, skip map[string]bool) (config.Account, bool) {
	// Legacy fallback — single-key providers have no account rows.
	if len(p.Accounts) == 0 {
		legacy := config.Account{
			ID: p.ID + ":default", ProviderID: p.ID, Label: "default",
			AuthKey: p.AuthKey, Enabled: true, Weight: 1,
		}
		if skip[legacy.ID] || !health.IsAccountAvailable(p.ID, legacy.ID) {
			return config.Account{}, false
		}
		return legacy, true
	}

	r.wrrMu.Lock()
	ctr, ok := r.acctRR[p.ID]
	if !ok {
		ctr = &atomic.Int64{}
		if r.acctRR == nil {
			r.acctRR = map[string]*atomic.Int64{}
		}
		r.acctRR[p.ID] = ctr
	}
	r.wrrMu.Unlock()
	n := int(ctr.Add(1))

	eligible := []config.Account{}
	for _, a := range p.Accounts {
		if !a.Enabled {
			continue
		}
		if skip[a.ID] {
			continue
		}
		if !health.IsAccountAvailable(p.ID, a.ID) {
			continue
		}
		eligible = append(eligible, a)
	}
	if len(eligible) == 0 {
		return config.Account{}, false
	}
	// Build an expanded pick list so weight >1 gets multiple slots — number of
	// slots equals the weight. Slot order is stable per provider (account order).
	picks := []config.Account{}
	for _, a := range eligible {
		w := a.Weight
		if w < 1 {
			w = 1
		}
		for i := 0; i < w; i++ {
			picks = append(picks, a)
		}
	}
	return picks[n%len(picks)], true
}

// PinnedAccount resolves an explicit account pin (combo member → account ID) with
// the same eligibility rules as NextAccount: the account must exist, be enabled,
// not already tried this request, and not be in cooldown. ok=false means the
// Member can't be served right now — the caller should rotate to the next member.
func (r *Registry) PinnedAccount(p *config.Provider, accountID string, health *HealthTracker, skip map[string]bool) (config.Account, bool) {
	for _, a := range p.Accounts {
		if a.ID != accountID {
			continue
		}
		if !a.Enabled || skip[a.ID] || !health.IsAccountAvailable(p.ID, a.ID) {
			return config.Account{}, false
		}
		return a, true
	}
	return config.Account{}, false
}

// AccountsForLog renders the set of enabled accounts for diagnostics.
func (r *Registry) AccountsForLog(providerID string) []string {
	p := r.GetProvider(providerID)
	if p == nil {
		return nil
	}
	out := []string{}
	for _, a := range p.Accounts {
		if a.Enabled {
			out = append(out, a.ID)
		}
	}
	return out
}

// SelectWRR runs smooth weighted round-robin over the combo's members.
// Returns "" if no member is eligible.
func (r *Registry) SelectWRR(comboID string, eligible func(pid string) bool) string {
	r.wrrMu.Lock()
	defer r.wrrMu.Unlock()
	entries, ok := r.wrrState[comboID]
	if !ok || len(entries) == 0 {
		return ""
	}
	total := 0
	for _, e := range entries {
		total += e.weight
	}
	best := -1
	for i := range entries {
		entries[i].current += entries[i].weight
		if eligible(entries[i].providerID) {
			if best == -1 || entries[i].current > entries[best].current {
				best = i
			}
		}
	}
	if best == -1 {
		return ""
	}
	entries[best].current -= total
	return entries[best].providerID
}

// --- settings helpers ---

func getSettingInt(st *store.Store, key string, fallback int) (int, error) {
	v, err := st.GetSetting(key)
	if err != nil || v == "" {
		return fallback, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback, err
	}
	return n, nil
}

func getSettingCSVInts(st *store.Store, key string, fallback []int) ([]int, error) {
	v, err := st.GetSetting(key)
	if err != nil || v == "" {
		return fallback, err
	}
	out := []int{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return fallback, nil
	}
	return out, nil
}
