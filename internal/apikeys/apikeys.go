// Package apikeys 对外分发的 API 密钥（key 分发 / 积分限额 / IP 与模型管控）。
//
// 与网关 api_key 的关系：
//   - config.json 的 api_key 是**管理员总钥匙**，照旧全权放行、零回归；
//   - 本包的 key 是**分发给使用者的子钥匙**，前缀 wbk_，各自带积分额度、
//     token 额度、IP 名单、模型白名单、版本归属、有效期。
// 鉴权顺序：先判 wbk_ 前缀 → 命中走本包；否则回退原 api_key 校验。
//
// 口径与 workbuddy-manager 的 key 分发对齐：
//   - 停用/过期 → 403（凭据本身不可用）；
//   - 配额用尽（token / 积分）→ 429（避免被客户端读成「密钥无效」）；
//   - IP 不在白名单 / IP 超限 / 版本不匹配 / 模型不在白名单 → 400（这是本次
//     请求的参数不对，报文要能透出原因）。
//
// 落盘：data/keys.json，原子替换 + 每次变更立即写（key 数量少，无防抖必要）。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prefix 分发明文 key 的前缀（同时也是"是否归本包处理"的判据）。
const Prefix = "wbk_"

// Key 一把分发的密钥。
type Key struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Hash        string   `json:"hash"`         // sha256(明文)，不存明文
	Prefix      string   `json:"prefix"`       // 展示用：明文前 12 字符
	Enabled     bool     `json:"enabled"`
	ExpiresAt   string   `json:"expires_at"`   // RFC3339；空 = 不过期
	MaxIPs      int      `json:"max_ips"`      // 0 = 不限
	IPAllow     []string `json:"ip_allowlist"` // 空 = 不限制；支持精确 IP 与 CIDR
	Models      []string `json:"models"`       // 空 = 全部
	Realm       string   `json:"realm"`        // 空 = 不限；cn / global
	Quota       int64    `json:"quota"`        // token 额度；0 = 不限
	UsedTokens  int64    `json:"used_tokens"`
	QuotaCredit float64  `json:"quota_credit"` // 积分额度；0 = 不限
	UsedCredit  float64  `json:"used_credit"`
	CreatedAt   string   `json:"created_at"`
	LastUsedAt  string   `json:"last_used_at"`
	LastIP      string   `json:"last_ip"`
	ReqCount    int64    `json:"req_count"`
	IPs         []string `json:"ips"` // 历史使用过的 IP（MaxIPs 判定用）
	Seq         int64    `json:"seq"` // 创建序号：列表排序用（created_at 只到秒，同秒分不出先后）
}

// file 落盘结构。
type file struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

// Store 并发安全的密钥库。
type Store struct {
	mu    sync.Mutex
	path  string
	keys  []*Key
	index map[string]*Key // hash → key
	seq   int64           // 已发出的最大创建序号

	// TrustProxy 反代信任开关（见 Config.TrustProxy）。false（默认）时
	// ClientIP 只认 TCP 对端地址，不读可伪造的 X-Forwarded-For / X-Real-IP。
	// 进程内配置（main 注入），不落盘。
	TrustProxy bool
}

// New 创建密钥库；path 为空 = 纯内存（测试用）。
func New(path string) *Store {
	s := &Store{path: path, index: make(map[string]*Key)}
	if path != "" {
		if err := s.load(); err != nil {
			log.Printf("[apikeys] 读取 %s 失败（从空库开始）: %v", path, err)
		}
	}
	return s
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	for _, k := range f.Keys {
		if k == nil || k.Hash == "" {
			continue
		}
		s.keys = append(s.keys, k)
		s.index[k.Hash] = k
		if k.Seq > s.seq {
			s.seq = k.Seq
		}
	}
	s.sortLocked()
	return nil
}

// sortLocked 新的排前面（与 manager 的 id DESC 同一观感）。
//
// 先比 Seq：created_at 只精确到秒，同一秒内建的几把密钥字符串完全相同，
// 按它排序会退化成「插入顺序」（旧的在前）。
func (s *Store) sortLocked() {
	sort.SliceStable(s.keys, func(i, j int) bool {
		if s.keys[i].Seq != s.keys[j].Seq {
			return s.keys[i].Seq > s.keys[j].Seq
		}
		return s.keys[i].CreatedAt > s.keys[j].CreatedAt
	})
}

