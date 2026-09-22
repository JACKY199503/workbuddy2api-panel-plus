// model_catalog.go context_length / max_output_tokens 四级查找链 + model.json
// 本地缓存（任务书 model-json-dynamic）。
//
// 查找链（32a3c13 三级 → 四级，上游动态值永远权威不变）：
//  1. 上游动态值（ModelInfo.ContextWindow/MaxTokens）——权威，永远压过 model.json
//     （即使后者更新：上游才是权威，任务书 §清理）；
//  2. 静态种子表（context_catalog.go 的 contextCapFallback，编译期兜底）；
//  3. model.json 本地缓存（数据目录，含运行时从 models.dev 补的值 + 仓库种子
//     embed 的初值）；损坏 → 降级种子并 WARN 不崩溃（手动维护入口的容错）；
//  4. 触发 models.dev 按需拉取（异步，不阻塞本次响应）→ 值写入 model.json →
//     本次先落 1M/省略，下次命中缓存；负缓存 24h。
//
// model.json 读写并发安全（单 sync.Mutex 全程持锁 + 原子落盘 tmp+rename，
// 多请求同时 miss 同一模型只写一次）；文件损坏/不可写均静默降级（种子表→1M）。
//
// 状态归属：包级 catalogState 单例（与 modelsDev fetcher 同模式，进程一份）。
// 测试用 resetModelCatalog / loadModelCatalogAt 隔离。
package upstream

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// modelSeedFS 仓库种子版 model.json（context_catalog 静态表 27 值迁移，source=seed）。
// 只作 model.json 缺失时的初值来源；运行目录的 model.json 一旦存在则以它为准
// （用户手动维护入口——直接编辑文件，格式容错由 loadModelCatalog 的校验兜底）。
//
//go:embed model.json
var modelSeedFS embed.FS

