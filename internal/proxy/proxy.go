package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

const maxBodyBytes = 4 << 20 // 4 MiB

// maxLogPayload caps how many bytes of a request/response body we store in the
// request_log for dashboard inspection. Bodies larger than this are truncated.
const maxLogPayload = 4 << 10 // 4 KiB

// maxAccountsPerProvider caps how many distinct accounts of one provider we try
// in a single request before declaring that provider done. Free-tier keys burn
// out one-by-one; this keeps a single client request from pinning against a
// whole pool serially while still covering the common "2–3 spare keys" case.
const maxAccountsPerProvider = 3

type Proxy struct {
	registry *registry.Registry
	store    *store.Store
	client   *http.Client
	timeout  time.Duration
	// modelAliases maps names clients might send that match no provider/combo to
	// the name of the provider/combo that should actually serve the request.
	modelAliases map[string]string
}

func New(reg *registry.Registry, st *store.Store, timeout time.Duration) *Proxy {
	tr := &http.Transport{
		DisableCompression: true,
		// Header timeout only: covers connect → first response bytes. Once headers
		// arrive, the stream lives on the client's request context (cancel = Esc) and
		// the per-chunk stall detector in stream.go. Applied as a transport timeout
		// (not a context) so canceling the request context later cleans up the body
		// without the timeout ever firing mid-stream.
		ResponseHeaderTimeout: timeout,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: min(timeout, 10*time.Second),
	}
	return &Proxy{
		registry: reg,
		store:    st,
		client:   &http.Client{Transport: tr},
		timeout:  timeout,
	}
}

// SetModelAliases installs the client-name → route fallback map (MODEL_ALIASES).
func (p *Proxy) SetModelAliases(m map[string]string) { p.modelAliases = m }

// peekInfo extracts routing fields without consuming more than the header JSON.
type peekInfo struct {
	Model  string
	Stream bool
	Raw    []byte
}

// peekBody reads the request body (bounded) and parses the routing fields.
func peekBody(r *http.Request) (*peekInfo, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var probe struct {
		Model  json.RawMessage `json:"model"`
		Stream bool            `json:"stream"`
	}
	_ = json.Unmarshal(raw, &probe)
	var model string
	if len(probe.Model) > 0 {
		var m string
		if err := json.Unmarshal(probe.Model, &m); err == nil {
			model = m
		}
	}
	return &peekInfo{Model: model, Stream: probe.Stream, Raw: raw}, nil
}

