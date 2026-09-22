// modelsdev.go models.dev 按需兜底（context_length 四级查找链的第 4 级）。
//
// 定位：只对「上游动态值缺失 + 静态种子表未收录 + model.json 未缓存」的模型查
// models.dev，是兜底的兜底——超时短（默认 5s）、失败静默降级（context_length 落
// DefaultContextWindow=1M），绝不阻塞 /v1/models 主路径（查找异步化，本次请求
// 直接返回兜底值，拉到后写 model.json 供下次命中）。
//
// 数据源（2026-09-16 逆向，结论详见 .claude/reports/model-json-dynamic.md）：
//   - 官方聚合 JSON 端点 https://models.dev/api.json：~4.7MB 单文档、217 provider、
//     免鉴权、Cloudflare 托管静态站；
//   - 无按模型/按 provider 子端点（/z-ai.json 等 302 回 /），「按需」的实现是
//     单次拉全量文档 + 建裸 id 索引（拉一次只发生一次，此后进程内复用索引）；
//   - schema：{ "<provider>": { "models": { "<id>": { "limit": {"context": N,
//     "output": N} } } } }，模型 id 有裸名（glm-5.2）与带命名空间（openai/gpt-5.5）
//     两种形态，均取尾段做索引 key；
//   - 多 provider 同名值会分歧（聚合网关常自报改动）：vendor 官方源（zai/
//     moonshotai/openai/google/deepseek/minimax）优先，其余取众数（共识值）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelsDevURL models.dev 官方聚合 JSON 端点（唯一端点，见文件头逆向结论）。
const ModelsDevURL = "https://models.dev/api.json"

// modelsDevTimeout 单次拉取超时：兜底的兜底，不值得等（任务书 §2：如 5s）。
const modelsDevTimeout = 5 * time.Second

// modelsDevFetchCooldown 拉取节流（进程级）：文档是全量聚合体，5min 内不重拉
// （同模型 24h 负缓存之外的整体节流，防短窗反复打 models.dev）。
const modelsDevFetchCooldown = 5 * time.Minute

// modelsDevNegativeTTL 同模型负缓存：查不到的模型 24h 内不重查
// （任务书 §2：如同模型 24h 内不重查，查不到的模型负缓存防反复打）。
const modelsDevNegativeTTL = 24 * time.Hour

// modelsDevNegativesSoftCap 负缓存 map 的软上限：规模超过它时才做一次过期条目淘汰
// 扫描。分批摊销是为避免每次 lookup 都做 O(n) 全扫——第 4 级触发点在 /v1/models 里
// 每个模型各调一次（handler 遍历模型列表逐条 V4），n 大时全扫会被请求数放大成 CPU
// 开销。淘汰只针对 TTL 已过期的条目，故低于上限时不扫也不会让任何有效条目过期失效。
//
// 注意「只扫过期条目」不足以保证有界：TTL 从**首次未命中**起算（见 lookup 的写回
// 判断），若模型名持续出现在 /v1/models 名单里，一个 TTL 窗口内所有条目都会被反复
// 盖章成 fresh，扫描一条也删不掉，规模只增不减。因此超过**硬上限**（两倍软上限）
// 时额外丢弃最旧的条目——负缓存是纯性能优化（miss 时多查一次进程内索引），丢条目
// 只可能让某个模型重新进一次扫描，不改变任何对外语义。
const modelsDevNegativesSoftCap = 1024

// modelsDevNegativesHardCap 负缓存 map 的硬上限：超过即按时间序丢弃最旧条目
// （见 modelsDevNegativesSoftCap 注释）。取两倍软上限，给「一轮 /v1/models 新增」
// 留出余量，正常规模远达不到。
const modelsDevNegativesHardCap = 2 * modelsDevNegativesSoftCap

// modelsDevMaxBody 拉取响应体上限（文档实测 ~4.7MB，留余量；防异常大响应拖死）。
const modelsDevMaxBody = 32 << 20

