// Package server —— 协议兼容层（/v1/responses、/v1/messages）。
//
// 设计取舍：**不复制调度逻辑**。网关的选号轮转、软冷却、熔断、模型编排、
// 分发密钥、用量记账全部长在 chatCompletions 那条链上；本层只做两件事——
// 入站把外部协议请求体翻译成 OpenAI chat 请求体，出站把 chat 的响应/流
// 翻译成目标协议形态。中间整段复用内部 /v1/chat/completions（mux 内调），
// 因此新增协议对既有 chat 路径零改动、对编排与密钥分发零回归。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// compatProto 目标协议标识。
type compatProto string

const (
	protoResponses compatProto = "responses"
	protoMessages  compatProto = "messages"
)

// newCompatID 生成协议风格的随机 ID（resp_xxx / msg_xxx）。
// crypto/rand 不可用时回落时间戳（不阻塞请求）。
func newCompatID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + hex.EncodeToString(b)
}

// compatRun 把已翻译好的 chat 请求体交给内部 chat 链路，再把结果按目标协议写回。
// stream 由入站请求决定（决定出站是 SSE 还是单 JSON）。
func (h *Handler) compatRun(w http.ResponseWriter, r *http.Request, p compatProto, chatBody []byte, stream bool, model string) {
	inner := r.Clone(r.Context())
	inner.URL.Path = "/v1/chat/completions"
	inner.URL.RawPath = ""
	inner.Body = io.NopCloser(bytes.NewReader(chatBody))
	inner.ContentLength = int64(len(chatBody))
	// 翻译后的 body 恒为 JSON：入站客户端漏带 Content-Type（curl -d 直发）时
	// 内部链路读不到，这里补齐，避免协议层差异变成 400。
	inner.Header.Set("Content-Type", "application/json")

	rec := newCompatRecorder(w, p, stream, model)
	h.mux.ServeHTTP(rec, inner)
	rec.finish()
}

// compatRecorder 拦截内部 chat 响应并转写为目标协议：
//   - 非流式：缓冲整个 JSON，finish 时整体翻译；
//   - 流式：逐帧解析 chat chunk，实时翻译成目标协议的 SSE 事件；
//   - 错误：缓冲 OpenAI 错误体，finish 时翻译成目标协议错误形态（状态码保留）。
type compatRecorder struct {
	w      http.ResponseWriter
	proto  compatProto
	stream bool
	model  string

	id        string // responses: resp_xxx
	msgID     string // 消息级 id（responses 内部 item / anthropic message）
	created   int64
	hdr       http.Header
	status    int
	body      []byte // 非流式响应体 / 错误体
	buf       []byte // 流式残留半帧
	text      strings.Builder
	finishRsn string
	usage     map[string]any
	started   bool
	doneSent  bool
	errored   bool
	closed    bool

	// 工具调用转写状态（两种协议共用）：
	//   - 非流式：finish 时从 chat 响应整体解析进 calls；
	//   - 流式：按 OpenAI delta.tool_calls 的 index 建 Anthropic content_block
	//     （tool_use），index 由 nextIdx 递增分配，保证与 content 数组顺序一致。
	nextIdx   int
	textIdx   int
	textOpen  bool
	toolOrder []int
	tools     map[int]*compatToolBlock
	calls     []compatToolCall
}

// compatToolBlock 一个流式 tool_use 内容块的累积状态。
type compatToolBlock struct {
	idx     int // Anthropic content_block index
	id      string
	name    string
	args    strings.Builder
	open    bool
	stopped bool
}

// compatToolCall 一个完整的工具调用（两种协议出站渲染的公共形态）。
type compatToolCall struct {
	id    string
	name  string
	args  string // 字符串化 JSON（arguments / input_json_delta 拼接结果）
	input any    // args 解析后的对象；解析失败 → 空对象
}

// toolCalls 输出累积完成的工具调用列表（按出现顺序）。
func (cr *compatRecorder) toolCalls() []compatToolCall {
	if len(cr.calls) > 0 {
		return cr.calls
	}
	if len(cr.toolOrder) == 0 {
		return nil
	}
	out := make([]compatToolCall, 0, len(cr.toolOrder))
	for _, k := range cr.toolOrder {
		tb, ok := cr.tools[k]
		if !ok || tb == nil {
			continue
		}
		raw := tb.args.String()
		var in any
		if json.Unmarshal([]byte(raw), &in) != nil {
			in = map[string]any{}
		}
		out = append(out, compatToolCall{id: tb.id, name: tb.name, args: raw, input: in})
	}
	return out
}