// flush 原子落盘（调用方持锁）。
func (s *Store) flush() {
	if s.path == "" {
		return
	}
	sorted := make([]*Key, len(s.keys))
	copy(sorted, s.keys)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq > sorted[j].Seq
		}
		return sorted[i].CreatedAt > sorted[j].CreatedAt
	})
	b, err := json.MarshalIndent(file{Version: 1, Keys: sorted}, "", "  ")
	if err != nil {
		log.Printf("[apikeys] 序列化失败: %v", err)
		return
	}
	dir := filepath.Dir(s.path)
	tmp := filepath.Join(dir, ".keys.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[apikeys] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[apikeys] 替换 %s 失败: %v", s.path, err)
	}
}

// ── 生成与校验 ────────────────────────────────────────────────────────

// Plain 生成一把新 key，返回明文。
func (s *Store) Plain() string {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return Prefix + hex.EncodeToString(buf[:])
}

func hashOf(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Err 业务错误（带 HTTP 状态码与错误码）。
type Err struct {
	Status int
	Code   string
	Msg    string
}

func (e *Err) Error() string { return e.Code + ": " + e.Msg }

func errf(status int, code, format string, a ...any) *Err {
	return &Err{Status: status, Code: code, Msg: fmt.Sprintf(format, a...)}
}

var errNoEntropy = &Err{Code: "entropy_failed", Msg: "随机数不可用"}

// Validate 创建/更新前的字段校验。
//
// 为什么在落库**之前**拦而不是存下去再说：存坏数据的后果是「该密钥永久不可用」
// （非法 CIDR 匹配不上任何 IP → 拒绝所有来源），而报错只说「不在白名单内」，
// 用户完全看不出是自己写错了。宁可在这里拒掉并说清怎么写。
func Validate(in Key, full bool) *Err {
	if full && strings.TrimSpace(in.Name) == "" {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_name", Msg: "密钥名称不能为空或只有空格"}
	}
	if full && len(strings.TrimSpace(in.Name)) > 64 {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_name", Msg: "密钥名称不能超过 64 个字符"}
	}
	if bad, ok := badCIDR(in.IPAllow); !ok {
		return &Err{
			Status: http.StatusBadRequest,
			Code:   "invalid_ip_allowlist",
			Msg: "IP 白名单里有无法识别的条目：" + bad + "。\n请写成单个 IP（1.2.3.4）或 CIDR（10.0.0.0/8）的形态。\n" +
				"留着它会让这把密钥拒绝所有来源（因为匹配不上任何 IP）。",
		}
	}
	if e := validateExpiresAt(in.ExpiresAt); e != nil {
		return e
	}
	// 负数额度拒绝而非静默归零：0 表「不限」，把 -5 悄悄变 0 是放大权限
	// （管理员想收紧结果变全放开）。宁可报错让用户填 0。
	if in.Quota < 0 {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_quota",
			Msg: "token 额度不能为负数（0 = 不限）"}
	}
	if in.MaxIPs < 0 {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_max_ips",
			Msg: "IP 数量上限不能为负数（0 = 不限）"}
	}
	if in.QuotaCredit < 0 || math.IsNaN(in.QuotaCredit) || math.IsInf(in.QuotaCredit, 0) {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_quota_credit",
			Msg: "积分额度不能为负数（0 = 不限）"}
	}
	return nil
}

// validateExpiresAt 有效期格式校验：非空时必须是合法 RFC3339。
//
// 由来（反向测试实锤）：Update/Create 此前不校验格式，写进 "not-a-date" 之类的
// 串后 Verify 侧 time.Parse 失败 → **静默不判过期**——非法有效期等价于永不过期，
// 且面板上看不出任何异常。宁可在这里拒掉。
func validateExpiresAt(s string) *Err {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return &Err{Status: http.StatusBadRequest, Code: "invalid_expires_at",
			Msg: "有效期不是合法的时间格式（RFC3339，如 2026-10-01T00:00:00+08:00）：" + s}
	}
	return nil
}

func badCIDR(items []string) (string, bool) {
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if net.ParseIP(s) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(s); err != nil {
			return s, false
		}
	}
	return "", true
}

