// Package autoroute 模型编排：auto 虚拟模型（昼夜主模型轮换）与降级链。
//
// 背景（上游 issue #31）：网关只负责出号，不内置模型轮换；但上游账号的免费额度/
// 夜间窗口/限流状态会随时间与套餐波动，客户端写死单一模型容易撞墙。本包把
// 「虚拟模型名 → 实际模型候选链」的编排收在网关侧：客户端只写 auto，网关按当前
// 时段挑主模型，失败时按链降级。
//
// 设计边界：
//   - 只做「选哪个模型」，不做「选哪个账号」（仍由 pool 负责），两者正交；
//   - 编排结果是一条**候选链**，不是覆盖式改写：链首即首选，后续仅在可降级错误
//     或无可用账号时才尝试；
//   - 纯函数（Config 值语义 + 显式 now），可单测、无全局状态；配置热改通过
//     livecfg.Snapshot 整体替换。
package autoroute

import "strings"

// maxChain 候选链长度上限（防配置成环/过长导致请求放大）。
const maxChain = 8

// maxExpandDepth model_fallback 递归展开深度上限。
const maxExpandDepth = 3

// NoHealthyAccount 本地「无可用账号」的降级类别名（与 upstream.ErrKind.String()
// 同命名空间，便于统一配置）。
const NoHealthyAccount = "no_healthy_account"

// defaultKinds 默认可触发降级的错误类别。
//
// 判据是「换号解决不了、换模型可能解决」：限流/额度/该后端无此模型/上游故障/
// 账号授权故障/404/无可用账号。反之，内容拦截、参数错误、上下文超长、请求体
// 解析失败是**请求本身**的问题，换任何模型都一样撞墙，不降级（与网关既有的
// fail-fast 哲学一致）。
var defaultKinds = []string{
	"soft_rate", "hard_credit", "model_blocked", "server",
	"account_fault", "not_found", NoHealthyAccount,
}

// Config 模型编排配置（config.json 的 auto_model + model_fallback 两段）。
type Config struct {
	// Enabled 总开关。false 时 Chain 原样返回单元素链（零回归）。
	Enabled bool
	// DayPrimary / NightPrimary 白天/夜间主模型（带 realm 前缀，如 cn:hy3）。
	DayPrimary   string
	NightPrimary string
	// DayStart / DayEnd 白天窗口 [DayStart, DayEnd)，按 Asia/Shanghai 小时判定。
	// 例如 8/23 → 08:00–23:00 用 DayPrimary，23:00–08:00 用 NightPrimary。
	DayStart int
	DayEnd   int
	// Fallback auto 的降级链（有序）。
	Fallback []string
	// ModelFallback 任意模型 → 降级链（有序）。键为客户端写的模型名（含前缀）。
	ModelFallback map[string][]string
	// VirtualID 虚拟模型名（默认 "auto"）。客户端写这个名字（或 "<realm>:<名字>"
	// 形式）时由编排接管。
	//
	// 上游本身可能已提供同名模型（上游 cn 侧确实下发过 "auto"）：此时默认**让位**
	// （不劫持上游既有能力），只有 Override=true 才接管。想彻底避开同名冲突可把
	// 名字改成别的（如 "auto-router"）。
	VirtualID string
	// Override 虚拟模型名与上游真实模型同名时仍然接管（覆盖上游同名模型语义）。
	Override bool
	// OnEmpty 上游 200 但正文为空时视为失败并降级。
	//
	// 由来（issue #31 点名的坑）：部分模型默认 reasoning_effort=max，思考 token
	// 吃满 max_tokens 预算 → 返回 200 但 content 为空，且不报错 → 客户端拿到空
	// 回复。开启后这类空回复走降级链（而不是把空结果当成功返回）。
	OnEmpty bool
	// FallbackOn 可触发降级的错误类别名集合；空 = 用 defaultKinds。
	FallbackOn []string
}

// RealmOf 取模型名的 realm 前缀（"cn:hy3" → "cn"；无前缀 → "cn"）。
func RealmOf(model string) string {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn"
	}
	p := model[:idx]
	if p != "cn" && p != "global" {
		return "cn"
	}
	return p
}

// BareOf 取去前缀的裸模型名。
func BareOf(model string) string {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return model
	}
	if p := model[:idx]; p == "cn" || p == "global" {
		return model[idx+1:]
	}
	return model
}

// IsAuto 判断模型名是否命中虚拟模型名（"auto" / "cn:auto" / "global:auto"）。
// 已废弃语义保留：请用 IsVirtual（含同名让位判定）。
func IsAuto(model string) bool { return BareOf(model) == "auto" }

// virtualID 生效的虚拟模型名（空值回落 "auto"）。
func (c Config) virtualID() string {
	if s := strings.TrimSpace(c.VirtualID); s != "" {
		return s
	}
	return "auto"
}

