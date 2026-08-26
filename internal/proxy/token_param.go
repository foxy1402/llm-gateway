package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"llm-gateway/internal/config"
)

// Recognized values for Provider/Account/ComboMember.TokenParamMode. Empty
// ("") means "auto": pass the client's field through, and self-heal via
// detection + a learned per-account cache if the model rejects it.
const (
	TokenParamMaxTokens     = "max_tokens"
	TokenParamMaxCompletion = "max_completion_tokens"
)

// ValidTokenParamMode reports whether s is a recognized TokenParamMode value
// (including "" for auto). Used by the dashboard API to reject typos instead
// of silently storing a mode that will never match anything.
func ValidTokenParamMode(s string) bool {
	return s == "" || s == TokenParamMaxTokens || s == TokenParamMaxCompletion
}

// resolveTokenParamMode picks the effective mode for one attempt, most
// specific wins: a combo member's explicit pin beats the account's, which
// beats the provider's. If nothing is explicitly configured, fall back to
// whatever smart mode has already LEARNED for this exact account from a
// previous live auto-heal (see tryTokenParamHeal) — that's what lets request
// #2+ skip the failing first attempt entirely instead of re-discovering it
// every single time. Returns "" when there's no explicit setting and nothing
// learned yet, meaning: pass the client's field through as-is and let the
// reactive detect-and-heal-once safety net handle it if it's wrong.
func (p *Proxy) resolveTokenParamMode(upstream *config.Provider, account config.Account, member *config.ComboMember) string {
	if member != nil && member.TokenParamMode != "" {
		return member.TokenParamMode
	}
	if account.TokenParamMode != "" {
		return account.TokenParamMode
	}
	if upstream != nil && upstream.TokenParamMode != "" {
		return upstream.TokenParamMode
	}
	if upstream != nil {
		return p.registry.Health().LearnedTokenParam(upstream.ID, account.ID)
	}
	return ""
}

// applyTokenParamMode forces the request body to use exactly the named field
// (TokenParamMaxTokens or TokenParamMaxCompletion), renaming the other one if
// present. ok=false means nothing changed — the target field is already the
// one in use, or the body has neither/both fields (ambiguous; left untouched
// rather than guessing).
func applyTokenParamMode(body []byte, mode string) (out []byte, ok bool) {
	other := TokenParamMaxCompletion
	if mode == TokenParamMaxCompletion {
		other = TokenParamMaxTokens
	} else if mode != TokenParamMaxTokens {
		return body, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	otherVal, hasOther := m[other]
	_, hasTarget := m[mode]
	if !hasOther || hasTarget {
		return body, false
	}
	m[mode] = otherVal
	delete(m, other)
	b, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return b, true
}

// tryTokenParamHeal is the reactive safety net (see call site in ServeHTTP):
// if resp looks like a "wrong field name" rejection and reqBody has exactly
// one of the two fields, it swaps the field, redispatches ONCE on the same
// upstream/account, remembers the answer for next time (registry.Health's
// learned-token-param cache — see resolveTokenParamMode), and returns the new
// response+body. healed=false leaves resp untouched — including its Body,
// which is restored unconsumed via a MultiReader so callers that don't heal
// can still read it exactly as if this check never ran. This still runs even
// when a mode was pre-applied (explicit or learned): configs go stale (a
// provider's model can change) and this is what self-corrects instead of
// wedging a wrong setting in place forever.
func (p *Proxy) tryTokenParamHeal(ctx context.Context, resp *http.Response, upstreamURL, authKey string, reqBody []byte, providerID, accountID string) (*http.Response, []byte, bool) {
	peeked := healPeek(resp)
	if !looksLikeTokenParamMismatch(peeked) {
		return resp, reqBody, false
	}
	fixedBody, healedField, ok := swapTokenParamField(reqBody)
	if !ok {
		return resp, reqBody, false
	}
	if ctx.Err() != nil {
		return resp, reqBody, false // client already gone; don't burn a redispatch
	}
	newResp, err := p.healRedispatch(ctx, upstreamURL, authKey, fixedBody)
	if err != nil {
		return resp, reqBody, false
	}
	resp.Body.Close()
	// Learn ONLY when the retry actually succeeded: the swap is symmetric and
	// self-corrects on a later mismatch, but a 500 redispatch proves nothing
	// about the field name itself.
	if healSucceeded(newResp) {
		p.registry.Health().LearnTokenParam(providerID, accountID, healedField)
	}
	slog.Info("auto-corrected max_tokens/max_completion_tokens field and retried, remembering for next time",
		"provider", providerID, "account", accountID, "field", healedField,
		"status_before", resp.StatusCode, "status_after", newResp.StatusCode)
	return newResp, fixedBody, true
}

// minTokenFloorDefault is the smallest completion budget we'll accept before
// proactively/reactively bumping it. Some reasoning-tier models (OpenAI's o-
// series and mirrors like Lightning AI's "openai/gpt-5.6-sol") spend part of
// the max_tokens/max_completion_tokens budget on hidden reasoning tokens
// before emitting any visible output, so a client probe like max_tokens: 1
// (a common "does this API key work" connection test) can never finish and
// the upstream errors instead of just truncating. 1 is enough for ordinary
// models but never enough for a reasoning model to say anything at all, so a
// small floor fixes the probe case without materially changing real requests.
const minTokenFloorDefault = 16

// tryMinTokensHeal is the reactive safety net for the "budget too small to
// finish" error class (see looksLikeTokenBudgetTooSmall). Unlike
// tryTokenParamHeal this can fire as a *second* heal on the same attempt: the
// field-name heal above may successfully rename max_tokens->max_completion_tokens
// and still get a fresh, unrelated rejection because the value itself (e.g. 1)
// is too small for this model to produce a single token of visible output.
// Bumping the value and redispatching once, on the same account/URL, is a
// request-shape fix exactly like the field-name heal: it must not burn the
// key into cooldown or consume a rotation attempt, since every sibling
// account would fail identically on the same too-small value.
func (p *Proxy) tryMinTokensHeal(ctx context.Context, resp *http.Response, upstreamURL, authKey string, reqBody []byte, providerID, accountID string, floor int) (*http.Response, []byte, bool) {
	if floor <= 0 {
		return resp, reqBody, false // MIN_COMPLETION_TOKENS=0 disables this heal
	}
	peeked := healPeek(resp)
	if !looksLikeTokenBudgetTooSmall(peeked) {
		return resp, reqBody, false
	}
	fixedBody, oldVal, ok := bumpTokenFloor(reqBody, floor)
	if !ok {
		return resp, reqBody, false
	}
	if ctx.Err() != nil {
		return resp, reqBody, false // client already gone; don't burn a redispatch
	}
	newResp, err := p.healRedispatch(ctx, upstreamURL, authKey, fixedBody)
	if err != nil {
		return resp, reqBody, false
	}
	resp.Body.Close()
	slog.Info("auto-bumped too-small max_tokens/max_completion_tokens and retried",
		"provider", providerID, "account", accountID, "from", oldVal, "to", floor,
		"status_before", resp.StatusCode, "status_after", newResp.StatusCode)
	return newResp, fixedBody, true
}

// looksLikeTokenBudgetTooSmall detects the "couldn't finish generating within
// the given token budget" error class — distinct from looksLikeTokenParamMismatch
// (wrong field name): the field name is right, but the value is too small for
// this model to emit anything. Seen from Lightning AI as: "Could not finish
// the message because max_tokens or model output limit was reached. Please
// try again with higher max_tokens." Matched loosely (mentions the token
// field plus "reached"/"limit" wording) since exact phrasing varies by vendor.
func looksLikeTokenBudgetTooSmall(body []byte) bool {
	norm := strings.ReplaceAll(strings.ToLower(string(body)), "_", "")
	if !strings.Contains(norm, "maxtokens") && !strings.Contains(norm, "maxcompletiontokens") {
		return false
	}
	return strings.Contains(norm, "output limit was reached") ||
		strings.Contains(norm, "could not finish the message") ||
		strings.Contains(norm, "try again with higher")
}

// bumpTokenFloor raises whichever of max_tokens/max_completion_tokens is
// present to floor, IF its current value is below floor. oldVal is the
// previous numeric value (for logging). ok=false means there's nothing to
// bump: neither field is present, both are (ambiguous), the value isn't a
// plain number, or it's already >= floor (guessing a bigger number wouldn't
// be based on anything and could mask a real "genuinely needs way more" case).
func bumpTokenFloor(body []byte, floor int) (out []byte, oldVal float64, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, 0, false
	}
	tokVal, hasTok := m[TokenParamMaxTokens]
	compVal, hasComp := m[TokenParamMaxCompletion]
	var field string
	var raw json.RawMessage
	switch {
	case hasTok && !hasComp:
		field, raw = TokenParamMaxTokens, tokVal
	case hasComp && !hasTok:
		field, raw = TokenParamMaxCompletion, compVal
	default:
		return nil, 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, 0, false
	}
	if n < 0 {
		// A negative budget is a client bug, not a too-small one — bumping it
		// would silently change the request's meaning instead of surfacing it.
		return nil, 0, false
	}
	if n >= float64(floor) {
		return nil, 0, false
	}
	b, err := json.Marshal(floor)
	if err != nil {
		return nil, 0, false
	}
	m[field] = b
	out, err = json.Marshal(m)
	if err != nil {
		return nil, 0, false
	}
	return out, n, true
}