// Create 建一把 key，返回明文（仅此一次可见）。
func (s *Store) Create(in Key) (plain string, k *Key, err error) {
	if e := Validate(in, true); e != nil {
		return "", nil, e
	}
	plain = s.Plain()
	if plain == "" {
		return "", nil, errNoEntropy
	}
	now := time.Now()
	k = &Key{
		ID:          newID(),
		Name:        trimOr(in.Name, "未命名密钥"),
		Hash:        hashOf(plain),
		Prefix:      plain[:12],
		Enabled:     true,
		ExpiresAt:   in.ExpiresAt,
		MaxIPs:      in.MaxIPs,
		IPAllow:     clean(in.IPAllow),
		Models:      clean(in.Models),
		Realm:       normRealm(in.Realm),
		Quota:       in.Quota,
		QuotaCredit: normCredit(in.QuotaCredit),
		CreatedAt:   now.Format(time.RFC3339),
	}
	if k.MaxIPs < 0 {
		k.MaxIPs = 0
	}
	if k.Quota < 0 {
		k.Quota = 0
	}
	s.mu.Lock()
	k.Seq = s.nextSeq()
	s.keys = append(s.keys, k)
	s.index[k.Hash] = k
	s.sortLocked()
	s.flush()
	s.mu.Unlock()
	return plain, k, nil
}

// Verify 校验一把分发的 key（不含 model / realm —— 那两项要等 body 解析出
// model 后由 VerifyRequest 判定）。
//
// 返回值：(key, err)。token 不是 wbk_ 前缀时返回 (nil, nil)——表示"不由本包处理"，
// 调用方应回退到原 api_key 校验。
func (s *Store) Verify(token, ip string) (*Key, error) {
	if !strings.HasPrefix(token, Prefix) {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.index[hashOf(token)]
	if k == nil {
		return nil, &Err{Status: http.StatusUnauthorized, Code: "invalid_api_key", Msg: "missing or invalid API key"}
	}
	if !k.Enabled {
		return nil, &Err{Status: http.StatusForbidden, Code: "key_disabled", Msg: "密钥已停用"}
	}
	if k.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, k.ExpiresAt); err == nil && time.Now().After(t) {
			return nil, &Err{Status: http.StatusForbidden, Code: "key_expired", Msg: "密钥已过期"}
		}
	}
	if k.Quota > 0 && k.UsedTokens >= k.Quota {
		return nil, errf(http.StatusTooManyRequests, "quota_exhausted",
			"密钥 token 配额已用尽（已用 %d / 上限 %d）", k.UsedTokens, k.Quota)
	}
	if k.QuotaCredit > 0 && k.UsedCredit >= k.QuotaCredit {
		return nil, errf(http.StatusTooManyRequests, "credit_quota_exhausted",
			"密钥积分额度已用尽（已用 %g / 上限 %g）", k.UsedCredit, k.QuotaCredit)
	}
	if len(k.IPAllow) > 0 && !ipIn(ip, k.IPAllow) {
		return nil, errf(http.StatusBadRequest, "ip_not_allowed", "来源 IP %s 不在密钥白名单内", ip)
	}
	if k.MaxIPs > 0 && ip != "" && !contains(k.IPs, ip) && len(k.IPs) >= k.MaxIPs {
		return nil, errf(http.StatusBadRequest, "too_many_ips",
			"密钥已绑定 %d 个 IP，超出上限 %d", len(k.IPs), k.MaxIPs)
	}
	return k, nil
}

// VerifyRequest 请求级校验：版本归属 + 模型白名单。
//
// 拆分出来的原因：model 要读完 body 才知道，而 withAuth 发生在读 body 之前。
// 口径与 manager 的 keysvc.validate 一致（400 + 具体原因），避免客户端把
// 「配置不对」显示成「密钥无效」。
func VerifyRequest(k *Key, model, realm string) *Err {
	if k == nil {
		return nil
	}
	want := normRealm(k.Realm)
	if want != "" {
		if strings.TrimSpace(model) == "" {
			label := "国内版"
			if want == "global" {
				label = "国际版"
			}
			return errf(http.StatusBadRequest, "realm_mismatch",
				"该密钥限定了%s模型，请求必须指定 model", label)
		}
		if realm != want {
			label := map[string]string{"cn": "国内版", "global": "国际版"}[want]
			other := "国际版"
			if want == "global" {
				other = "国内版"
			}
			hint := "模型名需带 global: 前缀"
			if want == "cn" {
				hint = "请去掉 global: 前缀"
			}
			return errf(http.StatusBadRequest, "realm_mismatch",
				"该密钥仅限%s模型，当前请求是%s模型（%s）", label, other, hint)
		}
	}
	if len(k.Models) > 0 {
		if strings.TrimSpace(model) == "" {
			return errf(http.StatusBadRequest, "model_not_allowed",
				"请求未指定 model，而该密钥启用了模型白名单")
		}
		if !modelAllowed(model, k.Models) {
			return errf(http.StatusBadRequest, "model_not_allowed",
				"模型 %s 不在密钥白名单内", model)
		}
	}
	return nil
}

