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

// TestDefaultRetryableCodesIncludes402 guards the "credits exhausted" default:
// several OpenAI-compatible upstreams (e.g. some Vercel AI Gateway providers)
// report an out-of-credit key as 402 Payment Required rather than 429, and
// that failure is exactly as account-scoped/retryable as a rate limit.
func TestDefaultRetryableCodesIncludes402(t *testing.T) {
	h := NewHealthTracker()
	for _, code := range []int{402, 429, 500, 502, 503, 504} {
		if !h.IsRetryable(code) {
			t.Fatalf("default retryable codes should include %d", code)
		}
	}
	if h.IsRetryable(200) || h.IsRetryable(404) {
		t.Fatal("200/404 must not be retryable by default")
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