// modelsDevValueMax 值校验上限：context/output 超过 1e9 视为脏数据拒绝
// （量级上限校验，任务书 §2 值校验；正数下界在 catalog 写入侧兜底）。
const modelsDevValueMax = int64(1e9)

// modelsDevVendorSources 官方 vendor provider 优先名单：models.dev 收录 217 个
// provider，聚合网关（merge-gateway/nano-gpt 等）自报的 limit 常与官方源分歧，
// 采值优先级 = 本名单命中 > 众数共识。
var modelsDevVendorSources = map[string]bool{
	"zai":           true, // Z.AI（glm 家族官方）
	"moonshotai":    true, // Moonshot AI（kimi 家族官方，国际版）
	"moonshotai-cn": true, // Moonshot AI 中国版
	"openai":        true,
	"google":        true,
	"deepseek":      true,
	"minimax":       true,
}

// modelsDevEntry models.dev 单模型采值结果（modelsdev json 的 limit 子集）。
type modelsDevEntry struct {
	Context int64
	Output  int64
}

// modelsDevFetcher models.dev 按需拉取器：进程级单例语义（包级变量 modelsDev），
// 拉取节流 + 裸 id 索引缓存 + 同模型负缓存。测试用 resetModelsDev / 独立 base URL
// 注入隔离（newModelsDevForTest）。
type modelsDevFetcher struct {
	mu sync.Mutex

	// doc 文档解析后的裸 id 索引（多 provider 同名合并采值：vendor 优先/众数）。
	// nil = 未拉取；空 map（非 nil）= 拉取过但索引为空（视作失败冷却）。
	doc map[string]modelsDevEntry

	lastFetch time.Time // 最近一次拉取尝试（成功与失败都算，冷却节流）
	fetched   bool      // 是否已拉取过（doc 字段区分成败）

	negatives map[string]time.Time // 查询未命中的模型 → 记录时间（24h 负缓存）
}

// modelsDev 包级拉取器实例（单例：全进程共享一份文档索引与节流状态）。
var modelsDev = &modelsDevFetcher{}

// resetModelsDev 测试隔离：清空单例状态（doc/fetched/lastFetch/negatives）。
func resetModelsDev() {
	modelsDev.mu.Lock()
	modelsDev.doc = nil
	modelsDev.fetched = false
	modelsDev.lastFetch = time.Time{}
	modelsDev.negatives = nil
	modelsDev.mu.Unlock()
}

