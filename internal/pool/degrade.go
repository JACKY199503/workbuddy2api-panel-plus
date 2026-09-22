// 连败降权（issue #114）：NoteFailures 计数 + 达阈临时出池 + NoteSuccess 清零。
// 与冷却/熔断「并存取更长者不叠加」由 entry.healthy 的并列或门天然保证
// （三个截止是或门，任一未到期即不可选，生效的一定是最远者——见 entry.degradeUntil 注释）。
package pool

import "time"

// NoteFailures 记录一次「不罚号的失败」（issue #114 连败兜底的喂入口）：
// consecutiveFails++，达到 degradeThreshold 触发降权（degradeUntil = now +
// 降权时长，计数清零供下一轮重新累计——同 recordBreakerFailureLocked 哲学）。
// **已在降权期内**（degradeUntil 未到期）再次达阈：不延长、不翻倍（防用户重试
// 把降权越堆越厚，与 CooldownSoftRate 兜底探测同哲学）——计数清零，到期放行后
// 需重新连败满阈值才再降。成功（NoteSuccess）清零计数并清降权截止。
//
// 谁该喂：ErrClient（未知 4xx，applyErrorPolicy default 只换号不罚）与传输层
// 失败（handler 网络抖动分支，同样只换号不罚）——两者都是「不知道原因的失败」，
// 正是连败兜底的目标形态。带权威分类的错误（429/WAF/402/5xx/12153/11140/11102）
// 各有「知道原因的惩罚」（冷却/熔断/禁用/负缓存），不喂本计数器——惩罚已存在，
// 再喂连败是重复计罚（任务书纪律：两机制并存取更长者，不叠加）。
func (p *Pool) NoteFailures(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.consecutiveFails++
	if e.consecutiveFails < p.degradeThreshold {
		p.dirty.Store(true)
		return
	}
	e.consecutiveFails = 0
	if !e.degradeUntil.IsZero() && time.Now().Before(e.degradeUntil) {
		// 降权期内的连败达阈：不延长（取更长者语义由 healthy 或门保证）。计数
		// 已清零——到期放行后需重新连败满阈值才再降。
		p.dirty.Store(true)
		return
	}
	e.degradeUntil = time.Now().Add(p.degradeDurationLocked())
	p.dirty.Store(true)
}

// degradeDurationLocked 返回降权时长（当前实现为固定 degradeCooldown，钳到
// [defaultDegradeCooldown, degradeCooldownMax]）。不做指数升级：触发计数在达阈时
// 清零、不持久化历史触发次数，档位不可推导；固定时长已满足「临时出池一段时间」
// 的语义（真持续坏号会在下一轮 5 连败里再降一次）。调用方必须已持有 p.mu。
func (p *Pool) degradeDurationLocked() time.Duration {
	d := p.degradeCooldown
	if d <= 0 {
		d = defaultDegradeCooldown
	}
	max := p.degradeCooldownMax
	if max <= 0 {
		max = defaultDegradeCooldownMax
	}
	if d > max {
		d = max
	}
	return d
}
