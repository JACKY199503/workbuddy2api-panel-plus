// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（Claude Code/Codex 等 CLI）在 system prompt 注入固定模板句，
// 上游内容审核按逐字精确匹配误杀合法流量（issue #36/PR39 的 11128）。
// 方案：网关在出站前用自有系统提示词替换客户端 system/developer 消息，
// 从源头消灭 system 来源的指纹误报（用户/assistant 消息里的指纹串仍由
// internal/upstream/sanitize.go 清洗，两层叠加、互不替代）。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息逐字不动。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system/developer 消息，保留 user/assistant/tool 及其他角色。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// Append 解析 OpenAI 请求体并在"开头连续 system/developer 块"之后插入一条
// 网关自有 system 提示词（issue #129 append 模式）：
//   - 开头连续块 = 从 messages[0] 起向后 role 为 system/developer（精确字符串
//     匹配，与 Rewrite 删除口径一致）的消息；遇第一条非 system/developer
//     消息（含非 map 消息、无 role 消息）即停；
//   - 插入点 = 连续块末尾之后（块长 0 时即 messages 最前）；
//   - 所有既有消息（含开头块、中途 system、user/assistant/tool）逐字不动
//     ——客户端项目规范/工具约定与网关提示词并用。
//
// 守卫与 Rewrite 逐条一致：空 body / 空 systemPrompt / 坏 JSON → 原样返回
// （绝不失败）；无 messages 字段 → messages=[网关 system]，其余字段原样。
//
// 边界判定须显式同时匹配 system 与 developer：归一（developer→system）在
// 下游 prepareBody 的 normalizeRoles，Append 执行时开头块里的 developer
// 还是 developer。网关消息角色用 system 而非 developer——上游 role 白名单
// 不含 developer，插 developer 等于制造一次必然归一与多余的 11128 风险窗口。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 扫描开头连续 system/developer 块，遇第一条非 system/developer 即停。
	insertAt := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	gw := map[string]any{"role": "system", "content": systemPrompt}
	// 已有消息逐字不动：只在插入点拼接，不重排、不改写任何元素。
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, gw)
	rewritten = append(rewritten, msgs[insertAt:]...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