func newCompatRecorder(w http.ResponseWriter, p compatProto, stream bool, model string) *compatRecorder {
	id := newCompatID(p.idPrefix())
	return &compatRecorder{
		w:       w,
		proto:   p,
		stream:  stream,
		model:   model,
		id:      id,
		msgID:   newCompatID("msg_"),
		created: time.Now().Unix(),
		hdr:     make(http.Header),
		tools:   make(map[int]*compatToolBlock),
	}
}

func (p compatProto) idPrefix() string {
	if p == protoMessages {
		return "msg_"
	}
	return "resp_"
}

func (cr *compatRecorder) Header() http.Header { return cr.hdr }

func (cr *compatRecorder) WriteHeader(code int) {
	cr.status = code
	if code != http.StatusOK {
		return // 错误响应：缓冲 body，finish 时按协议翻译
	}
	if cr.stream {
		cr.startStream()
	}
}

func (cr *compatRecorder) Write(p []byte) (int, error) {
	if cr.status == 0 {
		// chat 流式路径不显式 WriteHeader（StreamHint 直接写帧，靠 net/http 隐式
		// 200）：这里补上协议起始帧，保证 SSE headers 先于数据帧落地。
		cr.status = http.StatusOK
		if cr.stream {
			cr.startStream()
		}
	}
	if cr.status != http.StatusOK || !cr.stream {
		cr.body = append(cr.body, p...)
		return len(p), nil
	}
	cr.buf = append(cr.buf, p...)
	for {
		idx := bytes.Index(cr.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := string(cr.buf[:idx])
		cr.buf = append([]byte(nil), cr.buf[idx+2:]...)
		cr.handleFrame(frame)
	}
	return len(p), nil
}

func (cr *compatRecorder) Flush() {
	if f, ok := cr.w.(http.Flusher); ok {
		f.Flush()
	}
}

// handleFrame 处理一帧 chat chunk（data: <json>）
func (cr *compatRecorder) handleFrame(frame string) {
	line := strings.TrimRight(frame, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if strings.TrimSpace(payload) == "[DONE]" {
		cr.emitDone()
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	if e, hasErr := obj["error"]; hasErr {
		cr.emitStreamError(e)
		return
	}
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if d, ok := c["delta"].(map[string]any); ok {
				if t, ok := d["content"].(string); ok && t != "" {
					cr.text.WriteString(t)
					cr.emitDelta(t)
				}
				// 工具调用增量：首片带 id/name，后续片只带 arguments 片段。
				if tcs, ok := d["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok2 := tcv.(map[string]any)
						if tc == nil || !ok2 {
							continue
						}
						idx := 0
						if v, ok3 := tc["index"].(float64); ok3 {
							idx = int(v)
						}
						id, _ := tc["id"].(string)
						name, args := "", ""
						if fn, ok3 := tc["function"].(map[string]any); ok3 {
							name, _ = fn["name"].(string)
							args, _ = fn["arguments"].(string)
						}
						if id == "" && name == "" && args == "" {
							continue
						}
						cr.emitToolCall(idx, id, name, args)
					}
				}
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				cr.finishRsn = fr
			}
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		cr.usage = u
	}
	// 流式下 model 取上游实际模型（编排可能换了号/换了模型）：首帧已发出改不回，
	// 但末帧（response.completed / 后续事件）要用真值，否则客户端记录的是入站名。
	if m, ok := obj["model"].(string); ok && m != "" {
		cr.model = m
	}
}

// finish 收尾：非流式翻译整体响应，错误走协议错误形态，流式补终止事件。
func (cr *compatRecorder) finish() {
	if cr.closed {
		return
	}
	cr.closed = true

	// 网关侧响应头透出（编排实际模型等）：错误/成功都带。
	for k, vs := range cr.hdr {
		if strings.HasPrefix(strings.ToLower(k), "x-wb2a-") {
			for _, v := range vs {
				cr.w.Header().Add(k, v)
			}
		}
	}

	if cr.stream && cr.status == http.StatusOK {
		// 流中已发过 error 事件（上游中途报错）：不再补 completed，避免把失败流
		// 收尾成「成功完成」——客户端据此会认为请求成功。
		if !cr.errored {
			cr.emitDone() // 幂等：未收到 [DONE]（异常断流）也补齐收尾
		}
		return
	}
	if cr.status != http.StatusOK || cr.errored {
		cr.writeError()
		return
	}
	var chat map[string]any
	if json.Unmarshal(cr.body, &chat) != nil {
		cr.writeCompatError(http.StatusBadGateway, "api_error", "upstream_parse", "invalid upstream response")
		return
	}
	if e, hasErr := chat["error"]; hasErr {
		cr.writeCompatError(cr.statusOr500(), "api_error", "upstream_error", errorTextOf(e))
		return
	}
	cr.writeFinal(chat)
}

func (cr *compatRecorder) statusOr500() int {
	if cr.status >= 400 {
		return cr.status
	}
	return http.StatusBadGateway
}

// errorTextOf 从错误对象取 message（缺失回落空串）。
func errorTextOf(e any) string {
	if m, ok := e.(map[string]any); ok {
		if s, _ := m["message"].(string); s != "" {
			return s
		}
	}
	return "upstream error"
}

// writeAuthError 鉴权失败的统一出口：按入口协议选择错误形态——
// /v1/messages 走 Anthropic（{"type":"error","error":{...}}），其余走 OpenAI。
// 鉴权发生在协议层之前（withAuth），故形态选择只能落在这一层。
func writeAuthError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if r != nil && r.URL != nil && r.URL.Path == "/v1/messages" {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrType(status), "message": msg},
		})
		return
	}
	writeOpenAIError(w, status, code, msg)
}

