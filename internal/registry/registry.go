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
	wrrState   map[string][]wrrEntry
	wrrMu      sync.Mutex
}

func New() *Registry {
	return &Registry{
		providers:  map[string]*config.Provider{},
		combos:     map[string]*config.Combo{},
		health:     NewHealthTracker(),
		rrCounters: map[string]*atomic.Int64{},
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
	errCodes, _ := getSettingCSVInts(st, "health.error_codes", []int{429, 500, 502, 503, 504})
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

	// Rebuild WRR state for combos (keep existing rrCounters).
	r.wrrMu.Lock()
	newWRR := map[string][]wrrEntry{}
	for _, c := range combos {
		entries := make([]wrrEntry, 0, len(c.Members))
		for _, pid := range c.Members {
			if p, ok := provMap[pid]; ok {
				w := p.Weight
				if w < 1 {
					w = 1
				}
				entries = append(entries, wrrEntry{providerID: pid, weight: w})
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
	// Drop rotation state for combos that no longer exist so RemoveCombo doesn't
	// leave stale counters behind (#6).
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
		cp.Members = append([]string(nil), c.Members...)
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
		cp.Members = append([]string(nil), c.Members...)
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