// IsVirtual 该模型名是否应由编排接管。
//
// realExists = 该名字在上游真实模型表里存在。存在且未开 Override → 不接管
// （让位给上游同名模型，避免静默替换上游既有能力）。
func (c Config) IsVirtual(model string, realExists bool) bool {
	if !c.Enabled {
		return false
	}
	if BareOf(model) != c.virtualID() {
		return false
	}
	if realExists && !c.Override {
		return false
	}
	return true
}

// Chain 返回该请求的模型候选链（链首为首选）。非虚拟模型且无 model_fallback 时
// 返回单元素链 [model] —— 行为与未启用编排完全一致。
func (c Config) Chain(model string, now func() int, realExists bool) []string {
	chain := []string{model}
	if c.IsVirtual(model, realExists) {
		want := ""
		if idx := strings.IndexByte(model, ':'); idx > 0 {
			if p := model[:idx]; p == "cn" || p == "global" {
				want = p
			}
		}
		if p := c.primary(now(), want); p != "" {
			chain = []string{p}
			chain = append(chain, filterRealm(c.Fallback, want)...)
		}
	}
	return c.expand(chain)
}

// primary 按时段与 realm 挑主模型。hour 为 Asia/Shanghai 的 0-23 小时。
func (c Config) primary(hour int, wantRealm string) string {
	day, night := strings.TrimSpace(c.DayPrimary), strings.TrimSpace(c.NightPrimary)
	if wantRealm != "" {
		// 带 realm 前缀的 auto（如 global:auto）：只在该 realm 的候选里选。
		if day != "" && RealmOf(day) == wantRealm {
			if c.isDay(hour) || night == "" || RealmOf(night) != wantRealm {
				return day
			}
			return night
		}
		if night != "" && RealmOf(night) == wantRealm {
			return night
		}
		for _, m := range c.Fallback {
			if RealmOf(m) == wantRealm {
				return m
			}
		}
		return ""
	}
	if c.isDay(hour) {
		if day != "" {
			return day
		}
		return night
	}
	if night != "" {
		return night
	}
	return day
}

// isDay 当前小时是否落在白天窗口。DayStart==DayEnd 视为全天白天（无夜间窗口）。
func (c Config) isDay(hour int) bool {
	s, e := c.DayStart, c.DayEnd
	if s == e {
		return true
	}
	if s < e {
		return hour >= s && hour < e
	}
	// 跨零点窗口（如 22→6）：少见但语义明确。
	return hour >= s || hour < e
}

// filterRealm 按 realm 过滤候选链；want 为空时原样返回。
func filterRealm(list []string, want string) []string {
	if want == "" {
		return list
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		if m == "" {
			continue
		}
		if RealmOf(m) == want {
			out = append(out, m)
		}
	}
	return out
}

// expand 逐项按 ModelFallback 递归展开（去重、限深、限长，防配置成环）。
func (c Config) expand(chain []string) []string {
	if len(chain) == 1 && c.ModelFallback[chain[0]] == nil {
		return chain
	}
	out := make([]string, 0, len(chain))
	seen := map[string]bool{}
	var walk func(m string, depth int)
	walk = func(m string, depth int) {
		if m == "" || seen[m] || len(out) >= maxChain {
			return
		}
		seen[m] = true
		out = append(out, m)
		if depth >= maxExpandDepth {
			return
		}
		for _, nx := range c.ModelFallback[m] {
			walk(strings.TrimSpace(nx), depth+1)
		}
	}
	for _, m := range chain {
		walk(m, 0)
	}
	if len(out) == 0 {
		return chain
	}
	return out
}

// VirtualIDs 应在 /v1/models 里列出的虚拟模型名。
//
// 裸 "auto"（默认 cn 语义）+ 主模型所在 realm 的 "<realm>:auto"（如 cn:auto）。
// 未启用时返回 nil（列表里不出现虚拟模型）。
func (c Config) VirtualIDs() []string {
	if !c.Enabled {
		return nil
	}
	vid := c.virtualID()
	out := []string{vid}
	seen := map[string]bool{vid: true}
	for _, p := range []string{c.DayPrimary, c.NightPrimary} {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id := RealmOf(p) + ":" + vid
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// kindSet 返回生效的降级类别集合（显式配置优先）。
func (c Config) kindSet() map[string]bool {
	set := make(map[string]bool, len(defaultKinds))
	if len(c.FallbackOn) == 0 {
		for _, k := range defaultKinds {
			set[k] = true
		}
		return set
	}
	for _, k := range c.FallbackOn {
		k = strings.TrimSpace(k)
		if k != "" {
			set[k] = true
		}
	}
	return set
}

// Fallbackable 该错误类别是否应触发模型降级。
func (c Config) Fallbackable(kind string) bool {
	if kind == "" {
		return false
	}
	return c.kindSet()[strings.TrimSpace(kind)]
}
