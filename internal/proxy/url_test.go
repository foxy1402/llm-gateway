package proxy

import "testing"

// The rule is pure concatenation: base is the full OpenAI-compatible root
// (including its version), and the canonical endpoint path is appended as-is.
func TestBuildUpstreamURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// OpenAI-compatible /v1 roots
		{"https://gen.pollinations.ai/v1", "/chat/completions", "https://gen.pollinations.ai/v1/chat/completions"},
		{"https://openrouter.ai/api/v1", "/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://api.groq.com/openai/v1", "/chat/completions", "https://api.groq.com/openai/v1/chat/completions"},
		{"https://host/llm/v1/", "/embeddings", "https://host/llm/v1/embeddings"},

		// Non-/v1 versioned roots: no regex needed, always the user's version + endpoint
		{"https://generativelanguage.googleapis.com/v1beta/openai", "/chat/completions", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"https://generativelanguage.googleapis.com/v1beta", "/chat/completions", "https://generativelanguage.googleapis.com/v1beta/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/chat/completions", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/embeddings", "https://open.bigmodel.cn/api/paas/v4/embeddings"},
		{"https://open.bigmodel.cn/api/paas/v4", "/responses", "https://open.bigmodel.cn/api/paas/v4/responses"},
		{"https://open.bigmodel.cn/api/paas/v4", "/completions", "https://open.bigmodel.cn/api/paas/v4/completions"},
		{"https://host/v123", "/chat/completions", "https://host/v123/chat/completions"},
		{"https://host/api/v2preview", "/chat/completions", "https://host/api/v2preview/chat/completions"},

		// Version-less custom roots keep the exact custom path
		{"https://host/custom", "/chat/completions", "https://host/custom/chat/completions"},
		{"https://api.no-version.example.com", "/chat/completions", "https://api.no-version.example.com/chat/completions"},

		// Trailing slash on the base is normalized away
		{"https://host/api/v1/", "/chat/completions", "https://host/api/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := buildUpstreamURL(c.base, c.path); got != c.want {
			t.Errorf("buildUpstreamURL(%q,%q)=%q, want %q", c.base, c.path, got, c.want)
		}
	}
}