// ── 出站：SSE 事件 ────────────────────────────────────────────────

func (cr *compatRecorder) writeSSE(event string, payload any) {
	raw, _ := json.Marshal(payload)
	_, _ = io.WriteString(cr.w, "event: "+event+"\n")
	_, _ = io.WriteString(cr.w, "data: "+string(raw)+"\n\n")
	cr.Flush()
}

func (cr *compatRecorder) startStream() {
	if cr.started {
		return
	}
	cr.started = true
	h := cr.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	cr.w.WriteHeader(http.StatusOK)
	cr.Flush()
	if cr.proto == protoResponses {
		// message item 固定占 output_index 0；工具块从 1 起（否则首个工具与
		// 消息块同号，客户端按 output_index 归并时会把两者混成一个 item）。
		cr.nextIdx = 1
		cr.writeSSE("response.created", map[string]any{
			"type": "response.created", "response": cr.responseObject("in_progress", ""),
		})
		// in_progress 是官方事件序列的固定第二帧（SDK 据此把 status 从 queued
		// 推进到 in_progress）；缺帧不影响解析，但严格按顺序消费的客户端会卡在
		// 「等下一帧」——成本为零，补齐。
		cr.writeSSE("response.in_progress", map[string]any{
			"type": "response.in_progress", "response": cr.responseObject("in_progress", ""),
		})
		cr.writeSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": cr.outputItem("in_progress", ""),
		})
		cr.writeSSE("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": cr.msgID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		return
	}
	cr.writeSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": cr.msgID, "type": "message", "role": "assistant", "model": cr.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	// content_block_start 改为懒发：首个 text / tool_use 分片到达时才开块，
	// 保证块下标按真实出现顺序分配（工具先于文本时也自然对齐）。
	cr.writeSSE("ping", map[string]any{"type": "ping"})
}

func (cr *compatRecorder) emitDelta(t string) {
	if !cr.started {
		cr.startStream()
	}
	if cr.proto == protoResponses {
		cr.writeSSE("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": cr.msgID,
			"output_index": 0, "content_index": 0, "delta": t,
		})
		return
	}
	cr.ensureTextBlock()
	cr.writeSSE("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": cr.textIdx,
		"delta": map[string]any{"type": "text_delta", "text": t},
	})
}

