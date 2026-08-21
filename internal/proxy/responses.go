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
//   - stream, temperature, top_p, max_output_tokens, tools etc. pass through when
//     they have a chat-completions analogue.
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
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	messages := []chatMessage{}

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
	// Pass-through identical-key fields. These are what IDE clients (Cursor, Copilot,
	// etc.) rely on for tool use, structured output, and determinism — dropping them
	// silently degrades agentic features.
	for _, key := range []string{
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"tools", "tool_choice", "parallel_tool_calls",
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
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
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

	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"model":      originalModel,
		"status":     "completed",
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": text,
					},
				},
			},
		},
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