// ServeHTTP routes a proxied endpoint. `endpoint` is one of registry.Endpoint*.
// upstreamPath maps an endpoint to the upstream URL path for a provider that
// natively supports it.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request, endpoint string) {
	info, err := peekBody(r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "could not read request body", "invalid_request_error")
		return
	}
	if info.Model == "" {
		writeError(w, http.StatusBadRequest, "missing model", "invalid_request_error")
		return
	}

	// Resolve the caller-facing model to a combo or provider, following MODEL_ALIASES
	// fallbacks when the name matches nothing concrete. Providers/combos always win
	// over aliases, and alias cycles resolve deterministically rather than looping.
	var aliasChain []string
	var combo *config.Combo
	var provider *config.Provider
	lookup := info.Model
	for {
		combo = p.registry.GetCombo(lookup)
		if combo != nil && !combo.Enabled {
			combo = nil
		}
		provider = p.registry.GetProvider(lookup)
		if combo != nil || provider != nil {
			break
		}
		target, ok := p.modelAliases[lookup]
		if !ok || target == lookup || slices.Contains(aliasChain, lookup) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", info.Model), "invalid_request_error")
			return
		}
		slog.Info("model alias applied", "from", lookup, "to", target)
		aliasChain = append(aliasChain, lookup)
		lookup = target
	}

	// Embeddings bypass combos: the model must be a direct provider.
	if endpoint == registry.EndpointEmbeddings && combo != nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("model %q is a combo; embeddings require a direct provider id", info.Model),
			"invalid_request_error")
		return
	}
	if provider != nil && !provider.Enabled {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", info.Model), "invalid_request_error")
		return
	}

	// /v1/responses on a native provider forwards untouched; otherwise we translate
	// to chat completions. For combos the decision is per-attempt (each member differs).
	translateResponses := endpoint == registry.EndpointResponses

	// Build the attempt plan: either one provider or all combo members.
	var plan *rotationPlan
	if combo != nil {
		plan = p.newRotationPlan(combo, endpoint)
	}

	// Attempt budget: combo members × their accounts. For a direct provider we treat
	// it as a singleton with its own account pool (so a one-off provider call can
	// still cycle multiple keys on failure before giving up).
	memberCount := 1
	if plan != nil {
		memberCount = len(plan.members)
	}
	if memberCount == 0 {
		writeError(w, http.StatusBadGateway, "all upstreams failed", "gateway_error")
		return
	}
	maxAttempts := memberCount * maxAccountsPerProvider

	triedProviders := map[string]bool{} // providers whose account pool is exhausted for this request
	triedAccounts := map[string]bool{}  // accounts already burned during this request
	triedMembers := map[string]bool{}   // pinned members whose key already failed this request

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var upstream *config.Provider
		var member *config.ComboMember
		if plan != nil {
			member = plan.next(p.registry, triedProviders, triedMembers)
			if member == nil {
				break
			}
			upstream = p.registry.GetProvider(member.ProviderID)
			if upstream == nil || !upstream.Enabled {
				triedProviders[member.ProviderID] = true
				continue
			}
		} else {
			upstream = provider
		}

		// Resolve which account serves this attempt. A combo member may pin one
		// specific key (member.AccountID); otherwise rotate across the provider's
		// pool. Either way the pick must happen before model resolution: a bound
		// model lives partly on the member and partly on the account itself.
		var account config.Account
		var ok bool
		if member != nil && member.AccountID != "" {
			account, ok = p.registry.PinnedAccount(upstream, member.AccountID, p.registry.Health(), triedAccounts)
		} else {
			account, ok = p.registry.NextAccount(upstream, p.registry.Health(), triedAccounts)
		}
		if !ok {
			if member != nil && member.AccountID != "" {
				// Only this pinned key is out (tried/disabled/cooling); same-provider
				// siblings pinned to other keys must stay reachable.
				triedMembers[memberKey(*member)] = true
			} else {
				triedProviders[upstream.ID] = true
			}
			continue
		}

		// Model precedence: combo member > pinned/rotated key's own binding >
		// provider default. The member wins because it's the most specific,
		// deliberately-configured layer (e.g. "vercel key2 → gpt-oss").
		model := upstream.Model
		if account.Model != "" {
			model = account.Model
		}
		if member != nil && member.Model != "" {
			model = member.Model
		}
		triedAccounts[account.ID] = true
		authKey := account.AuthKey

		// logID tags log entries with the account that actually served the response
		// so quota burn is traceable per key without breaking provider-level queries
		// (dashboard filters use provider ID prefix match).
		logID := upstream.ID
		if len(upstream.Accounts) > 0 {
			logID = upstream.ID + "[" + account.Label + "]"
		}

		// Per-attempt responses decision: for a combo each member decides for itself.
		nativeThisAttempt := translateResponses && upstream.ResponsesNative
		format := StreamFormatChat
		if translateResponses && !nativeThisAttempt {
			format = StreamFormatResponses
		}

		upstreamPath := upstreamPathFor(endpoint, nativeThisAttempt)

		// Build upstream request body.
		body := info.Raw
		if translateResponses && !nativeThisAttempt {
			translated, err := ResponsesToChatRequest(body, model)
			if err != nil {
				slog.Error("responses->chat translation failed", "err", err)
				writeError(w, http.StatusInternalServerError, "responses translation failed", "gateway_error")
				return
			}
			body = translated
		} else {
			// Rewrite model field to the attempt's chosen model.
			rewritten, err := rewriteModel(body, model)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error")
				return
			}
			body = rewritten
		}

		// Dispatch. base_url is the full OpenAI-compatible root including its own
		// version (https://api.groq.com/openai/v1, https://generativelanguage.googleapis.com/v1beta/openai,
		// https://open.bigmodel.cn/api/paas/v4…). We append the endpoint name
		// directly — no version heuristics — so the result is exactly what the user
		// typed plus /chat/completions (or /completions, /responses, /embeddings).
		upstreamURL := buildUpstreamURL(upstream.BaseURL, upstreamPath)

		// The upstream request lives on the CLIENT's request context throughout:
		// client-cancel (Esc in Cursor) aborts the upstream stream (#11), and the
		// stream runs unbounded. Header/connect/TLS timeouts are enforced at the
		// transport level (ResponseHeaderTimeout etc.), so a fixed wall-clock context
		// timeout never kills a live stream mid-generation (#1).
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			p.registry.Health().RecordFailure(upstream.ID, 0)
			p.registry.Health().RecordAccountFailure(upstream.ID, account.ID)
			writeError(w, http.StatusBadGateway, "failed to build upstream request", "gateway_error")
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+authKey)
		upReq.Header.Set("Accept", "text/event-stream, application/json")
		upReq.Header.Set("X-Accel-Buffering", "no")

		start := time.Now()
		resp, err := p.client.Do(upReq)
		if err != nil {
			// Client-initiated abort (Esc) or parent deadline: the caller is gone, so
			// rotating to another provider is wasted and would even fire after the
			// client disconnected. Fail fast without rotating (#2, #11).
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				slog.Info("client aborted upstream dispatch", "provider", upstream.ID)
				return
			}
			// Transport-level failure (conn refused, dial timeout, header timeout):
			// the endpoint itself is unreachable, not the key. Cool down at the
			// *provider* level (all accounts share the same URL and would fail the
			// same way) and rotate to the next member. Do NOT penalize for
			// context.DeadlineExceeded on the client side (#2, #11).
			p.registry.Health().RecordFailure(upstream.ID, 0)
			slog.Warn("upstream dispatch failed", "provider", logID, "err", err)
			if plan == nil {
				writeError(w, http.StatusBadGateway, "upstream request failed", "gateway_error")
					p.log(info.Model, logID, endpoint, http.StatusBadGateway, time.Since(start), err.Error(), nil, nil, nil, upstreamURL, string(body), "")
					return
			}
			triedProviders[upstream.ID] = true
			continue
		}

		// Retryable upstream status → record and rotate within this provider's pool.
		// 429/5xx from a key are account-scoped (that key is over quota or broken) so
		// the next attempt re-tries the SAME provider's other accounts before the
		// combo moves to an entirely different provider.
		if p.registry.Health().IsRetryable(resp.StatusCode) {
			resp.Body.Close()
			// Account-scoped failure only: one burned key doesn't take the whole
			// pool out of rotation (that's the entire point of multi-account). The
			// account enters cooldown; other accounts on the same endpoint stay live.
			// Provider-level cooldown is reserved for transport failures where the
			// endpoint itself is unreachable and ALL keys would fail identically.
			p.registry.Health().RecordAccountFailure(upstream.ID, account.ID)
			if plan == nil {
					writeUpstreamError(w, resp.StatusCode)
					p.log(info.Model, logID, endpoint, resp.StatusCode, time.Since(start), fmt.Sprintf("upstream returned %d", resp.StatusCode), nil, nil, nil, upstreamURL, string(body), "")
					return
				}
			slog.Warn("upstream retryable status", "provider", logID, "status", resp.StatusCode)
			// NOTE: provider is NOT dropped from triedProviders — NextAccount will
			// yield a different key next time plan.next re-selects this provider.
			continue
		}

		// 404/405 endpoint-unsupported handling. Every OpenAI-compatible provider
		// MUST implement chat.completions — a 404 there means our URL is wrong
		// (bad base_url), not that the provider "doesn't support" it. Only the
		// *optional* endpoints (legacy /v1/completions, /v1/embeddings, native
		// /v1/responses) can genuinely be unimplemented.
		optional := endpoint == registry.EndpointCompletions ||
			endpoint == registry.EndpointEmbeddings ||
			(nativeThisAttempt && endpoint == registry.EndpointResponses)
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) && optional {
			resp.Body.Close()
			p.registry.Health().MarkUnsupported(upstream.ID, endpoint)
			if plan == nil {
				writeError(w, resp.StatusCode, fmt.Sprintf("provider does not support %s", endpoint), "invalid_request_error")
				p.log(info.Model, logID, endpoint, resp.StatusCode, time.Since(start), "unsupported endpoint", nil, nil, nil, upstreamURL, string(body), "")
				return
			}
			triedProviders[upstream.ID] = true
			continue
		}
		// Any remaining 404/405 on chat.completions (or a non-optional endpoint) is
		// a bad path/auth/body — surface the real upstream body so the misconfig is
		// visible instead of masquerading as "unsupported endpoint".
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			for k, v := range resp.Header {
				if strings.HasPrefix(k, "Content-") {
					w.Header()[k] = v
				}
			}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		p.log(info.Model, logID, endpoint, resp.StatusCode, time.Since(start), strings.TrimSpace(string(bodyBytes)), nil, nil, nil, upstreamURL, string(body), string(bodyBytes))
		return
	}

	// Non-2xx other than the above → surface to caller (e.g. 400 from upstream).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		for k, v := range resp.Header {
			if strings.HasPrefix(k, "Content-") {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		p.log(info.Model, logID, endpoint, resp.StatusCode, time.Since(start), strings.TrimSpace(string(bodyBytes)), nil, nil, nil, upstreamURL, string(body), string(bodyBytes))
			return
		}

		// Success path.
		p.registry.Health().RecordSuccess(upstream.ID)
		p.registry.Health().RecordAccountSuccess(upstream.ID, account.ID)
		if info.Stream {
			// SSE streaming. Status was checked above BEFORE streaming starts, so any
			// retryable error was already rotated — we only reach here on a 2xx and
			// can safely commit the client response headers.
			translateStream := format == StreamFormatResponses
			promptTokens, completionTokens, cachedTokens := p.streamResponse(w, resp, format, translateStream)
				p.log(info.Model, logID, endpoint, 200, time.Since(start), "", promptTokens, completionTokens, cachedTokens, upstreamURL, string(body), "")
			return
		}

		// Non-streaming: re-arm a body-read timeout for the response body only.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()

		var promptTokens, completionTokens, cachedTokens *int
		if translateResponses && !nativeThisAttempt {
			translated, err := ChatToResponsesResponse(respBody, info.Model)
			if err != nil {
				slog.Error("chat->responses translation failed", "err", err)
			writeError(w, http.StatusInternalServerError, "responses translation failed", "gateway_error")
					p.log(info.Model, logID, endpoint, 500, time.Since(start), err.Error(), nil, nil, nil, upstreamURL, string(body), string(respBody))
				return
			}
			respBody = translated
			promptTokens, completionTokens, cachedTokens = extractResponsesUsage(respBody)
		} else {
			promptTokens, completionTokens, cachedTokens = extractChatUsage(respBody)
		}

		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
			p.log(info.Model, logID, endpoint, resp.StatusCode, time.Since(start), "", promptTokens, completionTokens, cachedTokens, upstreamURL, string(body), string(respBody))
		return
	}

	writeError(w, http.StatusBadGateway, "all upstreams failed", "gateway_error")
	p.log(info.Model, "", endpoint, http.StatusBadGateway, 0, "all upstreams failed", nil, nil, nil, "", "", "")
}

