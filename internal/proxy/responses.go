package proxy

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// ChatRequestToResponsesRequest converts a /v1/chat/completions request body
// into a /v1/responses body targeting the same model — the mirror image of
// ResponsesToChatRequest, used by proxy.tryResponsesRerouteHeal for
// models/upstreams whose chat-completions endpoint rejects tool calls
// outright (observed live: Lightning AI's "openai/gpt-5.6-sol"). stream is
// always forced false: the reroute always dispatches non-streaming upstream
// regardless of what the client asked for (see chatResponseToSingleShotSSE).
func ChatRequestToResponsesRequest(body []byte) ([]byte, error) {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(body, &full); err != nil {
		return nil, fmt.Errorf("parse chat request: %w", err)
	}
	var in struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat request: %w", err)
	}

	type chatToolCall struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	type chatMsg struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []chatToolCall  `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}

	var instructions []string
	input := []any{}
	for _, raw := range in.Messages {
		var m chatMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch m.Role {
		case "system":
			if s := extractChatMessageText(m.Content); s != "" {
				instructions = append(instructions, s)
			}
		case "tool":
			var content any = ""
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				content = s
			} else if len(m.Content) > 0 {
				content = string(m.Content)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  content,
			})
		default: // "user", "assistant"
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				continue
			}
			textType := "input_text"
			if m.Role == "assistant" {
				textType = "output_text"
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    m.Role,
				"content": chatContentToResponsesParts(m.Content, textType),
			})
		}
	}

	out := map[string]any{
		"model":  in.Model,
		"input":  input,
		"stream": false,
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n")
	}
	if v, ok := full["tools"]; ok {
		if converted := convertChatToolsToResponses(v); converted != nil {
			out["tools"] = converted
		}
	}
	for _, key := range []string{"tool_choice", "temperature", "top_p", "parallel_tool_calls", "user"} {
		if v, ok := full[key]; ok {
			out[key] = json.RawMessage(v)
		}
	}
	if v, ok := full["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := full["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	return json.Marshal(out)
}

// chatContentToResponsesParts converts a chat-completions message "content"
// field (string or array of {type:"text"}/{type:"image_url"} parts) into a
// Responses-API content-part array. textType is "input_text" for user/system
// messages or "output_text" for assistant messages being replayed as prior
// conversation turns — the Responses API distinguishes the two.
func chatContentToResponsesParts(raw json.RawMessage, textType string) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []any{map[string]any{"type": textType, "text": s}}
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return []any{map[string]any{"type": textType, "text": ""}}
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		var typ string
		if t, ok := part["type"]; ok {
			_ = json.Unmarshal(t, &typ)
		}
		if typ == "image_url" {
			var iu struct {
				URL    string `json:"url"`
				Detail string `json:"detail"`
			}
			if u, ok := part["image_url"]; ok {
				_ = json.Unmarshal(u, &iu)
			}
			item := map[string]any{"type": "input_image", "image_url": iu.URL}
			if iu.Detail != "" {
				item["detail"] = iu.Detail
			}
			out = append(out, item)
			continue
		}
		var txt string
		if t, ok := part["text"]; ok {
			_ = json.Unmarshal(t, &txt)
		}
		out = append(out, map[string]any{"type": textType, "text": txt})
	}
	return out
}

// convertChatToolsToResponses rewrites a chat-completions tools array
// ({"type":"function","function":{name,description,parameters,...}}) into the
// Responses-API shape ({"type":"function",name,description,parameters,...})
// — the mirror image of convertResponsesTools. Tools already flat (no nested
// "function" key) pass through verbatim. Returns nil when nothing usable
// remains, so the caller omits the key.
func convertChatToolsToResponses(raw json.RawMessage) json.RawMessage {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fnRaw, hasFn := t["function"]
		if !hasFn {
			m := make(map[string]any, len(t))
			for k, v := range t {
				m[k] = json.RawMessage(v)
			}
			out = append(out, m)
			continue
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(fnRaw, &fn); err != nil {
			continue
		}
		item := map[string]any{"type": "function"}
		for k, v := range fn {
			item[k] = json.RawMessage(v)
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// ResponsesResponseToChatResponse converts a /v1/responses JSON response into
// a /v1/chat/completions-shaped payload — the mirror image of
// ChatToResponsesResponse, used by proxy.tryResponsesRerouteHeal so the
// client (which called chat.completions) never sees the Responses shape.
func ResponsesResponseToChatResponse(body []byte) ([]byte, error) {
	var resp struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		Model     string `json:"model"`
		Output    []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails *struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}

	text := ""
	var toolCalls []map[string]any
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Text == "" {
					continue
				}
				if text != "" {
					text += "\n"
				}
				text += c.Text
			}
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = "call_" + randomRespID()
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": item.Arguments,
				},
			})
		}
	}

	created := resp.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	id := resp.ID
	switch {
	case strings.HasPrefix(id, "resp_"):
		id = "chatcmpl-" + id[len("resp_"):]
	case id == "":
		id = "chatcmpl-" + randomRespID()
	}

	finishReason := "stop"
	message := map[string]any{"role": "assistant", "content": text}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = toolCalls
		message["content"] = nil
	}

	usageObj := map[string]any{
		"prompt_tokens":     resp.Usage.InputTokens,
		"completion_tokens": resp.Usage.OutputTokens,
		"total_tokens":      resp.Usage.TotalTokens,
	}
	// Translate the Responses-API cache/reasoning detail blocks into their
	// chat-completions equivalents — dropping these here would silently hide
	// prompt-cache savings (cached_tokens) and reasoning-token accounting for
	// every request this reroute heal touches, even though the upstream
	// reported them (see tryResponsesRerouteHeal).
	if d := resp.Usage.InputTokensDetails; d != nil {
		details := map[string]any{"cached_tokens": d.CachedTokens}
		if d.CacheWriteTokens > 0 {
			details["cache_write_tokens"] = d.CacheWriteTokens
		}
		usageObj["prompt_tokens_details"] = details
	}
	if d := resp.Usage.OutputTokensDetails; d != nil {
		usageObj["completion_tokens_details"] = map[string]any{"reasoning_tokens": d.ReasoningTokens}
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   resp.Model,
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": usageObj,
	}
	return json.Marshal(out)
}

func randomRespID() string {
	b := make([]byte, 12)
	_, _ = crand.Read(b)
	return fmt.Sprintf("%x", b)
}
