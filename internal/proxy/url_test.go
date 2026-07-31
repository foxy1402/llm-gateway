package proxy

import "testing"

func TestBuildUpstreamURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// Classic /v1 OpenAI-compatible bases
		{"https://gen.pollinations.ai/v1", "/v1/chat/completions", "https://gen.pollinations.ai/v1/chat/completions"},
		{"https://openrouter.ai/api/v1", "/v1/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://api.provider.com", "/v1/chat/completions", "https://api.provider.com/v1/chat/completions"},
		{"https://api.vivgrid.com/v1", "/v1/chat/completions", "https://api.vivgrid.com/v1/chat/completions"},
		{"https://host/llm/v1/", "/v1/embeddings", "https://host/llm/v1/embeddings"},

		// Non-/v1 versioned bases: future-proof, keep their own version, drop our /v1
		{"https://generativelanguage.googleapis.com/v1beta/openai", "/v1/chat/completions", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/v1/chat/completions", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/v1/embeddings", "https://open.bigmodel.cn/api/paas/v4/embeddings"},
		{"https://host/v2", "/v1/chat/completions", "https://host/v2/chat/completions"},
		{"https://host/v123", "/v1/chat/completions", "https://host/v123/chat/completions"},
		{"https://host/api/v2preview", "/v1/responses", "https://host/api/v2preview/responses"},
		{"https://host/v1beta", "/v1/chat/completions", "https://host/v1beta/chat/completions"},
		{"https://host/v1beta/", "/v1/chat/completions", "https://host/v1beta/chat/completions"},

		// Truly version-less bases keep the /v1 prefix
		{"https://host/custom", "/v1/chat/completions", "https://host/custom/v1/chat/completions"},
		{"https://api.no-version.example.com", "/v1/chat/completions", "https://api.no-version.example.com/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := buildUpstreamURL(c.base, c.path); got != c.want {
			t.Errorf("buildUpstreamURL(%q,%q)=%q, want %q", c.base, c.path, got, c.want)
		}
	}
}
