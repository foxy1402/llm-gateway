package proxy

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// ResponsesToChatRequest converts a /v1/responses request body into a
// /v1/chat/completions body targeting upstreamModel.
//
// Mapping:
//   - instructions → system message (prepended)
//   - input (string) → user message with that text
//   - input (array) → each item mapped:
//     {type:"message", role, content:[{type:"input_text",text}...]} → {role, content:text}
//     {type:"message", role, content:[...,{type:"input_image",image_url:"..."}]} →
//     {role, content:[{type:"text",text},...,{type:"image_url",image_url:{url}}]}
//     (chat-completions array content — see #vision).
//     {type:"function_call", call_id, name, arguments} → assistant message with
//     chat-completions tool_calls (id ← call_id).
//     {type:"function_call_output", call_id, output} → {role:"tool",
//     tool_call_id:call_id, name:<resolved from the matching function_call>,
//     content:output}. Strict tool backends (Kimi K3) reject tool messages whose
//     tool name cannot be resolved, so the name is carried when known.
//   - tools are CONVERTED from the responses shape
//     {type:"function", name, description, parameters} to the chat shape
//     {type:"function", function:{name, ...}} — forwarding them verbatim makes
//     tool-capable providers reject the request or silently drop tool calling.
//   - stream, temperature, top_p, max_output_tokens etc. pass through when they
//     have a chat-completions analogue.
func ResponsesToChatRequest(body []byte, upstreamModel string) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	// Content is `any` (not `string`): text-only messages still marshal as a
	// plain string (chat-completions' simple form), but a message containing an
	// input_image part marshals as an array so the image survives translation
	// instead of being silently dropped (#vision).
	type chatMessage struct {
		Role       string `json:"role"`
		Content    any    `json:"content"`
		ToolCalls  any    `json:"tool_calls,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
		Name       string `json:"name,omitempty"`
	}
	messages := []chatMessage{}
	// call_id → tool name, so each tool message can carry the name strict
	// backends (Kimi K3) need to resolve it.
	callNames := map[string]string{}

	if raw, ok := in["instructions"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			messages = append(messages, chatMessage{Role: "system", Content: s})
		}
	}

	if raw, ok := in["input"]; ok {
		// Try string first.
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			messages = append(messages, chatMessage{Role: "user", Content: s})
		} else {
			// Array of inputs.
			var arr []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, fmt.Errorf("input must be string or array")
			}
			for _, item := range arr {
				var typ string
				if t, ok := item["type"]; ok {
					_ = json.Unmarshal(t, &typ)
				}
				switch typ {
				case "function_call":
					var fc struct {
						CallID    string `json:"call_id"`
						ID        string `json:"id"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}
					if raw, err := json.Marshal(item); err == nil {
						_ = json.Unmarshal(raw, &fc)
					}
					id := fc.CallID
					if id == "" {
						id = fc.ID
					}
					messages = append(messages, chatMessage{
						Role:    "assistant",
						Content: nil,
						ToolCalls: []map[string]any{{
							"id":   id,
							"type": "function",
							"function": map[string]any{
								"name":      fc.Name,
								"arguments": fc.Arguments,
							},
						}},
					})
					if id != "" && fc.Name != "" {
						callNames[id] = fc.Name
					}
				case "function_call_output":
					var fo struct {
						CallID string          `json:"call_id"`
						Output json.RawMessage `json:"output"`
					}
					if raw, err := json.Marshal(item); err == nil {
						_ = json.Unmarshal(raw, &fo)
					}
					// Chat tool messages want string content.
					var content any = ""
					var os string
					if len(fo.Output) > 0 && json.Unmarshal(fo.Output, &os) == nil {
						content = os
					} else if len(fo.Output) > 0 {
						content = string(fo.Output) // objects/arrays forwarded as JSON text
					}
					messages = append(messages, chatMessage{
						Role:       "tool",
						Content:    content,
						ToolCallID: fo.CallID,
						Name:       callNames[fo.CallID],
					})
				case "item_reference":
					// References an item on OpenAI's server-side storage — no
					// chat-completions equivalent; skip rather than emit a ghost
					// message with empty content.
					slog.Warn("dropping item_reference input item (server-side items have no chat-completions equivalent)")
				default: // "", "message", or anything else carrying role/content
					role := "user"
					if r, ok := item["role"]; ok {
						_ = json.Unmarshal(r, &role)
					}
					var content any = ""
					if c, ok := item["content"]; ok {
						// String content.
						var cs string
						if err := json.Unmarshal(c, &cs); err == nil {
							content = cs
						} else {
							// Array of {type:"input_text"|"text",text} or
							// {type:"input_image",image_url,detail} parts.
							var parts []map[string]json.RawMessage
							if err := json.Unmarshal(c, &parts); err == nil {
								content = translateContentParts(parts)
							}
						}
					}
					messages = append(messages, chatMessage{Role: role, Content: content})
				}
			}
		}
	}

	out := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
	}
	// Pass-throughs.
	if v, ok := in["stream"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			out["stream"] = b
		}
	}
	// Tools need shape conversion, not passthrough: a verbatim forward of
	// {type:"function", name, ...} breaks every chat-completions upstream.
	if v, ok := in["tools"]; ok {
		if converted := convertResponsesTools(v); converted != nil {
			out["tools"] = converted
		}
	}
	// Pass-through identical-key fields. These are what IDE clients (Cursor, Copilot,
	// etc.) rely on for tool use, structured output, and determinism — dropping them
	// silently degrades agentic features.
	for _, key := range []string{
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"tool_choice", "parallel_tool_calls",
		"response_format", "logit_bias", "stop", "n", "seed", "user",
	} {
		if v, ok := in[key]; ok {
			out[key] = json.RawMessage(v)
		}
	}
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	return json.Marshal(out)
}