// ensureTextBlock 懒开 text 内容块（幂等）：首个文本分片到达时才发
// content_block_start，块下标从 nextIdx 取。
func (cr *compatRecorder) ensureTextBlock() {
	if cr.textOpen {
		return
	}
	cr.textOpen = true
	cr.textIdx = cr.nextIdx
	cr.nextIdx++
	cr.writeSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": cr.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// emitToolCall 处理一帧 OpenAI delta.tool_calls 分片，按协议转写：
//   - anthropic：content_block_start(tool_use) → input_json_delta → （收尾时）stop
//   - responses：output_item.added(function_call) → function_call_arguments.delta
//
// 分片形态（链路已按 OpenAI 规范收敛）：首片带 index/id/function.name，后续片只带
// function.arguments 片段且 name 键已删除。
func (cr *compatRecorder) emitToolCall(idx int, id, name, args string) {
	if !cr.started {
		cr.startStream()
	}
	tb, ok := cr.tools[idx]
	if !ok {
		if id == "" {
			id = newCompatID("toolu_")
		}
		tb = &compatToolBlock{id: id}
		cr.tools[idx] = tb
		cr.toolOrder = append(cr.toolOrder, idx)
	}
	if name != "" && tb.name == "" {
		tb.name = name
	}
	if args != "" {
		tb.args.WriteString(args)
	}
	if tb.open {
		// 已开块：只推参数增量（name 可能晚到，但块已发出，不再补发 start）
		cr.writeToolArgs(tb, args)
		return
	}
	tb.open = true
	tb.idx = cr.nextIdx
	cr.nextIdx++
	switch cr.proto {
	case protoResponses:
		cr.writeSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": tb.idx,
			"item": cr.functionCallItem(tb, "in_progress"),
		})
	default:
		cr.writeSSE("content_block_start", map[string]any{
			"type": "content_block_start", "index": tb.idx,
			"content_block": map[string]any{
				"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]any{},
			},
		})
	}
	if args != "" {
		cr.writeToolArgs(tb, args)
	}
}

// writeToolArgs 推一帧工具参数增量。
func (cr *compatRecorder) writeToolArgs(tb *compatToolBlock, args string) {
	if args == "" {
		return
	}
	if cr.proto == protoResponses {
		cr.writeSSE("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": tb.id,
			"output_index": tb.idx, "delta": args,
		})
		return
	}
	cr.writeSSE("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tb.idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

// functionCallItem Responses API 的 function_call output item。
func (cr *compatRecorder) functionCallItem(tb *compatToolBlock, status string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": tb.id, "call_id": tb.id,
		"name": tb.name, "arguments": tb.args.String(), "status": status,
	}
}

// closeBlocks 收尾所有已开内容块（幂等）：text 块 stop → 各 tool 块按序 stop。
func (cr *compatRecorder) closeBlocks() {
	if cr.proto == protoResponses {
		for _, k := range cr.toolOrder {
			tb := cr.tools[k]
			if tb == nil || !tb.open || tb.stopped {
				continue
			}
			tb.stopped = true
			cr.writeSSE("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": tb.id,
				"output_index": tb.idx, "arguments": tb.args.String(),
			})
			cr.writeSSE("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": tb.idx,
				"item": cr.functionCallItem(tb, "completed"),
			})
		}
		return
	}
	if cr.textOpen {
		cr.textOpen = false
		cr.writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": cr.textIdx})
	}
	for _, k := range cr.toolOrder {
		tb := cr.tools[k]
		if tb == nil || !tb.open || tb.stopped {
			continue
		}
		tb.stopped = true
		cr.writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": tb.idx})
	}
}

// emitDone 流式收尾（幂等）：responses 发 output_text.done / output_item.done /
// response.completed；anthropic 发 content_block_stop / message_delta / message_stop。
func (cr *compatRecorder) emitDone() {
	if cr.doneSent {
		return
	}
	cr.doneSent = true
	if !cr.started {
		cr.startStream()
	}
	txt := cr.text.String()
	if cr.proto == protoResponses {
		cr.writeSSE("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": cr.msgID,
			"output_index": 0, "content_index": 0, "text": txt,
		})
		cr.writeSSE("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": cr.outputItem("completed", txt),
		})
		// 工具调用块收尾（arguments.done + output_item.done）
		cr.closeBlocks()
		cr.writeSSE("response.completed", map[string]any{
			"type": "response.completed", "response": cr.responseObject("completed", txt),
		})
		return
	}
	// 空响应（无文本无工具）补一个空 text 块：Anthropic 客户端期望 content 至少一块。
	if !cr.textOpen && len(cr.toolOrder) == 0 {
		cr.ensureTextBlock()
	}
	cr.closeBlocks()
	cr.writeSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   cr.stopReason(cr.finishRsn),
			"stop_sequence": nil,
		},
		// usage 带 input_tokens：Anthropic 规范里 message_delta 只定义 output_tokens，
		// 但本网关流式的 input_tokens 只有流末才拿得到（message_start 已发出，补不回），
		// 这里并列给出，让按 message_delta 汇总用量的客户端不至于漏掉输入侧。
		"usage": cr.anthropicUsage(),
	})
	cr.writeSSE("message_stop", map[string]any{"type": "message_stop"})
}

