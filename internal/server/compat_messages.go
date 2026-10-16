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
//   - tools：可选，[{name, description, input_schema}] → OpenAI tools
//   - tool_choice：可选，{type: auto|any|tool|none} → OpenAI tool_choice
//   - max_tokens / temperature / top_p / stop_sequences / stream：可选
//
// 响应按 Anthropic 形态：{id,type:"message",role:"assistant",content:[{type:"text"} |
// {type:"tool_use"}],stop_reason,usage:{input_tokens,output_tokens}}；流式走 Anthropic
// SSE 事件序列（message_start → ping → content_block_start/delta/stop… →
// message_delta → message_stop）。
//
// 工具调用是 Claude Code 等 agent 客户端的命脉：入站必须把 tools / tool_choice /
// assistant.tool_use / user.tool_result 全部翻译成 OpenAI 形态，否则上游不会返回
// tool_calls，客户端拿到 stop_reason=end_turn 会判定「这轮已结束」直接收工
// （issue #2：任务几秒就中断，实际什么都没做）。
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
		TopK          *int              `json:"top_k"`
		StopSequences []string          `json:"stop_sequences"`
		Stream        bool              `json:"stream"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
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
		expanded, ok := anthropicMessageToChat(role, m.Content)
		if !ok {
			continue
		}
		msgs = append(msgs, expanded...)
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
	// top_k 是 Anthropic 独有字段（OpenAI 规范没有）：上游实测忽略未知字段（200），
	// 透传不产生 400；不支持的模型自然无效果，好过静默丢弃参数。
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	// tools / tool_choice：无 tools 时 tool_choice 无意义（上游会 400），整体不下发。
	if tools := anthropicToolsToChat(req.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	b, _ := json.Marshal(out)
	return b, req.Stream, model, ""
}

// anthropicMessageToChat 把一条 Anthropic 消息翻译成 1~N 条 chat 消息。
// 展开场景：user 消息里混有 tool_result（→ 独立 role:tool 消息）。
func anthropicMessageToChat(role string, raw json.RawMessage) ([]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, false
		}
		return []any{map[string]any{"role": role, "content": s}}, true
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	switch role {
	case "user":
		return anthropicUserBlocks(blocks)
	case "assistant":
		return anthropicAssistantBlocks(blocks)
	default:
		txt := joinBlockTexts(blocks)
		if txt == "" {
			return nil, false
		}
		return []any{map[string]any{"role": "system", "content": txt}}, true
	}
}

// anthropicUserBlocks 处理 user 消息的块数组：
//   - tool_result → 独立 role:tool 消息（tool_call_id = tool_use_id）
//   - text / image → 合并成一条 user 消息
//
// tool 结果必须先于同批的文本消息落进 wire（OpenAI 要求 tool 结果紧跟 assistant.tool_calls，
// 中间插入 user 文本会判配对断裂；链路的 repack 会兜底，这里按规范顺序输出）。
func anthropicUserBlocks(blocks []map[string]any) ([]any, bool) {
	out := make([]any, 0, 2)
	texts := make([]string, 0, len(blocks))
	parts := make([]any, 0, len(blocks))
	hasImage := false
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "tool_result":
			id, _ := b["tool_use_id"].(string)
			if id == "" {
				continue
			}
			txt := toolResultText(b["content"])
			// is_error=true 的工具结果必须让模型看得见：OpenAI 的 role:tool 没有
			// 对应字段，直接透传会把它当成一次正常返回，模型据此继续编造成功结果。
			// 在正文前打标记（不删原内容）——这是唯一的无损携带方式。
			if e, _ := b["is_error"].(bool); e {
				txt = "[tool_error] " + txt
			}
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": id, "content": txt,
			})
		case "text":
			if t, _ := b["text"].(string); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			url := anthropicImageURL(b)
			if url == "" {
				continue
			}
			hasImage = true
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	if hasImage && len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": parts})
	} else if len(texts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// anthropicAssistantBlocks 处理 assistant 消息的块数组：
//   - tool_use → OpenAI tool_calls（arguments 为字符串化 JSON）
//   - text → 合并为 content（无文本时给空串，工具调用轮允许空正文）
func anthropicAssistantBlocks(blocks []map[string]any) ([]any, bool) {
	texts := make([]string, 0, len(blocks))
	calls := make([]any, 0, len(blocks))
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "tool_use":
			name, _ := b["name"].(string)
			if name == "" {
				continue
			}
			id, _ := b["id"].(string)
			if id == "" {
				id = newCompatID("toolu_")
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": toolArgsJSON(b["input"])},
			})
		case "text":
			if t, _ := b["text"].(string); t != "" {
				texts = append(texts, t)
			}
		}
	}
	txt := strings.Join(texts, "\n")
	if len(calls) > 0 {
		return []any{map[string]any{"role": "assistant", "content": txt, "tool_calls": calls}}, true
	}
	if txt == "" {
		return nil, false
	}
	return []any{map[string]any{"role": "assistant", "content": txt}}, true
}

// toolArgsJSON 把 Anthropic tool_use.input（对象 / 字符串 / 空）规范成字符串化 JSON。
func toolArgsJSON(v any) string {
	switch in := v.(type) {
	case string:
		if strings.TrimSpace(in) != "" {
			return in
		}
	case nil:
	default:
		if b, err := json.Marshal(in); err == nil {
			return string(b)
		}
	}
	return "{}"
}

// toolResultText 把 tool_result.content（字符串 / 块数组 / 其他）拍平成文本。
func toolResultText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, it := range c {
			if m, ok := it.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
				continue
			}
			if s, ok := it.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

// anthropicImageURL 从 Anthropic image 块取 data URL / 远端 URL。
func anthropicImageURL(b map[string]any) string {
	src, ok := b["source"].(map[string]any)
	if !ok {
		return ""
	}
	st, _ := src["type"].(string)
	switch st {
	case "base64":
		d, _ := src["data"].(string)
		if d == "" {
			return ""
		}
		mt, _ := src["media_type"].(string)
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + d
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

// joinBlockTexts 拼接块数组里所有 text 块的文本（thinking 等非文本块忽略）。
func joinBlockTexts(blocks []map[string]any) string {
	var sb strings.Builder
	for _, b := range blocks {
		if t, _ := b["text"].(string); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// anthropicToolsToChat 把 Anthropic tools（{name, description, input_schema}）
// 翻译成 OpenAI tools（{type:"function", function:{name, description, parameters}}）。
// 形态不对 / 缺 name 的工具直接跳过（不带坏整批）。
func anthropicToolsToChat(raws []json.RawMessage) []any {
	if len(raws) == 0 {
		return nil
	}
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		var t struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
			// 少数客户端发 OpenAI 形态（已带 function 子树），一并兼容。
			Function map[string]any `json:"function"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		name := strings.TrimSpace(t.Name)
		desc := t.Description
		schema := t.InputSchema
		if fn := t.Function; fn != nil && name == "" {
			if n, _ := fn["name"].(string); n != "" {
				name = strings.TrimSpace(n)
			}
			if d, _ := fn["description"].(string); d != "" && desc == "" {
				desc = d
			}
			if p, ok := fn["parameters"].(map[string]any); ok && schema == nil {
				schema = p
			}
		}
		if name == "" {
			continue
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": desc, "parameters": schema,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoice 把 Anthropic tool_choice 翻译成 OpenAI 形态。
//   - "auto" / {type:"auto"} → "auto"
//   - "any"  / {type:"any"}  → "required"（强制至少调一次工具）
//   - {type:"tool", name}    → {type:"function", function:{name}}
//   - "none" / {type:"none"} → "none"（链路会把 tools 一并摘掉）
//
// 无法识别 → nil（不写 tool_choice，由上游按默认行为处理）。
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return "auto"
		case "any", "required":
			return "required"
		case "none":
			return "none"
		}
		return nil
	}
	var m struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(m.Type)) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool", "function":
		if n := strings.TrimSpace(m.Name); n != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": n}}
		}
	}
	return nil
}
