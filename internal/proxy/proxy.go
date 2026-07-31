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
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

const maxBodyBytes = 4 << 20 // 4 MiB

type Proxy struct {
	registry *registry.Registry
	store    *store.Store
	client   *http.Client
	timeout  time.Duration
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

	// Resolve the caller-facing model to a combo or provider.
	combo := p.registry.GetCombo(info.Model)
	if combo != nil && !combo.Enabled {
		combo = nil
	}
	provider := p.registry.GetProvider(info.Model)

	if combo == nil && provider == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", info.Model), "invalid_request_error")
		return
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

	tried := map[string]bool{}
	maxAttempts := 1
	if plan != nil {
		maxAttempts = len(plan.members)
	}
	if maxAttempts == 0 {
		writeError(w, http.StatusBadGateway, "all upstreams failed", "gateway_error")
		return
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var upstream *config.Provider
		if plan != nil {
			pid := plan.next(p.registry, tried)
			if pid == "" {
				break
			}
			upstream = p.registry.GetProvider(pid)
			if upstream == nil || !upstream.Enabled {
				tried[pid] = true
				continue
			}
		} else {
			upstream = provider
		}
		tried[upstream.ID] = true

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
			translated, err := ResponsesToChatRequest(body, upstream.Model)
			if err != nil {
				slog.Error("responses->chat translation failed", "err", err)
				writeError(w, http.StatusInternalServerError, "responses translation failed", "gateway_error")
				return
			}
			body = translated
		} else {
			// Rewrite model field.
			rewritten, err := rewriteModel(body, upstream.Model)
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
			writeError(w, http.StatusBadGateway, "failed to build upstream request", "gateway_error")
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+upstream.AuthKey)
		upReq.Header.Set("Accept", "text/event-stream, application/json")
		upReq.Header.Set("X-Accel-Buffering", "no")

		start := time.Now()
		resp, err := p.client.Do(upReq)
		if err != nil {
			tried[upstream.ID] = true
			// Client-initiated abort (Esc) or parent deadline: the caller is gone, so
			// rotating to another provider is wasted and would even fire after the
			// client disconnected. Fail fast without rotating (#2, #11).
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				slog.Info("client aborted upstream dispatch", "provider", upstream.ID)
				return
			}
			// Transport-level failure (conn refused, dial timeout, header timeout):
			// the provider is down/unreachable → cooldown it and rotate to the next
			// member. Do NOT penalize for context.DeadlineExceeded *on the client
			// side*, only when it's genuinely the upstream's header timeout.
			p.registry.Health().RecordFailure(upstream.ID, 0)
			slog.Warn("upstream dispatch failed", "provider", upstream.ID, "err", err)
			if plan == nil {
				writeError(w, http.StatusBadGateway, "upstream request failed", "gateway_error")
				p.log(info.Model, upstream.ID, endpoint, http.StatusBadGateway, time.Since(start), err.Error(), nil, nil)
				return
			}
			continue
		}

		// Retryable upstream status → record and rotate (only if we have a plan).
		if p.registry.Health().IsRetryable(resp.StatusCode) {
			resp.Body.Close()
			p.registry.Health().RecordFailure(upstream.ID, resp.StatusCode)
			if plan == nil {
				writeUpstreamError(w, resp.StatusCode)
				p.log(info.Model, upstream.ID, endpoint, resp.StatusCode, time.Since(start), fmt.Sprintf("upstream returned %d", resp.StatusCode), nil, nil)
				return
			}
			slog.Warn("upstream retryable status", "provider", upstream.ID, "status", resp.StatusCode)
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
				p.log(info.Model, upstream.ID, endpoint, resp.StatusCode, time.Since(start), "unsupported endpoint", nil, nil)
				return
			}
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
			p.log(info.Model, upstream.ID, endpoint, resp.StatusCode, time.Since(start), strings.TrimSpace(string(bodyBytes)), nil, nil)
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
			p.log(info.Model, upstream.ID, endpoint, resp.StatusCode, time.Since(start), strings.TrimSpace(string(bodyBytes)), nil, nil)
			return
		}

		// Success path.
		p.registry.Health().RecordSuccess(upstream.ID)
		if info.Stream {
			// SSE streaming. Status was checked above BEFORE streaming starts, so any
			// retryable error was already rotated — we only reach here on a 2xx and
			// can safely commit the client response headers.
			translateStream := format == StreamFormatResponses
			promptTokens, completionTokens := p.streamResponse(w, resp, format, translateStream)
			p.log(info.Model, upstream.ID, endpoint, 200, time.Since(start), "", promptTokens, completionTokens)
			return
		}

		// Non-streaming: re-arm a body-read timeout for the response body only.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()

		var promptTokens, completionTokens *int
		if translateResponses && !nativeThisAttempt {
			translated, err := ChatToResponsesResponse(respBody, info.Model)
			if err != nil {
				slog.Error("chat->responses translation failed", "err", err)
				writeError(w, http.StatusInternalServerError, "responses translation failed", "gateway_error")
				p.log(info.Model, upstream.ID, endpoint, 500, time.Since(start), err.Error(), nil, nil)
				return
			}
			respBody = translated
			promptTokens, completionTokens = extractResponsesUsage(respBody)
		} else {
			promptTokens, completionTokens = extractChatUsage(respBody)
		}

		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		p.log(info.Model, upstream.ID, endpoint, resp.StatusCode, time.Since(start), "", promptTokens, completionTokens)
		return
	}

	writeError(w, http.StatusBadGateway, "all upstreams failed", "gateway_error")
	p.log(info.Model, "", endpoint, http.StatusBadGateway, 0, "all upstreams failed", nil, nil)
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

func (p *Proxy) log(modelIn, providerID, endpoint string, status int, latency time.Duration, errMsg string, pt, ct *int) {
	entry := config.LogEntry{
		Timestamp:        time.Now().Unix(),
		ModelIn:          modelIn,
		ProviderUsed:     providerID,
		Endpoint:         endpoint,
		Status:           status,
		LatencyMs:        latency.Milliseconds(),
		PromptTokens:     pt,
		CompletionTokens: ct,
		Error:            errMsg,
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

func extractChatUsage(body []byte) (prompt, completion *int) {
	var v struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &v) == nil && (v.Usage.PromptTokens > 0 || v.Usage.CompletionTokens > 0) {
		p := v.Usage.PromptTokens
		c := v.Usage.CompletionTokens
		return &p, &c
	}
	return nil, nil
}

func extractResponsesUsage(body []byte) (prompt, completion *int) {
	var v struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &v) == nil && (v.Usage.InputTokens > 0 || v.Usage.OutputTokens > 0) {
		p := v.Usage.InputTokens
		c := v.Usage.OutputTokens
		return &p, &c
	}
	return nil, nil
}