// ModelCapEntry model.json 单条目（与静态表同字段口径 + 来源与抓取时间）。
// Source：seed（仓库种子迁移）/ modelsdev（运行时按需拉取）/ manual（用户手编
// ——无法区分手编与 seed，手编条目保留其原 source 字符串，语义等同「非拉取」）。
// Context 必须 >0（零/负条目校验拒绝）；MaxOutput 0 = 输出上限未知 → 省略字段。
type ModelCapEntry struct {
	ContextLength   int64  `json:"context_length"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
	FetchedAt       string `json:"fetched_at,omitempty"` // RFC3339；seed 条目为空
	Source          string `json:"source"`
}

// modelCatalog model.json 缓存状态机（包级 catalogState 单例的字段载体）。
type modelCatalog struct {
	mu sync.Mutex

	path    string // 落盘路径（空 = 禁用持久化：纯内存 + 种子）
	entries map[string]ModelCapEntry
	loaded  bool // entries 已初始化（含损坏降级形态）
}

// catalogState 包级单例：全进程一份 model.json 状态。
var catalogState = &modelCatalog{}

// initModelCatalogLocked 确保 entries 已加载（持锁调用）：
//   - 运行目录 model.json 存在且合法 → 全量加载（校验失败的条目剔除并 WARN）；
//   - 文件不存在 / 整体损坏（非法 JSON）→ 种子 embed 初值（不落盘：保持「用户
//     尚无缓存」状态，首次拉取成功后再落盘）；
//   - path 为空（未接线，如测试/工具进程）→ 种子初值。
func (c *modelCatalog) initLocked() {
	if c.loaded {
		return
	}
	c.entries = map[string]ModelCapEntry{}
	c.loaded = true
	if c.path == "" {
		c.loadSeedLocked()
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		// 不存在 / 不可读 → 种子（首次启动形态）。
		c.loadSeedLocked()
		return
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		// 整体损坏：降级种子 + WARN 不崩溃（任务书 §手动维护入口容错）。
		log.Printf("WARN: [upstream] model.json 损坏（降级内置种子表）: path=%s err=%v", c.path, err)
		c.loadSeedLocked()
		return
	}
	for id, e := range file {
		if !validCapEntry(e) {
			log.Printf("WARN: [upstream] model.json 条目非法剔除: model=%s entry=%+v", id, e)
			continue
		}
		c.entries[id] = e
	}
}

// loadSeedLocked 把仓库种子 model.json 灌入 entries（持锁调用）。
func (c *modelCatalog) loadSeedLocked() {
	raw, err := modelSeedFS.ReadFile("model.json")
	if err != nil {
		// embed 编译期保证存在，理论不可达；防御性兜底走静态表（initLocked 调用方
		// 查找链第 2 级本来就会兜，这里只需保持 entries 为空）。
		return
	}
	var seed map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &seed); err != nil {
		return
	}
	for id, e := range seed {
		if validCapEntry(e) {
			c.entries[id] = e
		}
	}
}

// validCapEntry 条目级校验：context 正数 + 输出非负（任务书 §值校验的写入侧）。
func validCapEntry(e ModelCapEntry) bool {
	return e.ContextLength > 0 && e.MaxOutputTokens >= 0
}

// get 查 model.json 缓存（第 3 级）。返回条目与是否命中。
// 只读内存，不发网络；加载与网络动作由 ensure/触发侧负责。
func (c *modelCatalog) get(model string) (ModelCapEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	e, ok := c.entries[model]
	if !ok || !validCapEntry(e) {
		return ModelCapEntry{}, false
	}
	return e, true
}

// put 写一条缓存（第 4 级拉取成功后调用）并落盘。同模型已存在（用户手动维护过）
// 仍覆盖——运行时拉取值采信 models.dev 官方源优先口径，比手编更可信；
// 若不希望覆盖，删掉 model.json 里对应条目即可（加载后手编值只在本次进程生效）。
func (c *modelCatalog) put(model string, e ModelCapEntry) {
	if model == "" || !validCapEntry(e) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.entries[model] = e
	c.saveLocked()
}

// saveLocked 原子落盘（tmp + rename，pool state.json 同模式）。持锁调用。
// path 为空 / 目录不可写 / 序列化失败 → 静默（内存缓存仍生效，下次进程重拉）。
func (c *modelCatalog) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [upstream] model.json 落盘失败（内存缓存仍生效）: path=%s err=%v", c.path, err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("WARN: [upstream] model.json 落盘改名失败: path=%s err=%v", c.path, err)
	}
}

// ---- 包级 API（查找链 3/4 级 + 接线）----

// SetModelCatalogPath 接线 model.json 落盘路径（cmd/server 启动时调用，
// 数据目录与 state.json 同风格）。首次调用生效；后续调用在已加载后仅更新路径
// （不重载——进程内以内存 entries 为准）。
func SetModelCatalogPath(path string) {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
}

// modelCatalogGet 第 3 级：model.json 缓存命中（内含种子初值与损坏降级）。
func modelCatalogGet(model string) (ModelCapEntry, bool) {
	return catalogState.get(model)
}

// modelCatalogPut 第 4 级写入：models.dev 拉到的值落缓存（含落盘）。
func modelCatalogPut(model string, context, maxOutput int64) {
	modelCatalogPutSourced(model, context, maxOutput, "modelsdev")
}

// modelCatalogPutSourced 写入指定来源的条目（测试可注入 fetched_at 检查落盘格式）。
func modelCatalogPutSourced(model string, context, maxOutput int64, source string) {
	catalogState.put(model, ModelCapEntry{
		ContextLength:   context,
		MaxOutputTokens: maxOutput,
		FetchedAt:       time.Now().UTC().Format(time.RFC3339),
		Source:          source,
	})
}

// resetModelCatalog 测试隔离：清空单例状态（entries/path/loaded）。
func resetModelCatalog() {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.path = ""
	c.loaded = false
}

// loadModelCatalogAt 测试接线：指向指定路径后立即触发一次加载（同步，可断言文件
// 解析行为）。生产路径用 SetModelCatalogPath（惰性首次 get 触发加载）。
func loadModelCatalogAt(path string) {
	SetModelCatalogPath(path)
	catalogState.mu.Lock()
	defer catalogState.mu.Unlock()
	catalogState.initLocked()
}

// ResetLookupChainForTest 跨包测试钩子：清空 model.json 缓存与 models.dev fetcher
// 的全部包级单例状态（含在途拉取冷却——防 server 包测试末尾的异步 goroutine 打
// 真网、防跨测试缓存污染）。仅测试引用（upstream 包内用 resetModelsDev /
// resetModelCatalog 等价内联）。
func ResetLookupChainForTest() {
	resetModelCatalog()
	resetModelsDev()
}

// ---- 四级查找链（对 handler 暴露的入口，签名与 32a3c13 三级版兼容）----

// ContextWindowListingV4 四级查找链的 context_length 决策：
//  1. remote>0 权威透出（上游动态值永远压过 model.json，任务书 §清理）；
//  2. 静态种子表（contextCapFallback）；
//  3. model.json 缓存（内含种子初值 / models.dev 运行时补充值）；
//  4. 全链 miss 且非负缓存 → 异步触发 models.dev 拉取（本次返回 DefaultContextWindow
//     1M，不阻塞；拉到后写 model.json 供下次命中）。
func ContextWindowListingV4(model string, remote int64, client *http.Client) int64 {
	if remote > 0 {
		return remote
	}
	if model == "" {
		return DefaultContextWindow
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context // 第 2 级：静态种子表（编译期兜底，永远可用）
	}
	if e, ok := modelCatalogGet(model); ok {
		return e.ContextLength // 第 3 级：model.json
	}
	if !modelsDev.negativeFresh(model) {
		// 第 4 级触发：先查进程内文档索引（拉过一次即常驻），命中直接入缓存
		// 返回（不等待异步拉取）；未命中 → 记负缓存 + 异步拉取（本次先回 1M）。
		if e, ok := modelsDev.lookup(model); ok {
			modelCatalogPut(model, e.Context, e.Output)
			return e.Context
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return DefaultContextWindow // 第 4 级兜底：1M（本次先回，拉到后下次命中）
}

// MaxOutputTokensListingV4 四级查找链的 max_output_tokens 决策（与 context 口径
// 刻意不同：未知 → 省略，无「宁可高估」安全侧）。
func MaxOutputTokensListingV4(model string, remote int64, client *http.Client) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if model == "" {
		return 0, false
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true // 第 2 级
	}
	if e, ok := modelCatalogGet(model); ok && e.MaxOutputTokens > 0 {
		return e.MaxOutputTokens, true // 第 3 级
	}
	if !modelsDev.negativeFresh(model) {
		// 与 ContextWindowListingV4 同触发：lookup 命中先入缓存再返回
		// （两条查找链并发 miss 同一模型时，第二调用方直接拿到刚写入的值）。
		if e, ok := modelsDev.lookup(model); ok {
			modelCatalogPut(model, e.Context, e.Output)
			return e.Output, e.Output > 0
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return 0, false // 第 4 级兜底：省略（输出上限不编造）
}

// ---- 第 4 级拉取值的回流（fetchDoc 成功后调用，写 model.json）----

// noteModelsDevMiss 查询未命中 models.dev 索引 → 负缓存已由 lookup 记录。
// 本函数是 lookup + 写缓存的粘合层：fetchDoc 拉到文档后对「曾 miss 过的模型」
// 重查一次并写入 model.json（下次 /v1/models 直接命中第 3 级）。
func (f *modelsDevFetcher) backfillMisses() {
	f.mu.Lock()
	doc := f.doc
	missed := make([]string, 0, len(f.negatives))
	for m := range f.negatives {
		missed = append(missed, m)
	}
	f.mu.Unlock()
	if doc == nil {
		return
	}
	for _, m := range missed {
		e, ok := doc[m]
		if !ok {
			continue // 仍查不到：负缓存 24h 生效，不写
		}
		modelCatalogPut(m, e.Context, e.Output)
		// 回流成功：清除负缓存条目（该模型已有值，后续走第 3 级缓存，
		// 不再进本清单）。
		f.mu.Lock()
		delete(f.negatives, m)
		f.mu.Unlock()
	}
}