// looksLikeTokenParamMismatch detects the "wrong max_tokens/max_completion_tokens
// field name" error class across providers. Wording varies — OpenAI: "Unsupported
// parameter: 'max_tokens' is not supported with this model. Use
// 'max_completion_tokens' instead."; Lightning AI: "this model is not supported
// MaxTokens, please use MaxCompletionTokens" — but every variant seen mentions
// both field names together, so matching on that combination
// (case/underscore-insensitive) is a reliable, provider-agnostic signal instead
// of hardcoding any one vendor's exact phrasing or status code (we've seen this
// as both 400 and 500 depending on the upstream).
func looksLikeTokenParamMismatch(body []byte) bool {
	norm := strings.ReplaceAll(strings.ToLower(string(body)), "_", "")
	return strings.Contains(norm, "maxtokens") && strings.Contains(norm, "maxcompletiontokens")
}

// swapTokenParamField renames whichever of max_tokens/max_completion_tokens is
// present in a chat-completions request body to the other name, preserving the
// value and every other field untouched. resultField reports which name the
// body ends up with. ok=false means there's nothing safe to swap — neither
// field is present (nothing to fix) or both are (ambiguous; guessing which one
// to drop risks silently changing the caller's intended limit, so we leave it
// alone and let the real error surface instead).
func swapTokenParamField(body []byte) (out []byte, resultField string, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", false
	}
	oldVal, hasOld := m[TokenParamMaxTokens]
	newVal, hasNew := m[TokenParamMaxCompletion]
	switch {
	case hasOld && !hasNew:
		m[TokenParamMaxCompletion] = oldVal
		delete(m, TokenParamMaxTokens)
		resultField = TokenParamMaxCompletion
	case hasNew && !hasOld:
		m[TokenParamMaxTokens] = newVal
		delete(m, TokenParamMaxCompletion)
		resultField = TokenParamMaxTokens
	default:
		return nil, "", false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", false
	}
	return b, resultField, true
}
