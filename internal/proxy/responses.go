package proxy

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// ResponsesToChatRequest converts a /v1/responses request body into a
// /v1/chat/completions body targeting upstreamModel.
//
// Mapping:
//   - instructions → system message (prepended)
//   - input (string) → user message with that text
//   - input (array) → each item mapped:
//       {type:"message", role, content:[{type:"input_text",text}...]} → {role, content:text}
//   - stream, temperature, top_p, max_output_tokens, tools etc. pass through when
//     they have a chat-completions analogue.
func ResponsesToChatRequest(body []byte, upstreamModel string) ([]byte, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
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
				content := ""
				if c, ok := item["content"]; ok {
					// String content.
					var cs string
					if err := json.Unmarshal(c, &cs); err == nil {
						content = cs
					} else {
						// Array of {type:"input_text"|"text", text:"..."} parts.
						var parts []map[string]json.RawMessage
						if err := json.Unmarshal(c, &parts); err == nil {
							for _, part := range parts {
								if t, ok := part["text"]; ok {
									var txt string
									if json.Unmarshal(t, &txt) == nil {
										if content != "" {
											content += "\n"
										}
										content += txt
									}
								}
							}
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

// ChatToResponsesResponse converts a /v1/chat/completions JSON response into
// a /v1/responses-shaped payload addressed to originalModel.
func ChatToResponsesResponse(body []byte, originalModel string) ([]byte, error) {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
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
		text = chat.Choices[0].Message.Content
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
