package proxy

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type StreamFormat int

const (
	StreamFormatChat StreamFormat = iota
	StreamFormatResponses
)

// streamStallTimeout is the maximum time to wait between SSE chunks before
// treating the upstream as stalled and terminating the stream. Distinct from the
// per-request header timeout: long coding generations can legitimately idle for
// tens of seconds between tokens, but a multi-minute silence means a hung upstream.
const streamStallTimeout = 90 * time.Second

// postDoneUsageWait is how long to keep reading after the [DONE] sentinel purely
// to collect a trailing usage frame. Several providers emit usage AFTER the
// sentinel, and returning the instant we see [DONE] logged NULL token counts for
// them. Nothing read here is forwarded — [DONE] is terminal for the client — so
// this only ever costs a frame that is already in flight.
const postDoneUsageWait = 250 * time.Millisecond

// errClientWriteFailed marks a stream that ended because writing to the CLIENT
// failed (it hung up mid-response). Like a context cancel, this is not the
// upstream's fault and must not cool down the account.
var errClientWriteFailed = errors.New("client write failed mid-stream")

// ssePayload extracts the data-field value from one SSE line. Per the SSE spec
// the colon may be followed by an optional space, so matching only "data: "
// (with the space) missed the equally legal "data:[DONE]" form — such a stream
// was treated as an unparsed comment, so the sentinel was never recognized, the
// loop waited for an EOF the upstream never sent, and the stall timer eventually
// marked a perfectly healthy account failed.
func ssePayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:"))), true
}

// isDoneSentinel reports whether an SSE data payload is the terminal sentinel.
// Compared case-insensitively: "[done]" is seen in the wild and missing it has
// the same account-poisoning consequence described on ssePayload.
func isDoneSentinel(payload []byte) bool {
	return bytes.EqualFold(payload, []byte("[DONE]"))
}

