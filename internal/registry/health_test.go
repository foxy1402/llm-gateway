package registry

import (
	"testing"
)

func TestSupportsEndpointGeneral(t *testing.T) {
	h := NewHealthTracker()
	h.MarkUnsupportedCompletions("a")
	if h.SupportsEndpoint("a", EndpointCompletions) {
		t.Fatal("a should not support completions")
	}
	if !h.SupportsEndpoint("a", EndpointChatCompletions) {
		t.Fatal("a should still support chat.completions")
	}
	h.MarkUnsupported("b", EndpointEmbeddings)
	if h.SupportsEndpoint("b", EndpointEmbeddings) {
		t.Fatal("b should not support embeddings")
	}
}

func TestCooldownWindow(t *testing.T) {
	h := NewHealthTracker()
	h.Configure(60, []int{429})
	h.RecordFailure("x", 429)
	if h.IsAvailable("x") {
		t.Fatal("x should be cooling down")
	}
	h.RecordSuccess("x")
	if !h.IsAvailable("x") {
		t.Fatal("x should be available after success")
	}
}

func TestSnapshotSorted(t *testing.T) {
	h := NewHealthTracker()
	h.RecordFailure("zeta", 500)
	h.RecordFailure("alpha", 500)
	h.RecordFailure("mid", 500)
	snap := h.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len %d", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i].ProviderID < snap[i-1].ProviderID {
			t.Fatalf("snapshot not sorted: %v", snap)
		}
	}
}
