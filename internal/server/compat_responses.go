package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// responses POST /v1/responses（OpenAI Responses API 形态）。
//
// 入站字段（本网关支持的子集）：
//   - model：必填，与 /v1/chat/completions 同语义（可写 cn:/global: 前缀或编排虚拟名）
//   - input：必填，字符串 或 [{role, content}] 数组（content 支持 input_text / input_image）
//   - instructions：可选，映射为 system 消息
//   - stream / max_output_tokens / temperature / top_p：可选，映射为 chat 同义字段
//
// 不支持的字段（tools / previous_response_id / reasoning 等）静默忽略：网关是
// 转发层，编造这些语义只会给出假成功。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := readAllBody(r)
	if err != nil {
		writeCompatErr(w, protoResponses, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, stream, model, msg := responsesToChat(raw)
	if msg != "" {
		writeCompatErr(w, protoResponses, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	h.compatRun(w, r, protoResponses, chatBody, stream, model)
}

// responsesToChat 把 Responses 请求体翻译成 chat 请求体。
// 返回翻译后的 body / 是否流式 / 模型名 / 错误文案（空串 = 成功）。
func responsesToChat(raw []byte) (chat []byte, stream bool, model, errMsg string) {
	var req struct {
		Model           string          `json:"model"`
		Input           json.RawMessage `json:"input"`
		Instructions    json.RawMessage `json:"instructions"`
		Stream          bool            `json:"stream"`
		MaxOutputTokens *int            `json:"max_output_tokens"`
		Temperature     *float64        `json:"temperature"`
		TopP            *float64        `json:"top_p"`
	}
	if json.Unmarshal(raw, &req) != nil {
		return nil, false, "", "invalid JSON body"
	}
	model = strings.TrimSpace(req.Model)
	if model == "" {
		return nil, false, "", "model is required"
	}
	msgs := make([]any, 0, 4)

	// instructions → system（字符串；块数组形态逐块拼文本）
	if len(req.Instructions) > 0 {
		var s string
		if json.Unmarshal(req.Instructions, &s) == nil {
			if strings.TrimSpace(s) != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": s})
			}
		} else {
			if txt := textOfBlocks(req.Instructions); txt != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": txt})
			}
		}
	}

	if len(req.Input) == 0 {
		return nil, false, "", "input is required"
	}
	var s string
	if json.Unmarshal(req.Input, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, false, "", "input is required"
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": s})
	} else {
		var items []map[string]any
		if json.Unmarshal(req.Input, &items) != nil {
			return nil, false, "", "input must be a string or an array of input items"
		}
		if len(items) == 0 {
			return nil, false, "", "input is required"
		}
		for _, it := range items {
			role, _ := it["role"].(string)
			switch role {
			case "user", "assistant", "system", "developer":
			default:
				role = "user" // 未知角色按 user 处理（Responses 的 input item 可省略 role）
			}
			c, ok := responsesContent(it["content"])
			if !ok {
				continue
			}
			msgs = append(msgs, map[string]any{"role": role, "content": c})
		}
	}
	if len(msgs) == 0 {
		return nil, false, "", "input is required"
	}

	out := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	b, _ := json.Marshal(out)
	return b, req.Stream, model, ""
}

// responsesContent 把 Responses 的 content（字符串 / 块数组）翻译成 chat content。
// 纯文本 → 字符串；含图片 → chat 的多段数组（text + image_url）。
func responsesContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return nil, false
		}
		return c, true
	case []any:
		texts := make([]string, 0, len(c))
		parts := make([]any, 0, len(c))
		hasImage := false
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text":
				if t, _ := m["text"].(string); t != "" {
					texts = append(texts, t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image", "image_url":
				url := ""
				if u, _ := m["image_url"].(string); u != "" {
					url = u
				} else if u, _ := m["url"].(string); u != "" {
					url = u
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
	case nil:
		return nil, false
	}
	return nil, false
}

// textOfBlocks 从 [{"type":"text","text":"..."}] 块数组取拼接文本（非该形态 → 空串）。
func textOfBlocks(raw json.RawMessage) string {
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if t, _ := b["text"].(string); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// writeCompatErr 按协议写出网关侧（未进入链路的）本地错误。
func writeCompatErr(w http.ResponseWriter, p compatProto, status int, code, msg string) {
	if p == protoResponses {
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code},
		})
		return
	}
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrType(status), "message": msg},
	})
}

// readAllBody 读取请求体（与 chat 路径同口径：读失败就地 400）。
func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte("{}"), nil
	}
	return io.ReadAll(r.Body)
}
