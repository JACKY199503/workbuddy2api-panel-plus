// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		r.entries = append(r.entries, LogEntry{TS: now, Ch: classifyLine(text), Text: text})
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