// lookup 查询一个模型的 (context, output, found)：
//   - 索引命中且值合法 → found=true；
//   - 索引未命中（含索引尚未就绪）→ 记入 negatives（既是 24h 负缓存，也是
//     fetchDoc 成功后 backfillMisses 的回流清单——「曾 miss 过的模型」），found=false。
//
// 只读内存索引，不发网络请求；网络动作由 ensureDocAsync（goroutine 内）负责。
// 注意：doc 就绪前的 miss 也记 negatives——backfillMisses 回流时查到即写
// model.json 并清除负缓存条目（freshLookup），查不到的保持 24h 负缓存。
func (f *modelsDevFetcher) lookup(model string) (modelsDevEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc != nil {
		if e, ok := f.doc[model]; ok {
			return e, true
		}
	}
	// 未命中（索引在但模型不在，或索引尚未就绪）：记 miss。
	if f.negatives == nil {
		f.negatives = make(map[string]time.Time)
	}
	now := time.Now()
	// 未命中「重复查询」不再刷新记录时刻：TTL 只从**首次未命中**起算。
	// 否则覆盖写等于每次查询都把条目续期——一个持续出现在 /v1/models 名单里的
	// 未知模型（每条 listing 经 ContextWindowListingV4 + MaxOutputTokensListingV4
	// 各查一次，即每请求 2 次写回）其条目永远 fresh，下面的惰性淘汰一条也扫不掉；
	// 又因淘汰只在超软上限时才扫，规模超限后只增不减（进程生命周期内无界增长）。
	// 续期对行为零影响：negativeFresh 只判是否存在 + within TTL，未过 TTL 的条目
	// 无论是否续期都同样短路第 4 级触发。
	if _, seen := f.negatives[model]; !seen {
		f.negatives[model] = now
	}
	// 惰性淘汰已过期的负缓存条目：TTL 到期后 negativeFresh 本就判 false（等效不存在），
	// 条目继续留着只是内存泄漏——models.dev 永不收录的模型名（model 由客户端任意指定）
	// 查一次就永久驻留，而全库唯一的删除点 backfillMisses 只删「文档里查到」的模型，
	// 永远不会回收这些条目，map 在进程生命周期内无界增长。
	// 仅在规模超软上限时才扫一遍（摊销 O(1)，理由见 modelsDevNegativesSoftCap 注释）。
	if len(f.negatives) > modelsDevNegativesSoftCap {
		for m, t := range f.negatives {
			if now.Sub(t) >= modelsDevNegativeTTL {
				delete(f.negatives, m)
			}
		}
	}
	// 硬上限兜底：TTL 未到期的条目本就无可淘汰（上一轮扫描已删净过期项），若规模
	// 仍超硬上限说明输入模型名太多，按时间序丢弃最旧条目、削回软上限
	// （纯性能优化：负缓存 miss 只会多查一次进程内索引，丢条目无语义影响）。
	if len(f.negatives) > modelsDevNegativesHardCap {
		cut := len(f.negatives) - modelsDevNegativesSoftCap
		oldest := make([]string, 0, len(f.negatives))
		for m := range f.negatives {
			oldest = append(oldest, m)
		}
		sort.Slice(oldest, func(i, j int) bool { return f.negatives[oldest[i]].Before(f.negatives[oldest[j]]) })
		for _, m := range oldest[:cut] {
			delete(f.negatives, m)
		}
	}
	return modelsDevEntry{}, false
}

// negativeFresh 模型是否在负缓存有效期内（供查找链短路第 4 级触发）。
func (f *modelsDevFetcher) negativeFresh(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.negatives[model]
	return ok && time.Since(t) < modelsDevNegativeTTL
}

// ensureDocAsync 确保 models.dev 文档索引可用（异步，不阻塞调用方）：
// 已有索引 / 拉取冷却期内 / 已有人在拉（in-flight 去重）→ 直接返回。
// 否则起 goroutine 拉取解析，完成后落 f.doc（失败静默：只刷新 lastFetch 冷却，
// 下次查找仍走 1M 兜底，不重试风暴）。
func (f *modelsDevFetcher) ensureDocAsync(client *http.Client, baseOverride string) {
	f.mu.Lock()
	if f.doc != nil {
		f.mu.Unlock()
		return
	}
	if f.fetched && time.Since(f.lastFetch) < modelsDevFetchCooldown {
		// 拉取过（成功或失败）且冷却期内：不再打 models.dev。
		f.mu.Unlock()
		return
	}
	// in-flight 去重：把 fetched/lastFetch 先置为「本次进行中」，
	// 后续并发调用在冷却窗口内直接返回，不重复起拉取。
	f.fetched = true
	f.lastFetch = time.Now()
	f.mu.Unlock()

	go f.fetchDoc(client, baseOverride)
}

// fetchDoc 拉取并解析 models.dev 文档，建裸 id 索引（goroutine 内执行，永不 panic
// 上抛：任何失败只静默冷却）。
// 拒绝 nil client（不回落 http.DefaultClient）：生产调用方恒传非 nil，nil 只意味着
// 测试疏漏——DefaultClient 无超时（挂起隐患）且会打真网（测试污染 + 不确定延迟），
// 静默 WARN + 返回（与 fetch 失败同语义，降级 1M 兜底）让疏漏显式化。
func (f *modelsDevFetcher) fetchDoc(client *http.Client, baseOverride string) {
	if client == nil {
		log.Printf("WARN: [upstream] models.dev fetch: nil client rejected (no DefaultClient fallback, silent fallback to 1M)")
		return
	}
	url := ModelsDevURL
	if baseOverride != "" {
		url = baseOverride
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch: build request: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch failed (silent fallback to 1M): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("WARN: [upstream] models.dev fetch status %d (silent fallback to 1M)", resp.StatusCode)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBody))
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch read: %v", err)
		return
	}
	doc, err := parseModelsDevDoc(raw)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev parse failed (silent fallback to 1M): %v", err)
		return
	}
	f.mu.Lock()
	f.doc = doc
	f.mu.Unlock()
	// 文档就绪后把「曾 miss 过的模型」回流 model.json（第 4 级 → 第 3 级，
	// 下次 /v1/models 直接命中缓存）。仍查不到的保持负缓存。
	f.backfillMisses()
}