// Touch 记录一次成功鉴权（IP 归属、请求数、最近使用）。
func (s *Store) Touch(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return
	}
	k.ReqCount++
	if ip != "" {
		if !contains(k.IPs, ip) {
			k.IPs = append(k.IPs, ip)
		}
		k.LastIP = ip
	}
	k.LastUsedAt = time.Now().Format(time.RFC3339)
	s.flush()
}

// Consume 累加已消耗积分与 token。
func (s *Store) Consume(id string, credit float64, tokens int64) {
	if credit <= 0 && tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return
	}
	if credit > 0 {
		k.UsedCredit += credit
	}
	if tokens > 0 {
		k.UsedTokens += tokens
	}
	s.flush()
}

// nextSeq 取下一个创建序号（调用方持锁）。
func (s *Store) nextSeq() int64 {
	s.seq++
	return s.seq
}

func (s *Store) byIDLocked(id string) *Key {
	for _, k := range s.keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

// ── 管理面 ──────────────────────────────────────────────────────────

// List 返回快照副本。
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, 0, len(s.keys))
	sorted := make([]*Key, len(s.keys))
	copy(sorted, s.keys)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq > sorted[j].Seq
		}
		return sorted[i].CreatedAt > sorted[j].CreatedAt
	})
	for _, k := range sorted {
		out = append(out, *k)
	}
	return out
}

// Get 按 ID 取副本。
func (s *Store) Get(id string) (*Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return nil, false
	}
	cp := *k
	return &cp, true
}

// Update 局部更新：只覆盖本次**显式提交**的字段（PATCH 语义）。
//
// 显式传 null 的 expires_at 表示「清空有效期（永不过期）」——与「本次没带该字段」
// 区分开，否则编辑页的「永不过期」选项无法生效。
func (s *Store) Update(id string, patch map[string]any) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return nil, &Err{Status: http.StatusNotFound, Code: "not_found", Msg: "密钥不存在"}
	}
	probe := Key{Name: k.Name}
	if v, ok := patch["name"].(string); ok {
		probe.Name = v
	}
	if v, ok := patch["ip_allowlist"].([]any); ok {
		probe.IPAllow = clean(toStringSlice(v))
		if bad, good := badCIDR(probe.IPAllow); !good {
			return nil, &Err{
				Status: http.StatusBadRequest,
				Code:   "invalid_ip_allowlist",
				Msg: "IP 白名单里有无法识别的条目：" + bad + "。\n请写成单个 IP（1.2.3.4）或 CIDR（10.0.0.0/8）的形态。\n" +
					"留着它会让这把密钥拒绝所有来源（因为匹配不上任何 IP）。",
			}
		}
	}
	if e := Validate(probe, true); e != nil {
		return nil, e
	}
	if v, ok := patch["name"].(string); ok && strings.TrimSpace(v) != "" {
		k.Name = strings.TrimSpace(v)
	}
	if v, ok := patch["enabled"].(bool); ok {
		k.Enabled = v
	}
	if v, ok := patch["expires_at"]; ok {
		if v == nil {
			k.ExpiresAt = ""
		} else if str, ok2 := v.(string); ok2 {
			// 更新侧同样校验格式（与 Create 同一判据）：非法串会导致 Verify 侧
			// time.Parse 失败 → 静默不判过期（= 永不过期）。空串等价 null（清空）。
			if e := validateExpiresAt(str); e != nil {
				return nil, e
			}
			k.ExpiresAt = strings.TrimSpace(str)
		}
	}
	if v, ok := patch["quota_credit"].(float64); ok {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, &Err{Status: http.StatusBadRequest, Code: "invalid_quota_credit",
				Msg: "积分额度不能为负数（0 = 不限）"}
		}
		k.QuotaCredit = normCredit(v)
	}
	if v, ok := patch["quota"].(float64); ok {
		if v < 0 {
			return nil, &Err{Status: http.StatusBadRequest, Code: "invalid_quota",
				Msg: "token 额度不能为负数（0 = 不限）"}
		}
		k.Quota = int64(v)
		if k.Quota < 0 {
			k.Quota = 0
		}
	}
	if v, ok := patch["realm"].(string); ok {
		k.Realm = normRealm(v)
	}
	if v, ok := patch["max_ips"].(float64); ok {
		if v < 0 {
			return nil, &Err{Status: http.StatusBadRequest, Code: "invalid_max_ips",
				Msg: "IP 数量上限不能为负数（0 = 不限）"}
		}
		k.MaxIPs = int(v)
		if k.MaxIPs < 0 {
			k.MaxIPs = 0
		}
	}
	if v, ok := patch["ip_allowlist"].([]any); ok {
		k.IPAllow = clean(toStringSlice(v))
	}
	if v, ok := patch["models"].([]any); ok {
		k.Models = clean(toStringSlice(v))
	}
	s.flush()
	cp := *k
	return &cp, nil
}