func (cr *compatRecorder) emitStreamError(e any) {
	cr.errored = true
	msg := errorTextOf(e)
	code := ""
	if m, ok := e.(map[string]any); ok {
		if s, _ := m["code"].(string); s != "" {
			code = s
		}
	}
	if cr.proto == protoResponses {
		errObj := map[string]any{"message": msg, "type": "server_error"}
		if code != "" {
			errObj["code"] = code
		}
		cr.writeSSE("error", map[string]any{"type": "error", "error": errObj})
		return
	}
	cr.writeSSE("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
}

// ── 出站：非流式 ──────────────────────────────────────────────────

func (cr *compatRecorder) writeFinal(chat map[string]any) {
	txt := chatContent(chat)
	finish := ""
	if chs, ok := chat["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if fr, _ := c["finish_reason"].(string); fr != "" {
				finish = fr
			}
			if msg, ok := c["message"].(map[string]any); ok {
				if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
					cr.calls = parseChatToolCalls(tcs)
				}
			}
		}
	}
	if u, ok := chat["usage"].(map[string]any); ok && len(u) > 0 {
		cr.usage = u
	}
	if m, _ := chat["model"].(string); m != "" {
		cr.model = m
	}
	if cr.proto == protoResponses {
		writeJSON(cr.w, http.StatusOK, cr.responseObject(responsesStatus(finish), txt))
		return
	}
	// Anthropic content：文本块在前，其后每个工具调用一个 tool_use 块。
	// content 不允许为空数组 → 兜底一个空文本块。
	content := make([]any, 0, len(cr.calls)+1)
	if txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	for _, tc := range cr.toolCalls() {
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.id, "name": tc.name, "input": tc.input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	writeJSON(cr.w, http.StatusOK, map[string]any{
		"id":            cr.msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         cr.model,
		"content":       content,
		"stop_reason":   cr.stopReason(finish),
		"stop_sequence": nil,
		"usage":         cr.anthropicUsage(),
	})
}

// anthropicUsage Anthropic 形态用量对象。除 input/output 外并列给出缓存用量：
// 上游 chat usage 带 cache_creation_input_tokens / cache_read_input_tokens，
// Anthropic 规范里这两个字段恒存在（缺字段会让按 usage 做成本统计的客户端拿到
// undefined，而不是 0）。无源数据时 tokens() 回落 0，不编造。
func (cr *compatRecorder) anthropicUsage() map[string]any {
	return map[string]any{
		"input_tokens":                cr.tokens("prompt_tokens"),
		"output_tokens":               cr.tokens("completion_tokens"),
		"cache_creation_input_tokens": cr.tokens("cache_creation_input_tokens"),
		"cache_read_input_tokens":     cr.tokens("cache_read_input_tokens"),
	}
}

