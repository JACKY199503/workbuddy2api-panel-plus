package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// messages POST /v1/messages（Anthropic Messages API 形态）。
//
// 入站字段：
//   - model：必填
//   - messages：必填，[{role: user|assistant, content: string | blocks}]
//   - system：可选，字符串或块数组 → 前置 system 消息
//   - max_tokens / temperature / top_p / stop_sequences / stream：可选
//
// 响应按 Anthropic 形态：{id,type:"message",role:"assistant",content:[{type:"text"}],
// stop_reason,usage:{input_tokens,output_tokens}}；流式走 Anthropic SSE 事件序列
// （message_start → content_block_start → ping → content_block_delta… →
// content_block_stop → message_delta → message_stop）。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := readAllBody(r)
	if err != nil {
		writeCompatErr(w, protoMessages, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, stream, model, msg := messagesToChat(raw)
	if msg != "" {
		writeCompatErr(w, protoMessages, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	h.compatRun(w, r, protoMessages, chatBody, stream, model)
}

// messagesToChat 把 Anthropic 请求体翻译成 chat 请求体。
func messagesToChat(raw []byte) (chat []byte, stream bool, model, errMsg string) {
	var req struct {
		Model         string            `json:"model"`
		Messages      []json.RawMessage `json:"messages"`
		System        json.RawMessage   `json:"system"`
		MaxTokens     *int              `json:"max_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		StopSequences []string          `json:"stop_sequences"`
		Stream        bool              `json:"stream"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return nil, false, "", "invalid JSON body"
	}
	model = strings.TrimSpace(req.Model)
	if model == "" {
		return nil, false, "", "model is required"
	}
	msgs := make([]any, 0, len(req.Messages)+1)

	// system：字符串或 [{type:"text",text}] 块数组 → 前置 system 消息
	if len(req.System) > 0 {
		var s string
		if json.Unmarshal(req.System, &s) == nil {
			if strings.TrimSpace(s) != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": s})
			}
		} else if txt := textOfBlocks(req.System); txt != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": txt})
		}
	}

	if len(req.Messages) == 0 {
		return nil, false, "", "messages is required"
	}
	for _, rm := range req.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rm, &m) != nil {
			return nil, false, "", "invalid message item"
		}
		role := m.Role
		switch role {
		case "user", "assistant":
		case "system":
			// Anthropic 规范里 system 只在顶层；放到 messages 里按 system 处理（宽松兼容）。
		default:
			return nil, false, "", "invalid role: " + role
		}
		c, ok := anthropicContent(m.Content)
		if !ok {
			continue
		}
		msgs = append(msgs, map[string]any{"role": role, "content": c})
	}
	if len(msgs) == 0 {
		return nil, false, "", "messages is required"
	}

	out := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	b, _ := json.Marshal(out)
	return b, req.Stream, model, ""
}

// anthropicContent 把 Anthropic content（字符串 / 块数组）翻译成 chat content。
// 纯文本 → 字符串；含图片（source.type=base64 的 data URL）→ chat 多段数组。
func anthropicContent(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, false
		}
		return s, true
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	texts := make([]string, 0, len(blocks))
	parts := make([]any, 0, len(blocks))
	hasImage := false
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text":
			if t, _ := b["text"].(string); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			// Anthropic: source{type:"base64"|"url", media_type, data}
			url := ""
			if src, ok := b["source"].(map[string]any); ok {
				st, _ := src["type"].(string)
				switch st {
				case "base64":
					if d, _ := src["data"].(string); d != "" {
						mt, _ := src["media_type"].(string)
						if mt == "" {
							mt = "image/png"
						}
						url = "data:" + mt + ";base64," + d
					}
				case "url":
					url, _ = src["url"].(string)
				}
			}
			if url == "" {
				continue
			}
			hasImage = true
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	if hasImage {
		if len(parts) == 0 {
			return nil, false
		}
		return parts, true
	}
	if len(texts) == 0 {
		return nil, false
	}
	return strings.Join(texts, "\n"), true
}
