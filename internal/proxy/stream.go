package proxy

import (
	"bufio"
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// the body instead of sending the sentinel.
func (p *Proxy) streamResponse(w http.ResponseWriter, upstream *http.Response, format StreamFormat, translate bool) (*int, *int, *int, error) {
	// SSE headers.
	// Note: if translate is false, we forward chat-completions stream bytes as-is.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(upstream.StatusCode)

	// Flush via ResponseController so wrapping middleware (the request logger's
	// statusWriter, the fail-ban statusCapture) can never silently swallow
	// streaming flushes again — a direct w.(http.Flusher) assertion returns nil
	// through those wrappers and tokens arrive in 2 KiB bursts.
	rc := http.NewResponseController(w)
	_ = rc.Flush()

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
	// a timer that resets on every successful read.
	type readResult struct {
		line []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		for {
			line, err := src.ReadBytes('\n')
			readCh <- readResult{line, err}
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
		}
		line, err := rr.line, rr.err
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data: ")) {
				payload := bytes.TrimPrefix(trimmed, []byte("data: "))
				// #12: extract usage from every chunk before translation. Upstream
				// chunks are always chat-completions format (prompt_tokens/completion_tokens),
				// even when we later translate them to the responses shape for the client.
				if pt, ct, cached := extractChunkUsage(payload); pt != nil || ct != nil || cached != nil {
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
				if bytes.Equal(payload, []byte("[DONE]")) {
					if translate && format == StreamFormatResponses {
						// Emit buffered tool calls as function_call events…
						writeFunctionCallEvents(&out, toolOrder, toolBuf)
						// …then the terminal response.completed carrying them in
						// the output array.
						completed := buildResponseCompleted(respModel, lastUsage, &lastUsageDetails, sawUsage, functionCallOutputItems(toolOrder, toolBuf))
						out.WriteString("event: response.completed\n")
						out.WriteString("data: ")
						out.Write(completed)
						out.WriteString("\n\n")
					} else {
						out.Write(line)
					}
				} else {
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
								buf = &bufferedToolCall{}
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
				}
			} else {
				// Comments/keepalive lines — forward as-is.
				out.Write(line)
			}
			// Flush accumulated bytes.
			if out.Len() > 0 {
				_, _ = w.Write(out.Bytes())
				out.Reset()
				_ = rc.Flush()
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
	return promptTokens, completionTokens, cachedTokens, nil
}

// extractChunkUsage pulls prompt/completion/cached token counts from a single
// chat-completions SSE chunk's usage block. Returns nil for all when the chunk
// has no usage.
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
	pt, ct := c.Usage.PromptTokens, c.Usage.CompletionTokens
	var cached *int
	if d := c.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
		v := d.CachedTokens
		cached = &v
	}
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

// toolCallItemIDs derives stable responses-API ids from the chat tool_call id
// (falling back to a random one when the upstream omitted it).
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
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
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
		if ch.Delta.Content != "" {
			b, _ := json.Marshal(map[string]any{
				"type":  "response.output_text.delta",
				"delta": ch.Delta.Content,
			})
			events = append(events, sseEvent{event: "response.output_text.delta", data: b})
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
			if len(details.PromptTokensDetails) > 0 {
				usageObj["prompt_tokens_details"] = details.PromptTokensDetails
			}
			if len(details.CompletionTokensDetails) > 0 {
				usageObj["completion_tokens_details"] = details.CompletionTokensDetails
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
