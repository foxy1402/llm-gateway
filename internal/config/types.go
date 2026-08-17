package config

// Account is one API credential under a provider. Accounts share the provider's
// base URL but may carry different keys — the gateway rotates across them so
// rate-limited keys don't take the whole provider offline.
type Account struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Label      string `json:"label"`
	AuthKey    string `json:"auth_key"`
	Enabled    bool   `json:"enabled"`
	Position   int    `json:"position"`
	Weight     int    `json:"weight"` // >1 favored by weight-aware account rotation
	// Model optionally pins this key to one upstream model (aggregator endpoints
	// like Vercel AI Gateway / OpenRouter where every key can call many models).
	// Empty = fall back to the combo member model, then the provider default.
	Model string `json:"model,omitempty"`
}

// Provider is an upstream endpoint plus the pool of accounts and known models the
// dashboard has fetched for it. AuthKey/Model are kept for legacy import/backup
// compatibility but at runtime real credentials live in Accounts and the model
// the caller uses is whatever the combo/consumer picks from Models (or the
// caller's own `model` field, which gets rewritten).
type Provider struct {
	ID              string    `json:"id"`
	Display         string    `json:"display"`
	BaseURL         string    `json:"base_url"`
	AuthKey         string    `json:"auth_key"` // legacy single-key fallback
	Model           string    `json:"model"`    // fallback + used when combos leave member model empty
	Weight          int       `json:"weight"`
	Tags            []string  `json:"tags"`
	Enabled         bool      `json:"enabled"`
	ResponsesNative bool      `json:"responses_native"`
	Accounts        []Account `json:"accounts,omitempty"`
	Models          []string  `json:"models,omitempty"`
}

type RotationPolicy string

const (
	RoundRobin         RotationPolicy = "round-robin"
	WeightedRoundRobin RotationPolicy = "weighted-round-robin"
	Priority           RotationPolicy = "priority"
	Random             RotationPolicy = "random"
)

// ComboMember binds routing to one specific upstream shape: provider + key + model.
// AccountID pins the member to one account of the provider (empty = rotate across
// all keys of that provider). Model overrides the account's own model pin; an empty
// Model means "use the pinned/rotated account's model, else the provider default".
type ComboMember struct {
	ProviderID string `json:"provider_id"`
	AccountID  string `json:"account_id"`
	Model      string `json:"model"`
}

// Combo is a virtual model ID that routes across provider members.
type Combo struct {
	ID          string         `json:"id"`
	DisplayName string         `json:"display_name"`
	Rotation    RotationPolicy `json:"rotation"`
	Members     []ComboMember  `json:"members"` // provider+model in position order
	Enabled     bool           `json:"enabled"`
}

type LogEntry struct {
	ID               int64  `json:"id"`
	Timestamp        int64  `json:"ts"`
	ModelIn          string `json:"model_in"`           // what caller sent (combo id or provider id)
	ProviderUsed     string `json:"provider_used"`      // actual upstream provider id
	Endpoint         string `json:"endpoint"`           // chat.completions | completions | responses | embeddings
	Status           int    `json:"status"`
	LatencyMs        int64  `json:"latency_ms"`
	PromptTokens     *int   `json:"prompt_tokens,omitempty"`
	CompletionTokens *int   `json:"completion_tokens,omitempty"`
	CachedTokens     *int   `json:"cached_tokens,omitempty"` // prompt tokens served from upstream KV cache
	Error            string `json:"error,omitempty"`
	UpstreamURL      string `json:"upstream_url,omitempty"`
	RequestPayload   string `json:"request_payload,omitempty"`
	ResponseSnippet  string `json:"response_snippet,omitempty"`
}

type LogFilter struct {
	ProviderID string
	Endpoint   string
	ErrorsOnly bool
	Since      int64 // unix epoch, inclusive
	Until      int64 // unix epoch, exclusive
	Limit      int
	Offset     int
}

// HealthSnapshot is a read-only view of per-provider health for admin/status.
type HealthSnapshot struct {
	ProviderID            string `json:"provider_id"`
	Failures              int    `json:"failures"`
	Available             bool   `json:"available"`
	CooldownRemainingMs   int64  `json:"cooldown_remaining_ms"`
	UnsupportedCompletion bool   `json:"unsupported_completions"`
}