func upstreamPathFor(endpoint string, nativeResponses bool) string {
	switch endpoint {
	case registry.EndpointChatCompletions:
		return "/chat/completions"
	case registry.EndpointCompletions:
		return "/completions"
	case registry.EndpointResponses:
		if nativeResponses {
			return "/responses"
		}
		return "/chat/completions"
	case registry.EndpointEmbeddings:
		return "/embeddings"
	default:
		return "/chat/completions"
	}
}

// buildUpstreamURL appends an endpoint path to the provider's base URL. The base
// is expected to already be the full OpenAI-compatible root including its version
// (https://api.groq.com/openai/v1, https://generativelanguage.googleapis.com/v1beta/openai,
// https://open.bigmodel.cn/api/paas/v4). Only a stray trailing slash on the base is
// normalized away — no version detection, so any current-or-future API prefix works.
func buildUpstreamURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + path
}

// BuildUpstreamURL is a public wrapper so the dashboard joins upstream URLs the
// same way as the proxy (used by the fetch-models helper).
func BuildUpstreamURL(base, path string) string { return buildUpstreamURL(base, path) }

// rewriteModel replaces the "model" field in the JSON body.
func rewriteModel(body []byte, model string) ([]byte, error) {
	// Patch only the top-level "model" field's byte slice instead of
	// unmarshalling + remarshalling the whole body (#3, polish): large FIM
	// completions prompts would otherwise be fully copied twice per request.
	// We locate the top-level "model" key via a token scan and splice its value;
	// on any shape surprise we fall back to the safe full round-trip below.
	if val, ok := spliceModel(body, model); ok {
		return val, nil
	}
	b, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = b
	return json.Marshal(m)
}