// convertResponsesTools rewrites a Responses-API tools array into the
// chat-completions shape:
//
//	{"type":"function","name":"f","description":"...","parameters":{...},"strict":true}
//	  → {"type":"function","function":{"name":"f","description":"...","parameters":{...},"strict":true}}
//
// Tools already in the chat shape (nested "function" key) pass through with
// their exact bytes. Responses-only built-in tools (web_search, file_search,
// code_interpreter, computer_use, mcp, local_shell, image_generation) have no
// chat-completions equivalent and are DROPPED with a warning — forwarding them
// verbatim makes tool-capable upstreams 400 the whole request.
// Returns nil when nothing usable remains, so the caller omits the key.
func convertResponsesTools(raw json.RawMessage) json.RawMessage {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw // not an array — forward untouched rather than destroy it
	}
	out := make([]map[string]any, 0, len(tools))
	changed := false
	for _, t := range tools {
		if _, nested := t["function"]; nested {
			m := make(map[string]any, len(t))
			for k, v := range t {
				m[k] = json.RawMessage(v)
			}
			out = append(out, m)
			continue
		}
		var typ string
		if tv, ok := t["type"]; ok {
			_ = json.Unmarshal(tv, &typ)
		}
		if typ != "function" {
			_, hasName := t["name"]
			if !hasName {
				slog.Warn("dropping responses built-in tool with no chat-completions equivalent", "type", typ)
				continue
			}
		}
		inner := make(map[string]any, len(t))
		for k, v := range t {
			if k != "type" {
				inner[k] = json.RawMessage(v)
			}
		}
		out = append(out, map[string]any{"type": "function", "function": inner})
		changed = true
	}
	if len(out) == 0 {
		return nil
	}
	if !changed {
		return raw // all chat-shaped already: keep the original bytes
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// translateContentParts converts a Responses-API content-part array (input_text
// / input_image parts) into either:
//   - a plain concatenated string, when every part is text — matches the
//     pre-existing legacy behavior so text-only messages still produce the
//     simple chat-completions string form callers/tests expect, or
//   - a chat-completions-style content ARRAY (text parts as
//     {"type":"text","text":...}, images as
//     {"type":"image_url","image_url":{"url":...}}), when any part is an
//     input_image — this is what keeps vision/OCR requests from silently
//     losing their image when routed through /v1/responses to a provider
//     that only speaks chat-completions natively.
//
// Responses-API image parts carry image_url as a bare string (URL or base64
// data URL); chat-completions nests it under image_url.url — see
// https://developers.openai.com/api/docs/guides/images-vision.
func translateContentParts(parts []map[string]json.RawMessage) any {
	hasImage := false
	for _, part := range parts {
		if t, ok := part["type"]; ok {
			var typ string
			if json.Unmarshal(t, &typ) == nil && typ == "input_image" {
				hasImage = true
				break
			}
		}
	}
	if !hasImage {
		text := ""
		for _, part := range parts {
			if t, ok := part["text"]; ok {
				var txt string
				if json.Unmarshal(t, &txt) == nil {
					if text != "" {
						text += "\n"
					}
					text += txt
				}
			}
		}
		return text
	}

	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		var typ string
		if t, ok := part["type"]; ok {
			_ = json.Unmarshal(t, &typ)
		}
		if typ == "input_image" {
			var url string
			if u, ok := part["image_url"]; ok {
				_ = json.Unmarshal(u, &url)
			}
			if url == "" {
				// file_id-based images reference an upload on OpenAI's own storage,
				// which has no chat-completions equivalent for a third-party
				// upstream — drop with a warning instead of forwarding a broken part.
				slog.Warn("dropping input_image part with no image_url (file_id references are not supported by chat-completions translation)")
				continue
			}
			imageURL := map[string]any{"url": url}
			if d, ok := part["detail"]; ok {
				var detail string
				if json.Unmarshal(d, &detail) == nil && detail != "" {
					imageURL["detail"] = detail
				}
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": imageURL})
			continue
		}
		// input_text / text (and anything else carrying a plain "text" field).
		if t, ok := part["text"]; ok {
			var txt string
			if json.Unmarshal(t, &txt) == nil {
				out = append(out, map[string]any{"type": "text", "text": txt})
			}
		}
	}
	return out
}

// extractChatMessageText pulls plain text out of a chat-completions message's
// "content" field. Per spec this is normally a string, but some providers
// return an array of {"type","text"} parts for structured/refusal responses;
// tolerating that shape here means a provider doing so doesn't hard-fail the
// whole /v1/responses translation.
func extractChatMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	text := ""
	for _, part := range parts {
		if t, ok := part["text"]; ok {
			var txt string
			if json.Unmarshal(t, &txt) == nil {
				if text != "" {
					text += "\n"
				}
				text += txt
			}
		}
	}
	return text
}