// streamResponse copies an upstream SSE stream to the client, translating
// chat-completions deltas to responses-format events when requested.
//
// Caller has already inspected the response and committed to this upstream.
// Returns (promptTokens, completionTokens, cachedTokens) extracted from the terminal
// usage frame so the caller can log real token counts for streaming requests (#12);
// nil when the upstream did not report usage.
//
// The returned error is non-nil when the stream died uncleanly — upstream read
// error or stall timeout — so the caller can record an account failure instead of
// letting a provider that 200s then hangs keep a perfect health record. A clean
// EOF (with or without a [DONE] sentinel) is not an error: some providers close
// the body instead of sending the sentinel. [DONE] itself also terminates the
// read loop without waiting for EOF: some upstreams hold the connection open
// after the sentinel, and waiting there used to trip the stall timer and mark a
// healthy account failed.
//
// The upstream body is closed on every exit path, and the reader goroutine is
// reaped — a missed close leaks one connection (fd) per streamed response and,
// under sustained streaming load, exhausts sockets until every dial fails.
//
// ctx is the CLIENT's request context: when it is canceled (user hits Esc), the
// returned error wraps the context error so the caller can distinguish "client
// left" (no penalty) from "upstream died" (account failure). A nil ctx is
// treated as context.Background (never canceled).
func (p *Proxy) streamResponse(w http.ResponseWriter, upstream *http.Response, format StreamFormat, translate bool, ctx context.Context) (*int, *int, *int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Defensive: a synthetic response assembled by a heal could omit either field,
	// and both would panic below (WriteHeader rejects 0; a nil body nil-derefs).
	if upstream.Body == nil {
		upstream.Body = http.NoBody
	}
	status := upstream.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	// SSE headers.
	// Note: if translate is false, we forward chat-completions stream bytes as-is.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)

	// Flush via ResponseController so wrapping middleware (the request logger's
	// statusWriter, the fail-ban statusCapture) can never silently swallow
	// streaming flushes again — a direct w.(http.Flusher) assertion returns nil
	// through those wrappers and tokens arrive in 2 KiB bursts.
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	// Close the upstream body on every exit path — including panics. Before this,
	// the ONLY body never closed in the proxy was this one: one leaked connection
	// per streamed response, eventually exhausting sockets under sustained load
	// until every dial failed with "cannot assign requested address" and every
	// combo 502'd. Close releases the TRANSPORT connection; the reader goroutine
	// below is stopped separately via doneCh (Close alone does not unblock a
	// Read on every body implementation, e.g. wrapped or in-memory bodies).
	defer upstream.Body.Close()

	src := bufio.NewReaderSize(upstream.Body, 4096)
	var out bytes.Buffer

	// Accumulate usage for terminal responses.completed event. The two detail
	// blobs are preserved verbatim so prompt-cache reporting (cached_tokens for
	// OpenAI-compatible, cache_creation/cache_read for Anthropic-style) survives
	// translation instead of being silently dropped.
	var lastUsage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	var lastUsageDetails usageDetails
	var sawUsage bool
	var respModel string
	// #12: capture prompt/completion token counts for logging regardless of format.
	var promptTokens, completionTokens, cachedTokens *int

	// Per-chunk stall detection: run each line read in a goroutine and bound it with
	// a timer that resets on every successful read. The goroutine stops when the
	// function returns (doneCh closed by defer) or when the upstream body hits
	// EOF/error: its send is gated on doneCh so it can never block forever on a
	// full channel after an early return ([DONE], stall, client abort) — a bare
	// blocking send would leak one goroutine per such stream.
	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		// A panic in the transport's Read (seen with some HTTP/2 + proxy stacks on
		// a torn-down connection) would otherwise kill the whole gateway. Contain it
		// and hand the reader loop a synthetic error so it terminates the stream
		// immediately instead of idling until the 90s stall timer fires.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic reading upstream stream", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				select {
				case readCh <- readResult{err: fmt.Errorf("panic reading upstream stream: %v", rec)}:
				case <-doneCh:
				}
			}
		}()
		for {
			line, err := src.ReadBytes('\n')
			select {
			case readCh <- readResult{line, err}:
			case <-doneCh:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	stall := time.NewTimer(streamStallTimeout)
	defer stall.Stop()

	// Buffered chat tool_call deltas (responses translation only). Chat-completions
	// streams tool calls as per-index deltas with no responses equivalent that can
	// be forwarded statelessly, so they accumulate here and are emitted as
	// function_call events when [DONE] arrives.
	toolOrder := []int{}
	toolBuf := map[int]*bufferedToolCall{}

	// flush writes whatever has accumulated to the client. A failed write means the
	// client is gone: previously the error was discarded, so the gateway drained the
	// entire upstream generation (paying for every token) and reported success.
	flush := func() error {
		if out.Len() == 0 {
			return nil
		}
		if _, err := w.Write(out.Bytes()); err != nil {
			return fmt.Errorf("%w: %v", errClientWriteFailed, err)
		}
		out.Reset()
		_ = rc.Flush()
		return nil
	}

	// emitTerminal writes the responses-format terminal events (buffered tool calls
	// followed by response.completed). Shared by the [DONE] path and the clean-EOF
	// path: a provider that closes the body instead of sending the sentinel used to
	// get neither, silently dropping every buffered tool call and leaving a strict
	// Responses client waiting forever for response.completed.
	emitTerminal := func() {
		if !translate || format != StreamFormatResponses {
			return
		}
		writeFunctionCallEvents(&out, toolOrder, toolBuf)
		completed := buildResponseCompleted(respModel, lastUsage, &lastUsageDetails, sawUsage, functionCallOutputItems(toolOrder, toolBuf))
		out.WriteString("event: response.completed\n")
		out.WriteString("data: ")
		out.Write(completed)
		out.WriteString("\n\n")
	}

	// absorbUsage records token counts from one chunk, keeping already-captured
	// values when a later frame reports nothing.
	absorbUsage := func(payload []byte) {
		pt, ct, cached := extractChunkUsage(payload)
		if pt != nil {
			promptTokens = pt
		}
		if ct != nil {
			completionTokens = ct
		}
		if cached != nil {
			cachedTokens = cached
		}
	}

	// drainPostDoneUsage keeps reading briefly after the sentinel to catch a
	// trailing usage frame. See postDoneUsageWait.
	drainPostDoneUsage := func() {
		if promptTokens != nil && completionTokens != nil {
			return // already have counts; nothing worth waiting for
		}
		deadline := time.NewTimer(postDoneUsageWait)
		defer deadline.Stop()
		for {
			select {
			case rr := <-readCh:
				if payload, ok := ssePayload(rr.line); ok && !isDoneSentinel(payload) {
					absorbUsage(payload)
					if promptTokens != nil && completionTokens != nil {
						return
					}
				}
				if rr.err != nil {
					return
				}
			case <-deadline.C:
				return
			case <-ctx.Done():
				return
			}
		}
	}

	for {
		var rr readResult
		select {
		case rr = <-readCh:
			if len(rr.line) > 0 && !stall.Stop() {
				<-stall.C
			}
			stall.Reset(streamStallTimeout)
		case <-stall.C:
			// Upstream stalled mid-response: the client got a truncated stream and
			// the account must not keep a clean health record for it.
			return promptTokens, completionTokens, cachedTokens, fmt.Errorf("upstream stalled for %s mid-stream", streamStallTimeout)
		case <-ctx.Done():
			// Client went away mid-stream (Esc in an IDE, page nav). Not an upstream
			// fault: the wrapped context error tells the caller NOT to penalize
			// the account. Prefer a read that already completed (both can be ready
			// at once; Go would otherwise pick randomly and drop a good chunk):
			// only when no chunk is waiting do we report the disconnect.
			select {
			case rr = <-readCh:
				if len(rr.line) > 0 && !stall.Stop() {
					<-stall.C
				}
				stall.Reset(streamStallTimeout)
			default:
				return promptTokens, completionTokens, cachedTokens, fmt.Errorf("client disconnected mid-stream: %w", ctx.Err())
			}
		}
		line, err := rr.line, rr.err
		if len(line) > 0 {
			if payload, isData := ssePayload(line); isData {
				// #12: extract usage from every chunk before translation. Upstream
				// chunks are always chat-completions format (prompt_tokens/completion_tokens),
				// even when we later translate them to the responses shape for the client.
				absorbUsage(payload)
				if isDoneSentinel(payload) {
					if translate && format == StreamFormatResponses {
						emitTerminal()
					} else {
						out.Write(line)
						// Terminate the final event with its blank separator line: the
						// sentinel's own trailing newline arrives as a SEPARATE read that
						// the return below never consumes, so a strict SSE parser would
						// see an unterminated event.
						if !bytes.HasSuffix(line, []byte("\n\n")) {
							out.WriteString("\n")
						}
					}
					if ferr := flush(); ferr != nil {
						return promptTokens, completionTokens, cachedTokens, ferr
					}
					// The sentinel is the protocol-level end of stream: stop here
					// instead of waiting for EOF. Upstreams that keep the connection
					// open after [DONE] used to hold us until the stall timer fired,
					// which then marked the account failed for a stream that had
					// actually completed cleanly. Nothing after the sentinel is
					// forwarded ([DONE] is terminal per SSE convention), but we do
					// briefly collect a trailing usage frame for the request log.
					drainPostDoneUsage()
					return promptTokens, completionTokens, cachedTokens, nil
				}
				if translate && format == StreamFormatResponses {
					translated, model, usage, details, toolDeltas, ok := translateChatChunk(payload)
					if model != "" {
						respModel = model
					}
					if usage != nil {
						lastUsage = *usage
						sawUsage = true
					}
					if details != nil {
						lastUsageDetails = *details
					}
					for _, td := range toolDeltas {
						buf, seen := toolBuf[td.Index]
						if !seen {
							// Assign the fallback call id ONCE, at buffer creation:
							// deriving it lazily generated a fresh random id per call
							// site, so the streamed function_call events and the final
							// response.completed advertised DIFFERENT call_ids for the
							// same call and agent clients could not correlate them.
							buf = &bufferedToolCall{id: td.ID}
							if buf.id == "" {
								buf.id = "call_" + randomID()
							}
							toolBuf[td.Index] = buf
							toolOrder = append(toolOrder, td.Index)
						}
						if td.ID != "" {
							buf.id = td.ID
						}
						if td.Name != "" {
							buf.name = td.Name
						}
						buf.args += td.Arguments
					}
					if ok {
						for _, evt := range translated {
							out.WriteString("event: ")
							out.WriteString(evt.event)
							out.WriteString("\ndata: ")
							out.Write(evt.data)
							out.WriteString("\n\n")
						}
					}
				} else {
					out.Write(line)
				}
			} else {
				// Comments/keepalive lines — forward as-is.
				out.Write(line)
			}
			// Flush accumulated bytes.
			if ferr := flush(); ferr != nil {
				return promptTokens, completionTokens, cachedTokens, ferr
			}
		}
		if err != nil {
			if err != io.EOF {
				// Headers are long committed, so the client only sees an abrupt end;
				// report it so the caller can mark the account unhealthy.
				return promptTokens, completionTokens, cachedTokens, fmt.Errorf("upstream stream read error: %w", err)
			}
			break
		}
	}
	// Clean EOF with no sentinel: still owe the client the terminal events.
	emitTerminal()
	if ferr := flush(); ferr != nil {
		return promptTokens, completionTokens, cachedTokens, ferr
	}
	return promptTokens, completionTokens, cachedTokens, nil
}

// extractChunkUsage pulls prompt/completion/cached token counts from a single
// chat-completions SSE chunk's usage block. Returns nil for all when the chunk
// has no usage, or when the usage block carries no actual counts: providers that
// send `"usage":{}` or an all-zero block on the terminal frame used to CLOBBER
// the real counts captured from an earlier chunk, logging 0 prompt / 0
// completion alongside a non-zero cached_tokens. Mirrors extractChatUsage.
func extractChunkUsage(payload []byte) (*int, *int, *int) {
	var c struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Usage == nil {
		return nil, nil, nil
	}
	var cached *int
	if d := c.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
		v := d.CachedTokens
		cached = &v
	}
	if c.Usage.PromptTokens <= 0 && c.Usage.CompletionTokens <= 0 {
		return nil, nil, cached
	}
	pt, ct := c.Usage.PromptTokens, c.Usage.CompletionTokens
	return &pt, &ct, cached
}

