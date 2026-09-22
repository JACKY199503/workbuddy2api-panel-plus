// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（后台 goroutine + 失败仅 debug 日志），读操作只发生在启动时
// （加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
package redisstore

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

// writeConcurrencyLimit fire-and-forget 异步写的在途上限（发现 4：写 goroutine
// 无信号量限制，高写入速率下可瞬时堆积）。超过的排队不丢弃——写语义不变（见 goWrite）。
const writeConcurrencyLimit = 8

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
	// Close 关停 Store：Upstash 等待已提交的异步写全部执行完再关底层连接
	// （停机语义：最后一笔 Redis 镜像必须写完），之后新提交的写直接丢弃；幂等。
	// Noop 为空操作。进程退出前在 pool.Close() 之后调用。
	Close() error
}

const (
	bindPrefix  = "wb2api:bind:"
	stateKey    = "wb2api:state"
	readTimeout = 3 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] 警告: redis 连接串解析失败 (%v)，降级 Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)
	return &Upstash{
		client: client,
		sem:    make(chan struct{}, writeConcurrencyLimit),
		done:   make(chan struct{}),
	}
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 若 url 本身已含 scheme（rediss://、redis://、https://...upstash.io 等）：
//   - rediss:// 或 redis:// 原样返回（已是完整连接串）
//   - 其余（如 https://xxx.upstash.io）剥掉 "://" 前缀只取 host，再按
//     "rediss://default:<token>@<host>:6379" 组装
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash 真实现：redis.Client 封装。
//
// 写并发上限（发现 4）：三个异步写共享 sem（cap=writeConcurrencyLimit 的信号量），
// 在途写超过上限时新写排队不丢弃——语义仍是 fire-and-forget，只是把"无限堆积"
// 收敛为"有界排队"。Close 前已提交的写（含排队中）保证执行完，Close 后新提交
// 的写直接丢弃。
type Upstash struct {
	client *redis.Client
	// sem 写信号量（有界在途写）。cap=1 时退化为串行写，供测试观察调度语义。
	sem chan struct{}
	// done 关停标志（Close 关闭）。sem 与 done 由 New 初始化；测试可直接构造
	//（client=nil，goWrite/Close 不触网络）。
	done chan struct{}
	// closeOnce 保证 Close 幂等（多次调用只关一次 done channel）。
	closeOnce sync.Once
	// submitMu 收窄 goWrite 的提交/关停竞态：goWrite 先登记 wg 再查 done，
	// Close 先关 done 再等 wg——两侧互斥后，「Close 前提交的写必然执行」
	// 不再依赖 goroutine 调度时序（83d18ae 原版存在窗口：排队写在 Close
	// 关 done 之后才跑到检查点会被误丢，close_test.go:138 稳定复现）。
	submitMu sync.Mutex
	// wg 已提交未完成的写（Close 排空用）。
	wg sync.WaitGroup
}

// goWrite 以 fire-and-forget 方式执行 fn：写槽（sem）有界并发，Close 前提交的写
// 必然执行（停机镜像完整性），Close 后提交的写直接丢弃（进程已在退出）。
func (u *Upstash) goWrite(fn func()) {
	u.closeOnceGuard()
	u.submitMu.Lock()
	u.wg.Add(1)
	select {
	case <-u.done:
		u.submitMu.Unlock()
		u.wg.Done() // 关停后提交的写：登记即撤销，直接丢弃
		return
	default:
	}
	u.submitMu.Unlock()
	go func() {
		defer u.wg.Done()
		// 只阻塞抢写槽，不检查 done：此处若select done，已阻塞排队的写会在
		// close(done) 唤醒时全部走丢弃分支（selrand5 实测 100%），「Close 前
		// 提交的写（含排队中）必然执行」的契约被内部检查破坏——close_test.go:138
		// 因此间歇失败（约 20%：取决于 write-1/write-2 谁先抢到唯一槽）。丢弃
		// 语义已由提交点（submitMu 下的 done 检查）唯一承担；此处提交已冻结在
		// wg 中，Close 的 wg.Wait 必然等到它执行完。
		u.sem <- struct{}{}
		defer func() { <-u.sem }()
		fn()
	}()
}

// closeOnceGuard 防零值 Upstash（未经 New 构造）在 goWrite/Close 上 nil-map 式崩溃：
// sem/done 为 nil 时补建（cap=1）。仅测试会走到该路径。
func (u *Upstash) closeOnceGuard() {
	if u.sem == nil || u.done == nil {
		u.sem = make(chan struct{}, 1)
		u.done = make(chan struct{})
	}
}

// Close 等待已提交的异步写全部执行完毕，再关底层 redis 连接；幂等。
// 之后新提交的写直接丢弃（goWrite 的 done 检查）。停机路径在 pool.Close() 后调用：
// pool 的最后一次 Flush→SaveState 已提交，本方法保证它写完才返回。
func (u *Upstash) Close() error {
	u.closeOnceGuard()
	// 先关 done 再等 wg：与 goWrite 的 submitMu 互斥后，此时「已提交的写」集合
	// 已冻结（此后新提交的直接丢弃），wg.Wait 排空即覆盖在途 + 排队两层——
	// 原 sem 探测法在排队写尚未跑到抢槽点时会误判已排空（83d18ae 竞态）。
	u.submitMu.Lock()
	u.closeOnce.Do(func() { close(u.done) })
	u.submitMu.Unlock()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// 兜底超时（单写上限 5s，10s 富余）：卡死的写不应阻塞进程退出。
		log.Printf("[redisstore] WARN: Close 等待在途写超时，放弃（镜像可能未写完）")
	}
	return u.closeClient()
}

// closeClient 关底层 redis 连接（client 为 nil——测试构造——时跳过）。
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}
	return u.client.Close()
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState 异步写池状态 JSON 快照。
func (u *Upstash) SaveState(data []byte) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
	})
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀）。
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		v, err := u.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
func (Noop) Close() error                          { return nil }