// ChatToResponsesResponse converts a /v1/chat/completions JSON response into
// a /v1/responses-shaped payload addressed to originalModel.
func ChatToResponsesResponse(body []byte, originalModel string) ([]byte, error) {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int             `json:"prompt_tokens"`
			CompletionTokens        int             `json:"completion_tokens"`
			TotalTokens             int             `json:"total_tokens"`
			PromptTokensDetails     json.RawMessage `json:"prompt_tokens_details"`
			CompletionTokensDetails json.RawMessage `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}

	text := ""
	if len(chat.Choices) > 0 {
		text = extractChatMessageText(chat.Choices[0].Message.Content)
	}
	created := chat.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	id := chat.ID
	if id == "" {
		id = "resp_" + randomRespID()
	} else {
		id = "resp_" + id
	}

	// Emit one output item per function call, plus the text message item when
	// there is text. Dropping tool_calls here is what made agent clients on
	// /v1/responses never see the calls a chat-only upstream returned.
	output := []any{}
	if text != "" {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "output_text",
					"text": text,
				},
			},
		})
	}
	if len(chat.Choices) > 0 {
		for _, tc := range chat.Choices[0].Message.ToolCalls {
			callID := tc.ID
			if callID == "" {
				callID = "call_" + randomRespID()
			}
			output = append(output, map[string]any{
				"id":        "fc_" + randomRespID(),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   callID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			})
		}
	}
	if len(output) == 0 {
		// Response object must never carry an empty output array.
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "output_text",
					"text": "",
				},
			},
		})
	}

	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"model":      originalModel,
		"status":     "completed",
		"output":     output,
	}
	usageObj := map[string]any{
		"input_tokens":  chat.Usage.PromptTokens,
		"output_tokens": chat.Usage.CompletionTokens,
		"total_tokens":  chat.Usage.TotalTokens,
	}
	// Preserve cache/reasoning detail blobs (cached_tokens for OpenAI-compatible
	// billing visibility; reasoning_tokens; audio_tokens…). Raw forward so any
	// vendor-specific key under the details objects survives translation.
	if len(chat.Usage.PromptTokensDetails) > 0 {
		usageObj["prompt_tokens_details"] = chat.Usage.PromptTokensDetails
	}
	if len(chat.Usage.CompletionTokensDetails) > 0 {
		usageObj["completion_tokens_details"] = chat.Usage.CompletionTokensDetails
	}
	out["usage"] = usageObj
	if len(chat.Choices) > 0 && chat.Choices[0].FinishReason != "" {
		out["finish_reason"] = chat.Choices[0].FinishReason
	}
	return json.Marshal(out)
}

func randomRespID() string {
	b := make([]byte, 12)
	_, _ = crand.Read(b)
	return fmt.Sprintf("%x", b)
}