type sseEvent struct {
	event string
	data  []byte
}

// toolCallDelta is one streaming fragment of a chat-completions tool_call.
type toolCallDelta struct {
	Index               int
	ID, Name, Arguments string
}

// bufferedToolCall accumulates one chat-completions streaming tool_call (deltas
// keyed by index) until the terminal [DONE] frame.
type bufferedToolCall struct {
	id, name, args string
}

// writeFunctionCallEvents emits the Responses-API event sequence for each
// buffered tool call: output_item.added → function_call_arguments.delta →
// function_call_arguments.done → output_item.done. Argument deltas arrive
// once (in full) at [DONE] rather than token-by-token — correctness over
// streaming latency: chat tool_call fragments can't be mapped incrementally
// without inventing item ids.
func writeFunctionCallEvents(out *bytes.Buffer, order []int, buf map[int]*bufferedToolCall) {
	for n, idx := range order {
		tc := buf[idx]
		itemID, callID := toolCallItemIDs(tc)
		added, _ := json.Marshal(map[string]any{
			"type":         "response.output_item.added",
			"output_index": n,
			"item": map[string]any{
				"id": itemID, "type": "function_call", "call_id": callID,
				"name": tc.name, "arguments": "",
			},
		})
		delta, _ := json.Marshal(map[string]any{
			"type":    "response.function_call_arguments.delta",
			"item_id": itemID, "output_index": n, "delta": tc.args,
		})
		done, _ := json.Marshal(map[string]any{
			"type":    "response.function_call_arguments.done",
			"item_id": itemID, "output_index": n, "arguments": tc.args,
		})
		itemDone, _ := json.Marshal(map[string]any{
			"type":         "response.output_item.done",
			"output_index": n,
			"item": map[string]any{
				"id": itemID, "type": "function_call", "call_id": callID,
				"name": tc.name, "arguments": tc.args, "status": "completed",
			},
		})
		for _, evt := range []sseEvent{
			{"response.output_item.added", added},
			{"response.function_call_arguments.delta", delta},
			{"response.function_call_arguments.done", done},
			{"response.output_item.done", itemDone},
		} {
			out.WriteString("event: ")
			out.WriteString(evt.event)
			out.WriteString("\ndata: ")
			out.Write(evt.data)
			out.WriteString("\n\n")
		}
	}
}

