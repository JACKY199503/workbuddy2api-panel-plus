// context_catalog.go context_length / max_output_tokens 字段级静态兜底知识表。
//
// 数据来源四级（model-json-dynamic 任务书；查找链入口在 model_catalog.go 的
// ContextWindowListingV4 / MaxOutputTokensListingV4，本文件是第 2 级）：
//   - 上游动态值（ModelInfo.ContextWindow/MaxTokens，即 maxInputTokens/maxOutputTokens）权威，优先；
//   - 本文件静态知识表（远端零值时补齐；model.json 缺失/损坏时的编译期兜底）；
//   - model.json 本地缓存（数据目录，含运行时 models.dev 按需补值，见 model_catalog.go）；
//   - models.dev 按需拉取（异步不阻塞；仍未知 → context_length 1M 兜底——宁可高估
//     不低估：高估代价是客户端不截断、上游报错可重试；低估代价是下游客户端
//     （Codex/ZCode/Claude Code 按 context_length 提前截断）白白丢上下文；
//     max_output_tokens 未知 → 省略字段（输出上限无合估算据，不编造））。
//
// 知识表**一处定义、CN/global 两域共用**：context_length 是模型固有属性——fork
// 706412584 实测结论「两区是同一套 API 的两次部署」，同 id 上下文一致，无按 realm
// 分表必要（与 effort 档位的 realm 分表刻意不同）。
//
// 值来源两类，逐条注释标注：
//   - fork 实测：706412584 直连上游 /console/enterprises/personal/models 的
//     maxInputTokens（/tmp/fork_diffs/70_927f57f9.diff，2026-09-13 实测；
//     global 侧无法直测、按同 id 外推）；
//   - models.dev：https://models.dev/ 收录值（2026-09-16 查询，取多 provider 共识值；
//     官方源如 moonshotai/zai 优先）。未收录或歧义大者不编造。
package upstream

// DefaultContextWindow 知识表也未收录的模型的 context_length 兜底：1M。
// 上游多数大窗口模型的实际量级；高估优于低估（见文件头）。
const DefaultContextWindow int64 = 1000000

// contextCap 一个模型的上下文能力（字段级兜底条目）。
// context 必为正（否则条目无意义，直接走 1M 兜底）；
// maxOutput 为 0 表示输出上限未知 → max_output_tokens 字段省略（不编造）。
type contextCap struct {
	context   int64
	maxOutput int64
}

// contextCapFallback context_length / max_output_tokens 知识表（CN/global 共用）。
// 每条注释标注来源：实测 = fork 706412584 直连 CN /console 实测 maxInputTokens
// （global 侧为同 id 外推）；models.dev = 2026-09-16 收录共识值；估算 = 同族外推。
var contextCapFallback = map[string]contextCap{
	// ---- GLM 家族（z-ai）----
	"glm-5.2":       {context: 1000000, maxOutput: 131072}, // 实测（CN 1M；models.dev 共识 1M/131072）
	"glm-5.1":       {context: 200000, maxOutput: 131072},  // 实测（CN 200K；models.dev 共识 200K/131072）
	"glm-5.3":       {context: 1000000, maxOutput: 131072}, // 实测外推 + models.dev 共识 1M/131072
	"glm-5.3-flash": {context: 1000000, maxOutput: 131072}, // models.dev 共识 1M/131072
	"glm-5v-turbo":  {context: 200000, maxOutput: 131072},  // 实测（CN 200K；models.dev 共识 200K/131072）

	// ---- Kimi 家族（moonshot）----
	"kimi-k2.7":         {context: 256000, maxOutput: 65536},   // 实测（CN 256K）；输出 65536 为 models.dev kimi-k2.7-code 同族估算
	"kimi-k2.6":         {context: 256000, maxOutput: 262144},  // 实测（CN 256K）；输出 models.dev 官方 262144
	"kimi-k2.5":         {context: 164000, maxOutput: 262144},  // 实测（global 侧同 id 外推 164K）；输出 models.dev 共识 262144
	"kimi-k3":           {context: 1048576, maxOutput: 131072}, // models.dev 官方（moonshotai 1M/128K）
	"kimi-k2.8-preview": {context: 1048576, maxOutput: 0},      // models.dev（Kimi K2.8 Preview 1M；输出上限未收录，省略）

	// ---- MiniMax / 混元（tencent）----
	"minimax-m3":    {context: 512000, maxOutput: 512000}, // 实测（CN 512K）；输出 models.dev 共识 512000
	"hy3":           {context: 192000, maxOutput: 64000},  // 实测（CN 192K/64K，repo hy3 抓取样本同值）
	"hy3-preview":   {context: 262144, maxOutput: 64000},  // models.dev（共识 262144/64000）
	"hy4-preview":   {context: 1000000, maxOutput: 64000}, // 实测外推 + models.dev（~1M/64000）
	"hy4-preview-x": {context: 1000000, maxOutput: 64000}, // 实测外推（1M）；输出同族 hy4-preview 估算

	// ---- DeepSeek 家族 ----
	"deepseek-v4-pro":     {context: 1000000, maxOutput: 384000}, // 实测（CN 1M；models.dev 共识 1M/384000）
	"deepseek-v4-flash":   {context: 1000000, maxOutput: 384000}, // 实测（CN 1M；models.dev 共识 1M/384000）
	"deepseek-v4.1-flash": {context: 1000000, maxOutput: 384000}, // 实测外推 + models.dev 共识 1M/384000

	// ---- OpenAI / Google（global 域家族）----
	"gpt-6-astra":      {context: 1050000, maxOutput: 128000}, // models.dev（全 provider 一致 1050000/128000）
	"gpt-5.6-sol":      {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-terra":    {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.6-luna":     {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.5":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.4":          {context: 1050000, maxOutput: 128000}, // models.dev 共识
	"gpt-5.3-codex":    {context: 400000, maxOutput: 128000},  // models.dev（全 provider 一致 400000/128000）
	"gemini-3.5-flash": {context: 1048576, maxOutput: 65536},  // models.dev 共识

	// ---- global 域路由别名/别名模型 ----
	"auto": {context: 168000, maxOutput: 0}, // 实测外推（fork global 静态表 168K）；输出上限未知，省略
}

// ContextWindowListing 模型在 /v1/models 的 context_length（三级查找）：
// remote（上游 maxInputTokens）>0 时权威；否则查知识表；仍未收录 → DefaultContextWindow
// （1M，宁可高估不低估）。绝不再透出假 131072。
func ContextWindowListing(model string, remote int64) int64 {
	if remote > 0 {
		return remote
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context
	}
	return DefaultContextWindow
}

// MaxOutputTokensListing 模型在 /v1/models 的 max_output_tokens（三级查找）：
// remote（上游 maxOutputTokens）>0 时权威；否则查知识表；仍未收录 → ok=false
// （调用方省略字段，不编造输出上限）。与 ContextWindowListing 的 1M 兜底刻意不同：
// 输出上限无「宁可高估」的安全侧，未知即省略。
func MaxOutputTokensListing(model string, remote int64) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true
	}
	return 0, false
}