// parseModelsDevDoc 解析 models.dev api.json：{provider:{models:{id:{limit:{context,
// output}}}}} → 裸 id 索引。同名多 provider 采值优先级四级：官方 vendor 源
// （modelsDevVendorSources）> 票数众数 > provider 字典序 > 先出现。
// 第 3 级 provider 字典序是确定性 tie-break：聚合时维护候选的最小 provider 名
// （minProvider，同 doc 稳定的选择器身份），消灭 map 迭代序随机化导致的
// 「同票先到先得」值抖动（同 binary 两次拉取同一文档可能落不同的值进 model.json，
// /v1/models 的 context_length 不可复现）。不引入「值字典序」——那会把
// 「选谁」变成「选什么值」的启发式，语义不如 provider 名干净。
func parseModelsDevDoc(raw []byte) (map[string]modelsDevEntry, error) {
	var doc map[string]struct {
		Models map[string]struct {
			Limit *struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("models.dev doc: %w", err)
	}
	// 同名 id 的候选值收集：vendorOfficial 标记官方源，votes 计众数，
	// minProvider 维护该候选已见的最小 provider 名（tie-break 用）。
	type candidate struct {
		entry       modelsDevEntry
		vendor      bool
		votes       int
		aggKey      string // 去重聚合 key（同值多 provider 只计票不重复存）
		minProvider string
	}
	byModel := map[string][]candidate{}
	for provider, pv := range doc {
		for fullID, mv := range pv.Models {
			if mv.Limit == nil {
				continue
			}
			id := fullID
			if i := strings.LastIndex(fullID, "/"); i >= 0 {
				id = fullID[i+1:]
			}
			if id == "" {
				continue
			}
			// 值校验（任务书 §2）：正数 + 量级上限，脏值不进索引。
			ctx, out := mv.Limit.Context, mv.Limit.Output
			if ctx <= 0 || ctx > modelsDevValueMax {
				continue
			}
			if out < 0 || out > modelsDevValueMax {
				continue
			}
			key := fmt.Sprintf("%d/%d", ctx, out)
			cs := byModel[id]
			dup := false
			for i := range cs {
				if cs[i].aggKey == key {
					cs[i].votes++
					if modelsDevVendorSources[provider] {
						cs[i].vendor = true
					}
					if provider < cs[i].minProvider {
						cs[i].minProvider = provider
					}
					dup = true
					break
				}
			}
			if !dup {
				byModel[id] = append(cs, candidate{
					entry:       modelsDevEntry{Context: ctx, Output: out},
					vendor:      modelsDevVendorSources[provider],
					votes:       1,
					aggKey:      key,
					minProvider: provider,
				})
			}
		}
	}
	out := make(map[string]modelsDevEntry, len(byModel))
	for id, cs := range byModel {
		best := 0
		for i, c := range cs {
			// 优先级：官方 vendor 源 > 票数众数 > provider 字典序（tie-break 确定性）。
			cur := cs[best]
			better := false
			if c.vendor && !cur.vendor {
				better = true
			} else if c.vendor == cur.vendor && c.votes > cur.votes {
				better = true
			} else if c.vendor == cur.vendor && c.votes == cur.votes && c.minProvider < cur.minProvider {
				better = true
			}
			if better {
				best = i
			}
		}
		out[id] = cs[best].entry
	}
	return out, nil
}
