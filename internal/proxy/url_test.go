package proxy

import "testing"

func TestBuildUpstreamURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://gen.pollinations.ai/v1", "/v1/chat/completions", "https://gen.pollinations.ai/v1/chat/completions"},
		{"https://openrouter.ai/api/v1", "/v1/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://api.provider.com", "/v1/chat/completions", "https://api.provider.com/v1/chat/completions"},
		{"https://api.vivgrid.com/v1", "/v1/chat/completions", "https://api.vivgrid.com/v1/chat/completions"},
		{"https://host/llm/v1/", "/v1/embeddings", "https://host/llm/v1/embeddings"},
		{"https://host/v1beta", "/v1/chat/completions", "https://host/v1beta/v1/chat/completions"},
		{"https://host/custom", "/v1/chat/completions", "https://host/custom/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := buildUpstreamURL(c.base, c.path); got != c.want {
			t.Errorf("buildUpstreamURL(%q,%q)=%q, want %q", c.base, c.path, got, c.want)
		}
	}
}
