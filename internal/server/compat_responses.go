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
//   - tools：可选，[{type:"function", name, description, parameters}] → OpenAI tools
//   - tool_choice：可选，"auto" / "required" / "none" / {type:"function", name}
//   - stream / max_output_tokens / temperature / top_p：可选，映射为 chat 同义字段
//
// 不支持的字段（previous_response_id / reasoning 等）静默忽略：网关是转发层，
// 编造这些语义只会给出假成功。input 里的 function_call / function_call_output
// 项已支持（多轮工具调用的会话回放，缺失会让 Codex 之类客户端的后续轮次 400）。
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
		Model           string            `json:"model"`
		Input           json.RawMessage   `json:"input"`
		Instructions    json.RawMessage   `json:"instructions"`
		Stream          bool              `json:"stream"`
		MaxOutputTokens *int              `json:"max_output_tokens"`
		Temperature     *float64          `json:"temperature"`
		TopP            *float64          `json:"top_p"`
		Tools           []json.RawMessage `json:"tools"`
		ToolChoice      json.RawMessage   `json:"tool_choice"`
		// 以下三项是对齐 chat 语义的透传/翻译项：官方 Responses 客户端（Codex 等）
		// 常发，此前一律静默丢弃。字段改写规则见 responsesExtrasToChat。
		ParallelToolCalls *bool           `json:"parallel_tool_calls"`
		Reasoning         json.RawMessage `json:"reasoning"`
		Text              json.RawMessage `json:"text"`
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
		// 连续的 function_call 项合并成一条 assistant.tool_calls 消息（OpenAI 要求
		// 一次 assistant 消息带齐本轮全部调用，其 tool 结果紧随其后）。
		var pending []any
		flush := func() {
			if len(pending) == 0 {
				return
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": pending})
			pending = nil
		}
		for _, it := range items {
			switch typ, _ := it["type"].(string); typ {
			case "function_call":
				name, _ := it["name"].(string)
				if name == "" {
					continue
				}
				id, _ := it["call_id"].(string)
				if id == "" {
					id, _ = it["id"].(string)
				}
				if id == "" {
					id = newCompatID("call_")
				}
				pending = append(pending, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": toolArgsJSON(it["arguments"])},
				})
				continue
			case "function_call_output":
				flush()
				id, _ := it["call_id"].(string)
				if id == "" {
					id, _ = it["id"].(string)
				}
				if id == "" {
					continue
				}
				msgs = append(msgs, map[string]any{
					"role": "tool", "tool_call_id": id, "content": toolResultText(it["output"]),
				})
				continue
			}
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
		flush()
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
	// tools / tool_choice：无 tools 时 tool_choice 无意义（上游会 400），整体不下发。
	if tools := responsesToolsToChat(req.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	responsesExtrasToChat(out, req.ParallelToolCalls, req.Reasoning, req.Text)
	b, _ := json.Marshal(out)
	return b, req.Stream, model, ""
}

// responsesExtrasToChat 把 Responses 的三个常用控制字段落到 chat 语义上。
//   - parallel_tool_calls → 同名透传（OpenAI 规范字段，上游忽略未知字段，实测 200）
//   - reasoning.effort    → reasoning_effort（走上游既有的按模型降级管线）
//   - text.format         → response_format（json_object / json_schema 两种）
//
// 三者都是「客户端明确表达的模型行为约束」，丢弃等于让模型按默认跑；翻译不到位的
// 形态（如 reasoning 非对象、format 缺 type）一律不下发，不编造。
func responsesExtrasToChat(out map[string]any, parallel *bool, reasoning, text json.RawMessage) {
	if parallel != nil {
		out["parallel_tool_calls"] = *parallel
	}
	if len(reasoning) > 0 {
		var rs struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(reasoning, &rs) == nil {
			if e := strings.TrimSpace(rs.Effort); e != "" {
				out["reasoning_effort"] = strings.ToLower(e)
			}
		}
	}
	if len(text) > 0 {
		var ts struct {
			Format map[string]any `json:"format"`
		}
		if json.Unmarshal(text, &ts) == nil && len(ts.Format) > 0 {
			if ft, _ := ts.Format["type"].(string); strings.TrimSpace(ft) != "" {
				out["response_format"] = ts.Format
			}
		}
	}
}

// responsesToolsToChat 把 Responses 的 tools（{type:"function", name, description,
// parameters}）翻译成 OpenAI tools 形态；形态不对 / 缺 name 的项跳过。
func responsesToolsToChat(raws []json.RawMessage) []any {
	if len(raws) == 0 {
		return nil
	}
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		var t struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
			// 少数客户端直接发 OpenAI 形态（带 function 子树），一并兼容。
			Function map[string]any `json:"function"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		name := strings.TrimSpace(t.Name)
		desc := t.Description
		schema := t.Parameters
		if fn := t.Function; fn != nil {
			if name == "" {
				if n, _ := fn["name"].(string); n != "" {
					name = strings.TrimSpace(n)
				}
			}
			if desc == "" {
				desc, _ = fn["description"].(string)
			}
			if schema == nil {
				schema, _ = fn["parameters"].(map[string]any)
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
