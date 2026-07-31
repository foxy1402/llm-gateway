package config

type Provider struct {
	ID              string   `json:"id"`
	Display         string   `json:"display"`
	BaseURL         string   `json:"base_url"`
	AuthKey         string   `json:"auth_key"`
	Model           string   `json:"model"`
	Weight          int      `json:"weight"`
	Tags            []string `json:"tags"`
	Enabled         bool     `json:"enabled"`
	ResponsesNative bool     `json:"responses_native"`
}

type RotationPolicy string

const (
	RoundRobin         RotationPolicy = "round-robin"
	WeightedRoundRobin RotationPolicy = "weighted-round-robin"
	Priority           RotationPolicy = "priority"
	Random             RotationPolicy = "random"
)

type Combo struct {
	ID          string         `json:"id"`
	DisplayName string         `json:"display_name"`
	Rotation    RotationPolicy `json:"rotation"`
	Members     []string       `json:"members"` // provider IDs in position order
	Enabled     bool           `json:"enabled"`
}

type LogEntry struct {
	ID               int64  `json:"id"`
	Timestamp        int64  `json:"ts"`
	ModelIn          string `json:"model_in"`      // what caller sent (combo id or provider id)
	ProviderUsed     string `json:"provider_used"` // actual upstream provider id
	Endpoint         string `json:"endpoint"`      // chat.completions | completions | responses | embeddings
	Status           int    `json:"status"`
	LatencyMs        int64  `json:"latency_ms"`
	PromptTokens     *int   `json:"prompt_tokens,omitempty"`
	CompletionTokens *int   `json:"completion_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
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