// Delete 删除一把 key。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == id {
			delete(s.index, k.Hash)
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			s.flush()
			return true
		}
	}
	return false
}

// ResetUsage 清零用量（积分 / token / 请求数）。
//
// 不动 IPs：那是 IP 上限的记账依据，清掉会让 max_ips 限制失效（manager 同口径）。
func (s *Store) ResetUsage(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byIDLocked(id)
	if k == nil {
		return false
	}
	k.UsedCredit = 0
	k.UsedTokens = 0
	k.ReqCount = 0
	s.flush()
	return true
}

// ── 工具 ────────────────────────────────────────────────────────────

// ClientIP 取客户端 IP：TrustProxy=true 时优先 X-Forwarded-For / X-Real-IP
// （反代场景，代理会覆写这些头）；默认 false 只认 TCP 对端地址（RemoteAddr）。
//
// 由来（反向测试实锤的安全问题）：XFF / X-Real-IP 是请求方可随意伪造的头。
// 直连部署（无反代）下无条件信任它，等于任何人带一个
// `X-Forwarded-For: <白名单IP>` 就能绕过 ip_allowlist，并可伪造任意"新 IP"
// 干扰 max_ips 记账。故默认不信，只有显式声明挂在可信反代后才读。
func (s *Store) ClientIP(r *http.Request) string {
	if s != nil && s.TrustProxy {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if i := strings.Index(v, ","); i > 0 {
				return strings.TrimSpace(v[:i])
			}
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ModelAllowed 模型白名单判定（供 /v1/models 裁剪与请求校验共用同一份判据）。
func ModelAllowed(k *Key, model string) bool {
	if k == nil || len(k.Models) == 0 {
		return true
	}
	if strings.TrimSpace(model) == "" {
		return false
	}
	return modelAllowed(model, k.Models)
}

// RealmAllowed 版本归属判定（供 /v1/models 裁剪）；空 = 不限制。
func RealmAllowed(k *Key, realm string) bool {
	if k == nil {
		return true
	}
	want := normRealm(k.Realm)
	return want == "" || want == realm
}

// bareModel 模型名归一化：去 `cn:` 前缀（**保留 `global:`**）。
//
// `cn:` 只是上游的路由约定而不是模型名的一部分（面板显示的也是裸名），
// 而 `global:` 决定路由到哪个账号池——两个版本的同名模型不是一回事。
// 归一化后「白名单写 cn:xxx」与「写 xxx」等价，避免升级后原本能用的密钥
// 突然报「模型不在白名单内」。
func bareModel(model string) string {
	m := strings.TrimSpace(model)
	if len(m) > 3 && strings.EqualFold(m[:3], "cn:") {
		return m[3:]
	}
	return m
}

func modelAllowed(model string, allow []string) bool {
	bare := bareModel(model)
	for _, m := range allow {
		if m == model || bareModel(m) == bare {
			return true
		}
		// 支持 "cn:" 这类前缀通配：白名单写 "cn:" 命中所有 cn: 模型
		if strings.HasSuffix(m, ":") && strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}

func normRealm(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "cn" || s == "global" {
		return s
	}
	return ""
}

// normCredit 积分额度归一化：脏数据一律吃掉成 0（= 不限），不抛错。
//
// 钳到 0 是安全的：0 表示「不限」，不会把设了负数的密钥悄悄放行成「已超限」
// （那会让线上调用突然全 429）。
func normCredit(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func ipIn(ip string, allow []string) bool {
	parsed := net.ParseIP(ip)
	for _, a := range allow {
		if a == ip {
			return true
		}
		if strings.Contains(a, "/") {
			if _, n, err := net.ParseCIDR(a); err == nil && parsed != nil && n.Contains(parsed) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func toStringSlice(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102150405.000000")
	}
	return hex.EncodeToString(b[:])
}
