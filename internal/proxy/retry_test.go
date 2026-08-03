package proxy

import (
	"context"
	"path/filepath"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

// setupRotationEnv builds a registry with the given providers and a combo.
func setupRotationEnv(t *testing.T, provs []config.Provider, combo config.Combo) (*Proxy, *registry.Registry) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, p := range provs {
		if err := st.UpsertProvider(p); err != nil {
			t.Fatalf("upsert provider: %v", err)
		}
	}
	if err := st.UpsertCombo(combo); err != nil {
		t.Fatalf("upsert combo: %v", err)
	}
	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatalf("reload: %v", err)
	}
	px := New(reg, st, 0)
	return px, reg
}

func planFor(px *Proxy, combo *config.Combo, endpoint string) *rotationPlan {
	return px.newRotationPlan(combo, endpoint)
}

// pidOf reads the provider ID a plan selection produced ("" for nil).
func pidOf(m *config.ComboMember) string {
	if m == nil {
		return ""
	}
	return m.ProviderID
}

func mems(ids ...string) []config.ComboMember {
	out := make([]config.ComboMember, len(ids))
	for i, id := range ids {
		out[i] = config.ComboMember{ProviderID: id}
	}
	return out
}

func TestRoundRobin(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
		{ID: "c", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "r", Rotation: config.RoundRobin, Members: mems("a", "b", "c"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		counts[pidOf(plan.next(reg, map[string]bool{}))]++
	}
	if counts["a"] != 10 || counts["b"] != 10 || counts["c"] != 10 {
		t.Fatalf("round robin counts: %v", counts)
	}
}

func TestPriority(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "p", Rotation: config.Priority, Members: mems("a", "b"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	for i := 0; i < 5; i++ {
		if pid := pidOf(plan.next(reg, map[string]bool{})); pid != "a" {
			t.Fatalf("priority: got %q", pid)
		}
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	provs := []config.Provider{
		{ID: "heavy", Enabled: true, Weight: 2},
		{ID: "light", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "w", Rotation: config.WeightedRoundRobin, Members: mems("heavy", "light"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		counts[pidOf(plan.next(reg, map[string]bool{}))]++
	}
	if counts["heavy"] != 40 || counts["light"] != 20 {
		t.Fatalf("WRR counts: %v", counts)
	}
}

func TestCooldown(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "p", Rotation: config.Priority, Members: mems("a", "b"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	reg.Health().Configure(60, []int{429})
	reg.Health().RecordFailure("a", 429)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	if pid := pidOf(plan.next(reg, map[string]bool{})); pid != "b" {
		t.Fatalf("expected b while a is cooling down, got %q", pid)
	}
}

func TestRandomCoversAll(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
		{ID: "c", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "r", Rotation: config.Random, Members: mems("a", "b", "c"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[pidOf(plan.next(reg, map[string]bool{}))] = true
	}
	if len(seen) != 3 {
		t.Fatalf("random never selected all providers: %v", seen)
	}
}

// #10: random rotation must respect provider weights.
func TestRandomRespectsWeights(t *testing.T) {
	provs := []config.Provider{
		{ID: "heavy", Enabled: true, Weight: 9},
		{ID: "light", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "r", Rotation: config.Random, Members: mems("heavy", "light"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)
	counts := map[string]int{}
	const trials = 5000
	for i := 0; i < trials; i++ {
		counts[pidOf(plan.next(reg, map[string]bool{}))]++
	}
	if counts["heavy"] < int(0.75*trials) {
		t.Fatalf("heavy weight=9 got only %d/%d picks; weights ignored", counts["heavy"], trials)
	}
	if counts["light"] > int(0.25*trials) {
		t.Fatalf("light weight=1 got %d/%d picks; expected ~10%%", counts["light"], trials)
	}
}

func TestTriedSetExclusion(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "p", Rotation: config.Priority, Members: mems("a", "b"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)
	plan := planFor(px, &combo, registry.EndpointChatCompletions)

	tried := map[string]bool{"a": true}
	if pid := pidOf(plan.next(reg, tried)); pid != "b" {
		t.Fatalf("expected b after a tried, got %q", pid)
	}
	tried["b"] = true
	if m := plan.next(reg, tried); m != nil {
		t.Fatalf("expected exhausted, got %q", m.ProviderID)
	}
}

func TestUnsupportedEndpointSkipped(t *testing.T) {
	provs := []config.Provider{
		{ID: "a", Enabled: true, Weight: 1},
		{ID: "b", Enabled: true, Weight: 1},
	}
	combo := config.Combo{ID: "p", Rotation: config.Priority, Members: mems("a", "b"), Enabled: true}
	px, reg := setupRotationEnv(t, provs, combo)

	// a doesn't support completions.
	reg.Health().MarkUnsupportedCompletions("a")

	plan := planFor(px, &combo, registry.EndpointCompletions)
	if pid := pidOf(plan.next(reg, map[string]bool{})); pid != "b" {
		t.Fatalf("expected b (a unsupported), got %q", pid)
	}
	planChat := planFor(px, &combo, registry.EndpointChatCompletions)
	if pid := pidOf(planChat.next(reg, map[string]bool{})); pid != "a" {
		t.Fatalf("expected a for chat.completions, got %q", pid)
	}
}