// spliceModel replaces the value of the top-level "model" field by byte splice.
// It walks the first object level with a streaming decoder; once the "model" key
// is found it replaces the value's byte span without copying other large fields.
// Returns ok=false on any shape that isn't a flat-object "model" string, forcing
// the caller's structural fallback.
func spliceModel(body []byte, model string) ([]byte, bool) {
	quoted := strconv.Quote(model)
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := keyTok.(string)
		if key != "model" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
			return nil, false
		}
		// After decoding, InputOffset points just past the value. The value's start
		// is exactly end-len(raw) since RawMessage holds the raw bytes verbatim.
		end := dec.InputOffset()
		start := end - int64(len(raw))
		out := make([]byte, 0, len(body)-len(raw)+len(quoted))
		out = append(out, body[:start]...)
		out = append(out, quoted...)
		out = append(out, body[end:]...)
		return out, true
	}
	return nil, false
}

func (p *Proxy) log(modelIn, providerID, endpoint string, status int, latency time.Duration, errMsg string, pt, ct, cached *int, upstreamURL string, reqPayload string, respSnippet string) {
	// Truncate payloads to avoid storing megabytes per log entry.
	if len(reqPayload) > maxLogPayload {
		reqPayload = string(reqPayload[:maxLogPayload])
	}
	if len(respSnippet) > maxLogPayload {
		respSnippet = string(respSnippet[:maxLogPayload])
	}
	entry := config.LogEntry{
		Timestamp:        time.Now().Unix(),
		ModelIn:          modelIn,
		ProviderUsed:     providerID,
		Endpoint:         endpoint,
		Status:           status,
		LatencyMs:        latency.Milliseconds(),
		PromptTokens:     pt,
		CompletionTokens: ct,
		CachedTokens:     cached,
		Error:            errMsg,
		UpstreamURL:      upstreamURL,
		RequestPayload:   reqPayload,
		ResponseSnippet:  respSnippet,
	}
	// Async to avoid blocking the response path.
	go func() {
		if err := p.store.LogRequest(entry); err != nil {
			slog.Warn("failed to write request log", "err", err)
		}
	}()
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": typ, "code": typ},
	})
}

func writeUpstreamError(w http.ResponseWriter, upstreamStatus int) {
	writeError(w, upstreamStatus, fmt.Sprintf("upstream returned %d", upstreamStatus), "upstream_error")
}

// --- usage extraction ---

func extractChatUsage(body []byte) (prompt, completion, cached *int) {
	var v struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &v) == nil && (v.Usage.PromptTokens > 0 || v.Usage.CompletionTokens > 0) {
		p := v.Usage.PromptTokens
		c := v.Usage.CompletionTokens
		if d := v.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
			ct := d.CachedTokens
			cached = &ct
		}
		return &p, &c, cached
	}
	return nil, nil, nil
}

func extractResponsesUsage(body []byte) (prompt, completion, cached *int) {
	var v struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &v) == nil && (v.Usage.InputTokens > 0 || v.Usage.OutputTokens > 0) {
		p := v.Usage.InputTokens
		c := v.Usage.OutputTokens
		if d := v.Usage.InputTokensDetails; d != nil && d.CachedTokens > 0 {
			ct := d.CachedTokens
			cached = &ct
		}
		return &p, &c, cached
	}
	return nil, nil, nil
}
