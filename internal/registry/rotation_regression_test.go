package registry

import (
	"context"
	"path/filepath"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/store"
)

func wrrTestRegistry(t *testing.T, providers []config.Provider, combo config.Combo) *Registry {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, p := range providers {
		if err := st.UpsertProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertCombo(combo); err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	return r
}

// Weighted round-robin must not let an INELIGIBLE member bank credit. The old
// implementation advanced every entry's current weight but only subtracted the
// total from the winner, so a member cooling down for 60s accrued hundreds of
// credits and then took every single request until the debt unwound — starving
// the member that had carried the traffic the whole time.
func TestSelectWRRDoesNotStarveAfterCooldown(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", BaseURL: "https://a.example", Model: "m", Weight: 1, Enabled: true},
		{ID: "b", BaseURL: "https://b.example", Model: "m", Weight: 1, Enabled: true},
	}
	combo := config.Combo{
		ID: "c", Rotation: config.WeightedRoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}},
	}
	r := wrrTestRegistry(t, provs, combo)

	keyA, keyB := "", ""
	for _, e := range r.wrrState["c"] {
		switch e.providerID {
		case "a":
			keyA = e.key
		case "b":
			keyB = e.key
		}
	}
	if keyA == "" || keyB == "" {
		t.Fatalf("both members must be present in WRR state: %+v", r.wrrState["c"])
	}

	// Phase 1: "a" is cooling down; every request goes to "b".
	onlyB := func(k string) bool { return k == keyB }
	for i := 0; i < 200; i++ {
		if got := r.SelectWRR("c", onlyB); got != keyB {
			t.Fatalf("phase 1 request %d selected %q, want %q", i, got, keyB)
		}
	}

	// Phase 2: "a" recovers. Over the next 20 requests both members must get
	// traffic — "a" may legitimately win a few in a row, but it must not
	// monopolize the entire window the way banked credit made it.
	both := func(string) bool { return true }
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		counts[r.SelectWRR("c", both)]++
	}
	if counts[keyB] == 0 {
		t.Fatalf("recovered member starved the healthy one for 20 straight requests: %+v", counts)
	}
	if counts[keyA] == 0 {
		t.Fatalf("recovered member never selected: %+v", counts)
	}
}

// With equal weights and both members eligible, selection must alternate — the
// baseline WRR property the starvation fix must not break.
func TestSelectWRREqualWeightsAlternate(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", BaseURL: "https://a.example", Model: "m", Weight: 1, Enabled: true},
		{ID: "b", BaseURL: "https://b.example", Model: "m", Weight: 1, Enabled: true},
	}
	combo := config.Combo{
		ID: "c", Rotation: config.WeightedRoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}},
	}
	r := wrrTestRegistry(t, provs, combo)
	both := func(string) bool { return true }
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[r.SelectWRR("c", both)]++
	}
	if len(counts) != 2 {
		t.Fatalf("want both members selected, got %+v", counts)
	}
	for k, n := range counts {
		if n < 40 || n > 60 {
			t.Errorf("member %q got %d/100 selections; equal weights should split ~50/50: %+v", k, n, counts)
		}
	}
}

// Weights must still be honored: a 3:1 provider weight ratio should produce
// roughly 3:1 selection.
func TestSelectWRRRespectsWeights(t *testing.T) {
	provs := []config.Provider{
		{ID: "heavy", BaseURL: "https://h.example", Model: "m", Weight: 3, Enabled: true},
		{ID: "light", BaseURL: "https://l.example", Model: "m", Weight: 1, Enabled: true},
	}
	combo := config.Combo{
		ID: "c", Rotation: config.WeightedRoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "heavy"}, {ProviderID: "light"}},
	}
	r := wrrTestRegistry(t, provs, combo)
	var heavyKey, lightKey string
	for _, e := range r.wrrState["c"] {
		if e.providerID == "heavy" {
			heavyKey = e.key
		} else {
			lightKey = e.key
		}
	}
	both := func(string) bool { return true }
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		counts[r.SelectWRR("c", both)]++
	}
	if counts[heavyKey] != 300 || counts[lightKey] != 100 {
		t.Errorf("3:1 weights gave %d heavy / %d light over 400 picks, want 300/100", counts[heavyKey], counts[lightKey])
	}
}

// No eligible member means no selection — not a panic, and not a stale pick that
// the caller would then dispatch to a cooling-down provider.
func TestSelectWRRNoEligibleMembers(t *testing.T) {
	provs := []config.Provider{{ID: "a", BaseURL: "https://a.example", Model: "m", Weight: 1, Enabled: true}}
	combo := config.Combo{
		ID: "c", Rotation: config.WeightedRoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "a"}},
	}
	r := wrrTestRegistry(t, provs, combo)
	if got := r.SelectWRR("c", func(string) bool { return false }); got != "" {
		t.Fatalf("SelectWRR with nothing eligible = %q, want empty", got)
	}
}

// Account-scoped health state lives in the same map as provider state, so its
// keys are namespaced. A provider literally named like an account key must not
// be able to collide with (or read) account state, and Snapshot must not surface
// synthetic account rows as if they were providers.
func TestAccountStateKeysAreNamespaced(t *testing.T) {
	h := NewHealthTracker()
	h.Configure(60, []int{500})
	h.RecordAccountFailure("p", "p:a1")

	// The provider itself is untouched by an account-scoped failure.
	if !h.IsAvailable("p") {
		t.Fatal("an account-scoped failure must not cool down the whole provider")
	}
	if h.IsAccountAvailable("p", "p:a1") {
		t.Fatal("the failed account should be cooling down")
	}
	if !h.IsAccountAvailable("p", "p:a2") {
		t.Fatal("a sibling account must stay available")
	}
	for _, snap := range h.Snapshot() {
		if snap.ProviderID != "p" {
			t.Errorf("Snapshot leaked a non-provider row %q (account state must be filtered out)", snap.ProviderID)
		}
	}
}

// Reload must clear learned "endpoint unsupported" marks: an operator who fixes
// a base URL or switches a provider to a Responses-native upstream would
// otherwise keep being routed away from that endpoint until the process
// restarted, with nothing in the UI explaining why.
func TestReloadClearsUnsupportedMarks(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertProvider(config.Provider{ID: "p", BaseURL: "https://p.example", Model: "m", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	r.Health().MarkUnsupported("p", EndpointResponses)
	if r.Health().SupportsEndpoint("p", EndpointResponses) {
		t.Fatal("MarkUnsupported did not take effect")
	}
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	if !r.Health().SupportsEndpoint("p", EndpointResponses) {
		t.Fatal("Reload must clear learned unsupported-endpoint marks so a config fix takes effect without a restart")
	}
}
