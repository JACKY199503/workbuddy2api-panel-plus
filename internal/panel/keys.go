package panel

// keys.go 对外密钥分发管理（/panel/api/keys*）。
//
// 与 config.json 的 api_key 分工：api_key 是管理员总钥匙；这里管的是分发给
// 使用者的 wbk_ 子钥匙，各自带积分额度 / token 额度 / IP 名单 / 模型白名单 /
// 版本归属 / 有效期。存储落在 data/keys.json，由 internal/apikeys 维护。
//
// 字段与校验口径对齐 workbuddy-manager 的 key 分发（0 = 不限，IP 白名单写入前
// 校验 CIDR，名称不能只有空白）。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

func (p *Panel) writeKeysJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeKeyErr 把 apikeys.Err 原样透出（带中文原因，前端直接显示）。
func (p *Panel) writeKeyErr(w http.ResponseWriter, err error) {
	if ke, ok := err.(*apikeys.Err); ok {
		p.writeKeysJSON(w, ke.Status, map[string]any{"error": ke.Code, "message": ke.Msg})
		return
	}
	p.writeKeysJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": err.Error()})
}

// keysList GET /panel/api/keys
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_disabled")
		return
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"keys": p.cfg.Keys.List()})
}

// keysCreate POST /panel/api/keys —— 明文只在此响应里出现一次。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_disabled")
		return
	}
	var in apikeys.Key
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		p.writeKeysJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "请求体不是合法 JSON"})
		return
	}
	plain, k, err := p.cfg.Keys.Create(in)
	if err != nil {
		p.writeKeyErr(w, err)
		return
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"key": k, "plain": plain})
}

// keysPatch POST /panel/api/keys/{id}
func (p *Panel) keysPatch(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_disabled")
		return
	}
	id := r.PathValue("id")
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		p.writeKeysJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request", "message": "请求体不是合法 JSON"})
		return
	}
	k, err := p.cfg.Keys.Update(id, patch)
	if err != nil {
		p.writeKeyErr(w, err)
		return
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"key": k})
}

// keysDelete DELETE /panel/api/keys/{id}
func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_disabled")
		return
	}
	if !p.cfg.Keys.Delete(r.PathValue("id")) {
		p.writeKeysJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "密钥不存在"})
		return
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysReset POST /panel/api/keys/{id}/reset —— 清零用量（积分 / token / 请求数）。
func (p *Panel) keysReset(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "keys_disabled")
		return
	}
	if !p.cfg.Keys.ResetUsage(r.PathValue("id")) {
		p.writeKeysJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "密钥不存在"})
		return
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysCheckModels POST /panel/api/keys/check-models —— 白名单里**匹配不到已知
// 模型**的条目（多半是拼错了）。
//
// 为什么要由后端判：判据必须与调用侧同一份（去 `cn:` 前缀、保留 `global:`），
// 前端再实现一遍就是第二份事实来源，迟早漂移——漂移的表现正是「列表里有、
// 调用被拒」。
//
// 拿不到清单时不猜：返回 checked=false，前端如实显示「暂时无法校验」。
// 只读本机网关的 /v1/models（管理员 api_key，不会被子密钥白名单裁剪），
// 不发外部请求。
func (p *Panel) keysCheckModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Models []string `json:"models"`
		Realm  string   `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	names := make([]string, 0, len(in.Models))
	for _, x := range in.Models {
		if s := strings.TrimSpace(x); s != "" {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		p.writeKeysJSON(w, http.StatusOK, map[string]any{"checked": true, "unknown": []string{}})
		return
	}
	known, ok := p.gatewayModelIDs(r)
	if !ok || len(known) == 0 {
		p.writeKeysJSON(w, http.StatusOK, map[string]any{
			"checked": false, "unknown": []string{},
			"reason": "暂时读不到模型清单（去「模型」页刷新一次再回来）",
		})
		return
	}
	unknown := make([]string, 0)
	for _, name := range names {
		if !known[stripRealmPrefix(name)] {
			unknown = append(unknown, name)
		}
	}
	p.writeKeysJSON(w, http.StatusOK, map[string]any{"checked": true, "unknown": unknown})
}

// gatewayModelIDs 取本机网关 /v1/models 的裸名集合（含 cn/global 两域）。
func (p *Panel) gatewayModelIDs(r *http.Request) (map[string]bool, bool) {
	host := r.Host
	if host == "" {
		return nil, false
	}
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1"+portOf(host)+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	if key := p.cfg.Live.Load().APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	c := &http.Client{Timeout: 8 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false
	}
	known := make(map[string]bool, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			known[stripRealmPrefix(d.ID)] = true
		}
	}
	return known, true
}

// portOf 取 "host:port" 的 ":port" 部分；无端口时返回空（网关默认 80，此处取不到
// 就直接放弃校验，不猜端口）。
func portOf(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && i+1 < len(host) {
		return host[i:]
	}
	return ""
}

// stripRealmPrefix 去掉 `cn:` / `global:` 前缀（两者都去）。
func stripRealmPrefix(name string) string {
	low := strings.ToLower(name)
	for _, pref := range []string{"cn:", "global:"} {
		if len(low) > len(pref) && strings.HasPrefix(low, pref) {
			return name[len(pref):]
		}
	}
	return name
}

// keysEnabled 供前端决定是否显示「密钥」入口。
func (p *Panel) keysEnabled() bool { return p.cfg.Keys != nil }
