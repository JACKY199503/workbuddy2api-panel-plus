package server

import (
	"strings"
	"time"
)

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// cstZone Asia/Shanghai 固定偏移：容器/宿主机时区常为 UTC，模型编排的昼夜判定
// 必须按北京时间算（不依赖 TZ 环境变量，避免部署差异导致窗口错位）。
var cstZone = time.FixedZone("CST", 8*60*60)

// hourCST 当前 Asia/Shanghai 的小时（0-23），供模型编排的昼夜主模型判定。
func hourCST() int { return time.Now().In(cstZone).Hour() }

// realModelExists 该模型名是否在上游真实模型表里（读本地缓存，不触发探测）。
//
// 用途：虚拟模型与上游同名时的让位判定——上游自带 auto 时不劫持它的语义
// （override=true 才是明确要求接管）。global 分支走 upstream 的 1h 模型缓存，
// 无 global 账号时直接返回空（零上游请求）。
func (h *Handler) realModelExists(name string) bool {
	realm, bare := resolveModel(name)
	if bare == "" {
		return false
	}
	if realm == "global" {
		ids, _ := h.fetchGlobalModels()
		for _, id := range ids {
			if id == bare {
				return true
			}
		}
		return false
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bare {
			return true
		}
	}
	return false
}

// isEmptyCompletion 聚合响应是否「200 但正文为空」：无 choices、或首条 message
// 的 content 空白且无 tool_calls。
//
// 判定从严：客户端真正拿不到正文才算空。带 tool_calls 的响应即便无正文也是有效
// 结果（工具调用场景），不触发降级。
func isEmptyCompletion(resp map[string]any) bool {
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		return true
	}
	c0, ok := choices[0].(map[string]any)
	if !ok {
		return false // 形态未知 → 不判空（宁可不降级，也不误吞正常响应）
	}
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return false
	}
	if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
		return false
	}
	content, _ := msg["content"].(string)
	return strings.TrimSpace(content) == ""
}