// functionCallOutputItems builds the function_call entries for the terminal
// response.completed event's output array (nil when no tool calls streamed).
func functionCallOutputItems(order []int, buf map[int]*bufferedToolCall) []any {
	if len(order) == 0 {
		return nil
	}
	items := make([]any, 0, len(order))
	for _, idx := range order {
		tc := buf[idx]
		itemID, callID := toolCallItemIDs(tc)
		items = append(items, map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": callID, "name": tc.name, "arguments": tc.args,
		})
	}
	return items
}

// toolCallItemIDs derives the responses-API ids from the chat tool_call id. Pure:
// the id is assigned once when the bufferedToolCall is created (including the
// random fallback for upstreams that omit it), so every call site — the streamed
// function_call events and the terminal response.completed — agrees.
func toolCallItemIDs(tc *bufferedToolCall) (itemID, callID string) {
	callID = tc.id
	if callID == "" {
		callID = "call_" + randomID()
	}
	return "fc_" + callID, callID
}

// usageDetails carries the prompt/completion token detail objects (cached_tokens,
// reasoning tokens, audio tokens…). Kept as raw JSON so we forward provider-specific
// fields verbatim instead of needing a struct for every vendor's variant.
type usageDetails struct {
	PromptTokensDetails     json.RawMessage `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails json.RawMessage `json:"completion_tokens_details,omitempty"`
}

// translateChatChunk converts one chat-completions SSE data payload into one or
// more responses-format events. toolDeltas carries any streamed tool_call
// fragments for the caller to buffer (they can't be translated statelessly).
func translateChatChunk(payload []byte) (events []sseEvent, model string, usage *struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}, details *usageDetails, toolDeltas []toolCallDelta, ok bool) {
	var chunk struct {
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Refusal          string `json:"refusal"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int             `json:"prompt_tokens"`
			CompletionTokens        int             `json:"completion_tokens"`
			TotalTokens             int             `json:"total_tokens"`
			PromptTokensDetails     json.RawMessage `json:"prompt_tokens_details"`
			CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, "", nil, nil, nil, false
	}
	model = chunk.Model
	if chunk.Usage != nil {
		usage = &struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		}{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
		details = &usageDetails{
			PromptTokensDetails:     chunk.Usage.PromptTokensDetails,
			CompletionTokensDetails: chunk.Usage.CompletionTokensDetails,
		}
	}
	for _, ch := range chunk.Choices {
		// The Responses API has no n>1 equivalent, and its delta events carry no
		// choice index — emitting every choice interleaved their text into one
		// indistinguishable stream. Only choice 0 is translated; ResponsesToChatRequest
		// no longer forwards n upstream, so this should not arise in practice.
		if ch.Index != 0 {
			continue
		}
		if ch.Delta.Content != "" {
			b, _ := json.Marshal(map[string]any{
				"type":  "response.output_text.delta",
				"delta": ch.Delta.Content,
			})
			events = append(events, sseEvent{event: "response.output_text.delta", data: b})
		}
		// A refusal is the model's actual answer; without this the client got an
		// empty response and no explanation. reasoning_content (DeepSeek/Qwen-style)
		// is surfaced on its own event rather than being merged into the answer text.
		if ch.Delta.Refusal != "" {
			b, _ := json.Marshal(map[string]any{
				"type":    "response.refusal.delta",
				"delta":   ch.Delta.Refusal,
				"refusal": ch.Delta.Refusal,
			})
			events = append(events, sseEvent{event: "response.refusal.delta", data: b})
		}
		if ch.Delta.ReasoningContent != "" {
			b, _ := json.Marshal(map[string]any{
				"type":  "response.reasoning_summary_text.delta",
				"delta": ch.Delta.ReasoningContent,
			})
			events = append(events, sseEvent{event: "response.reasoning_summary_text.delta", data: b})
		}
		for _, tc := range ch.Delta.ToolCalls {
			toolDeltas = append(toolDeltas, toolCallDelta{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" && usage == nil {
			// Some providers signal end without a usage frame; emit a final event placeholder.
			b, _ := json.Marshal(map[string]any{
				"type":          "response.output_text.done",
				"finish_reason": *ch.FinishReason,
			})
			events = append(events, sseEvent{event: "response.output_text.done", data: b})
		}
	}
	return events, model, usage, details, toolDeltas, len(events) > 0
}

func buildResponseCompleted(model string, usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}, details *usageDetails, sawUsage bool, outputItems []any) []byte {
	body := map[string]any{
		"id":         "resp_" + randomID(),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"model":      model,
		"status":     "completed",
	}
	if len(outputItems) > 0 {
		body["output"] = outputItems
	}
	if sawUsage {
		// Emit scalars plus any provider detail blobs (cached_tokens etc.) so IDEs
		// billing on cache reads see the savings instead of a stripped usage block.
		usageObj := map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.TotalTokens,
		}
		if details != nil {
			// Responses-API bodies name the detail blocks input_tokens_details /
			// output_tokens_details (NOT the chat-shaped prompt_/completion_
			// names) — the raw chunks arrive chat-shaped from the upstream, so
			// translate the keys here for Responses-native clients.
			if len(details.PromptTokensDetails) > 0 {
				usageObj["input_tokens_details"] = details.PromptTokensDetails
			}
			if len(details.CompletionTokensDetails) > 0 {
				usageObj["output_tokens_details"] = details.CompletionTokensDetails
			}
		}
		body["usage"] = usageObj
	}
	b, _ := json.Marshal(map[string]any{
		"type":     "response.completed",
		"response": body,
	})
	return b
}

// randomID produces a short unique suffix for log correlation.
func randomID() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
