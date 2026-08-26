package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// resolveReasoningEffortNone reports whether this account was previously
// LEARNED (via a live tryReasoningEffortHeal) to need reasoning_effort forced
// to "none" whenever the request carries a "tools" array. Mirrors
// resolveTokenParamMode's proactive-half/learned-cache pattern: apply what
// we've already discovered before dispatch instead of paying the failing
// round trip again on every request from the same client.
func (p *Proxy) resolveReasoningEffortNone(providerID, accountID string) bool {
	return p.registry.Health().LearnedReasoningEffortNone(providerID, accountID)
}

// applyReasoningEffortNone forces reasoning_effort to "none" on a request that
// carries a "tools" array. ok=false means nothing to do: no "tools" field (the
// quirk only applies to tool-call requests) or reasoning_effort is already
// "none".
func applyReasoningEffortNone(body []byte) (out []byte, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, hasTools := m["tools"]; !hasTools {
		return body, false
	}
	if raw, has := m["reasoning_effort"]; has {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s == "none" {
			return body, false
		}
	}
	b, err := json.Marshal("none")
	if err != nil {
		return body, false
	}
	m["reasoning_effort"] = b
	out, err = json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// looksLikeReasoningEffortToolsConflict detects the "this reasoning-tier model
// can't do tool calls unless reasoning_effort is off" error class. Observed
// live from Lightning AI's "openai/gpt-5.6-sol": "Function tools with
// reasoning_effort are not supported for gpt-5.6-sol in /v1/chat/completions.
// To use function tools, use /v1/responses or set reasoning_effort to
// 'none'." Matched loosely (mentions both "reasoning_effort" and "tool"
// together) since exact phrasing will vary by vendor, mirroring the other two
// heals in this package.
func looksLikeReasoningEffortToolsConflict(body []byte) bool {
	norm := strings.ToLower(string(body))
	return strings.Contains(norm, "reasoning_effort") && strings.Contains(norm, "tool")
}

// tryReasoningEffortHeal is the third chained reactive heal (see call site in
// ServeHTTP, alongside tryTokenParamHeal and tryMinTokensHeal): a model that
// rejects tool-call requests unless reasoning_effort is explicitly "none" is a
// request-shape problem identical across every account on this provider, not
// an account-scoped failure — rotating keys would just fail the same way on
// each one. Redispatches ONCE on the same account/URL and remembers the
// answer (registry.Health's learned-reasoning-effort cache) so the next
// tool-call request from this client skips the failing first attempt.
func (p *Proxy) tryReasoningEffortHeal(ctx context.Context, resp *http.Response, upstreamURL, authKey string, reqBody []byte, providerID, accountID string) (*http.Response, []byte, bool) {
	const peekCap = 8 << 10
	peeked, _ := io.ReadAll(io.LimitReader(resp.Body, peekCap))
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peeked), resp.Body))
	if !looksLikeReasoningEffortToolsConflict(peeked) {
		return resp, reqBody, false
	}
	fixedBody, ok := applyReasoningEffortNone(reqBody)
	if !ok {
		return resp, reqBody, false
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(fixedBody))
	if err != nil {
		return resp, reqBody, false
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+authKey)
	upReq.Header.Set("Accept", "text/event-stream, application/json")
	upReq.Header.Set("X-Accel-Buffering", "no")
	newResp, err := p.client.Do(upReq)
	if err != nil {
		return resp, reqBody, false
	}
	resp.Body.Close()
	p.registry.Health().LearnReasoningEffortNone(providerID, accountID)
	slog.Info("auto-forced reasoning_effort=none for tool-call request and retried, remembering for next time",
		"provider", providerID, "account", accountID,
		"status_before", resp.StatusCode, "status_after", newResp.StatusCode)
	return newResp, fixedBody, true
}