// responsesUsage Responses 形态用量对象：input/output/total 之上补 OpenAI 规范的
// tokens_details（缓存命中 token 与推理 token）。源数据取上游 details 子树，
// 缺失时回落平铺字段（cached_tokens / completion_thinking_tokens）。
func (cr *compatRecorder) responsesUsage() map[string]any {
	cached, reasoning := 0, 0
	if d, ok := cr.usage["prompt_tokens_details"].(map[string]any); ok {
		cached = numOf(d["cached_tokens"])
	} else {
		cached = cr.tokens("cached_tokens")
	}
	if d, ok := cr.usage["completion_tokens_details"].(map[string]any); ok {
		reasoning = numOf(d["reasoning_tokens"])
	} else {
		reasoning = cr.tokens("completion_thinking_tokens")
	}
	return map[string]any{
		"input_tokens":          cr.tokens("prompt_tokens"),
		"output_tokens":         cr.tokens("completion_tokens"),
		"total_tokens":          cr.tokens("total_tokens"),
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
}

// parseChatToolCalls 从 chat 响应的 message.tool_calls 解析工具调用列表。
func parseChatToolCalls(raws []any) []compatToolCall {
	out := make([]compatToolCall, 0, len(raws))
	for _, r := range raws {
		tc, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		args := ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		if name == "" {
			continue
		}
		id, _ := tc["id"].(string)
		if id == "" {
			id = newCompatID("toolu_")
		}
		var in any
		if json.Unmarshal([]byte(args), &in) != nil {
			in = map[string]any{}
		}
		out = append(out, compatToolCall{id: id, name: name, args: args, input: in})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responseObject Responses API 响应对象（status 决定进行中/完成）。
func (cr *compatRecorder) responseObject(status, txt string) map[string]any {
	item := cr.outputItem(status, txt)
	// 工具调用作为 function_call item 挂在 output 尾部（Responses 规范形态）。
	output := []any{item}
	for _, tc := range cr.toolCalls() {
		output = append(output, map[string]any{
			"type": "function_call", "id": tc.id, "call_id": tc.id,
			"name": tc.name, "arguments": tc.args, "status": status,
		})
	}
	out := map[string]any{
		"id":         cr.id,
		"object":     "response",
		"created_at": cr.created,
		"status":     status,
		"model":      cr.model,
		"output":     output,
		"usage":      cr.responsesUsage(),
	}
	// output_text 始终给（官方在非 completed 也带该字段，只是为空）：只按 completed
	// 给会让读 output_text 的客户端在 incomplete 时拿到 undefined 而非空串。
	// status=incomplete（对应 finish_reason=length）补 incomplete_details：
	// 客户端据此区分「被截断」与「正常结束」，缺字段会被当成成功完成。
	out["output_text"] = txt
	if status == "incomplete" {
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return out
}

// outputItem Responses API 的 output message item。
func (cr *compatRecorder) outputItem(status, txt string) map[string]any {
	return map[string]any{
		"type": "message", "id": cr.msgID, "status": status, "role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": txt, "annotations": []any{},
		}},
	}
}

// ── 出站：错误 ────────────────────────────────────────────────────

func (cr *compatRecorder) writeError() {
	status := cr.status
	if status < 400 {
		status = http.StatusBadGateway
	}
	msg := "upstream error"
	code := "upstream_error"
	var obj map[string]any
	if json.Unmarshal(cr.body, &obj) == nil {
		if e, ok := obj["error"]; ok {
			msg = errorTextOf(e)
			if m, ok2 := e.(map[string]any); ok2 {
				if s, _ := m["code"].(string); s != "" {
					code = s
				}
			}
		}
	}
	cr.writeCompatError(status, anthropicErrType(status), code, msg)
}

// writeCompatError 按目标协议写出错误（状态码沿用内部链路的判定）。
func (cr *compatRecorder) writeCompatError(status int, anthType, code, msg string) {
	if cr.proto == protoResponses {
		writeJSON(cr.w, status, map[string]any{
			"error": map[string]any{"message": msg, "type": anthType, "code": code},
		})
		return
	}
	writeJSON(cr.w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthType, "message": msg},
	})
}

// anthropicErrType HTTP 状态码 → Anthropic 错误 type。
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// cr.stopReason 计算本轮的 Anthropic stop_reason，并在「有工具调用」时兜底成
// tool_use：部分上游在返回 tool_calls 时仍给 finish_reason=stop，照搬会让
// stop_reason=end_turn —— Claude Code 等客户端据此判定「这轮已结束」直接收工
// （issue #2 的同类病：工具调用被当成对话终止）。有工具即 tool_use，无工具按映射。
func (cr *compatRecorder) stopReason(finish string) string {
	if len(cr.toolCalls()) > 0 {
		return "tool_use"
	}
	return anthropicStopReason(finish)
}

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// responsesStatus finish_reason → Responses API status。
func responsesStatus(finish string) string {
	if finish == "length" {
		return "incomplete"
	}
	return "completed"
}

// tokens 从 usage 取整数 token 数（缺失/非法 → 0）。
func (cr *compatRecorder) tokens(key string) int {
	if cr.usage == nil {
		return 0
	}
	return numOf(cr.usage[key])
}

// numOf 把 JSON 数字（float64 / int / json.Number）取成 int，其它类型一律 0。
func numOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// chatContent 从 chat 响应取助手正文（首个 choice）。
func chatContent(chat map[string]any) string {
	chs, ok := chat["choices"].([]any)
	if !ok || len(chs) == 0 {
		return ""
	}
	c, ok := chs[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		if s, _ := c["text"].(string); s != "" {
			return s
		}
		return ""
	}
	if s, ok := msg["content"].(string); ok {
		return s
	}
	// 多段 content（部分上游下数组形态）：拼接 text 段。
	if parts, ok := msg["content"].([]any); ok {
		var sb strings.Builder
		for _, p := range parts {
			if m, ok := p.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}
