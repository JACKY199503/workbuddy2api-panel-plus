<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel Plus</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  本仓库在上游之上<b>只新增两项功能</b>：🔑 API 密钥分发 · 🧠 模型编排
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

## 这是什么

三层 fork，**上游的全部能力一个没动**，只在其上加了密钥分发与模型编排：

| 层 | 项目 | 说明 |
|---|---|---|
| 根项目 | [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | 账号池调度、错误分类、提示词体系等核心设计 |
| 直接上游 | [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) | Web 面板与可视化运维层 |
| **本仓库** | `workbuddy2api-panel-plus` | 上游 + 两项增强 |

账号池调度、冷却熔断、定时任务、成长任务、Web 面板等**上游内容本文档不再复述**，直接看上游 README：

- 全部能力与配置细节 → [上游 README](https://github.com/linguo2625469/workbuddy2api-panel#readme)
- 根项目设计 → [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)

本仓库提交的源码 = 上游功能 + 两项增强，已合并好，`clone` 下来直接就是完整网关 + 面板，不需要先装上游、也不需要先打补丁。

---

## 🚀 快速开始（预构建镜像，开箱即用）

```bash
# 1. 取文件
git clone https://github.com/JACKY199503/workbuddy2api-panel-plus.git && cd workbuddy2api-panel-plus

# 2. 建挂载目录 + 初始配置（缺 config/config.json 容器起不来）
mkdir -p config auths data
cp config.example.json config/config.json

# 3. 属主对齐：容器以 uid 10001 运行，属主不对会 permission denied
sudo chown -R 10001:10001 config auths data

# 4. 起
docker compose up -d

# 5. 浏览器打开 http://<你的机器IP>:7863/ → 面板「添加账号」走 OAuth 登录
```

- 镜像：`ghcr.io/jacky199503/workbuddy2api-panel-plus:latest`（linux/amd64，push 到 main 自动构建）
- 自己编译：注释掉 `docker-compose.yml` 里的 `image:`、放开 `build: .`，或 `docker build -t wb2api .`
- 升级：`docker compose pull && docker compose up -d`（`config/`、`auths/`、`data/` 是挂载目录，不会丢）
- 健康检查：`curl -s http://localhost:7863/healthz`

---

## 🆕 新增一：API 密钥分发（`wbk_` 子钥匙）

**解决什么问题**：原来全站只有 `config.json` 里的一把管理员 `api_key`，要么不给人用，要么给人用就等于交出全部权限。现在可以按需签发子钥匙，每把自带额度与边界，随时停用、随时改额度。

| 项目 | 说明 |
|---|---|
| 入口 | 面板 → **API 密钥** 页（增删改查 / 停用 / 看用量）；接口 `GET/POST/PATCH/DELETE /panel/api/keys`，用管理员 key 鉴权 |
| 明文 | 前缀 `wbk_`，**只在创建时展示一次**；库里只存 `sha256(明文)`，落盘 `data/keys.json`（原子写，权限 600） |
| 鉴权顺序 | `wbk_` 前缀 → 走密钥库判定；否则回退原 `api_key` 校验（**不开密钥库时零回归**） |

**一把钥匙能限制什么**

| 字段 | 含义 |
|---|---|
| `name` | 名称（必填，≤64 字符） |
| `realm` | 版本归属：`cn` / `global`，空 = 不限 |
| `ip_allowlist` | IP 白名单，支持精确 IP 与 CIDR，空 = 不限制 |
| `max_ips` | 最多允许几个不同 IP 用过，0 = 不限 |
| `models` | 模型白名单（含虚拟模型 `auto`），空 = 全部 |
| `quota` / `quota_credit` | token 额度 / 积分额度，0 = 不限 |
| `expires_at` | 过期时间（RFC3339），空 = 不过期 |
| `enabled` | 停用开关 |

**怎么用**

```bash
curl http://HOST:7863/v1/chat/completions \
  -H "Authorization: Bearer wbk_xxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}'
```

**错误口径**（刻意区分，方便客户端判断要不要重试）

| 场景 | 状态码 |
|---|---|
| 停用 / 过期 | 403 |
| 额度用尽（token 或积分） | 429 |
| IP 不在白名单 / IP 超限 / 版本不匹配 / 模型不在白名单 | 400 |

**安全默认**：`trust_proxy` 默认 `false` —— 只认 TCP 对端地址，**不读可伪造的 `X-Forwarded-For` / `X-Real-IP`**，避免伪造头绕过 IP 白名单。确实挂在反向代理后面、需要真实客户端 IP 时，才在 `config.json` 里设 `"trust_proxy": true`。

---

<a id="model-orchestration"></a>

## 🆕 新增二：模型编排（auto · 免费额度薅满 · 昼夜自动切换）

**一句话目的**：把每天能白嫖的额度尽量用满；免费窗口过期、额度波动、被限流时**自动**换模型，全程不用手动改配置。

做法只有一个：客户端写 `model=auto`，剩下的全交给网关。具名模型（`cn:hy3` 之类）不受任何影响，照旧原样透传。

- **按钟点自动换主模型**：白天吃白天的免费额度，夜里吃夜里的免费窗口，到点自动切回。
- **出问题自动沿链降级**：主模型被限流 / 积分耗尽 / 上游报错 / 返回空正文，就沿降级链往下挑下一个能用的。
- **优惠到期不用管**：链是按「免费 → 低倍率 → 兜底」排的，免费额度没了自然落到下一个，配置一行不用动。

### 自动切换逻辑

| 时段（Asia/Shanghai） | 主模型 | 完整候选链（链首首选，失败才往下走） |
|---|---|---|
| **白天 08:00–23:00** | `cn:hy3`（免费） | `cn:hy3` → `cn:deepseek-v4.1-flash`（0.03x）→ `cn:glm-5.3-flash`（0.06x，1M 上下文、能看图）→ `cn:hy3-x`（0.05x，兜底） |
| **夜间 23:00–08:00** | `cn:hy4-preview`（夜间老用户免费窗口） | `cn:hy4-preview` → `cn:hy3` → `cn:deepseek-v4.1-flash` → `cn:glm-5.3-flash` → `cn:hy3-x` |

- 切换**不是定时任务**：每次请求进来按当前小时判定，08:00 一到请求自然回到 `cn:hy3`，无需重启、无需改配置。
- 白天主模型 `cn:hy3` 本身也在降级链里 —— 夜里 `cn:hy4-preview` 挂掉时接上的就是它；白天它已是链首，链里重复出现的那一项会被**自动去重跳过**。所以一条 `fallback` 同时服务昼夜两个时段。
- 链尾 `cn:hy3-x` 是兜底位：只要账号还有额度就一定有响应，不会把请求打空。

### 配置（`config.json` 的 `auto_model` 段；面板「配置」页可直接改，保存热生效）

```json
"auto_model": {
  "enabled": true,
  "day_primary":   "cn:hy3",
  "night_primary": "cn:hy4-preview",
  "day_start": 8,
  "day_end": 23,
  "fallback": ["cn:hy3", "cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "on_empty": true,
  "virtual_id": "auto",
  "override": true
}
```

| 字段 | 含义 |
|---|---|
| `enabled` | 编排开关。**代码缺省 false**（不写这段 = `auto` 当普通模型名直出，零回归）；本仓库 `config.example.json` 给的是 `true`（开箱即薅额度） |
| `day_primary` / `night_primary` | 白天 / 夜间主模型（带 realm 前缀） |
| `day_start` / `day_end` | 白天窗口 `[start, end)`，按 **Asia/Shanghai 小时**判定，默认 8 / 23 |
| `fallback` | 有序降级链（带 realm 前缀），昼夜共用；与主模型重复项自动跳过 |
| `virtual_id` / `override` | 虚拟模型名（默认 `auto`）；与上游真实模型同名时是否强行接管（上游 cn 侧下发过同名 `auto`，想接管就开 true 或改名） |
| `on_empty` | 上游 200 但正文为空也算失败并降级（部分模型 `reasoning_effort=max` 吃满预算会返回空），默认 true |
| `fallback_on` | 可触发降级的错误类别，空 = 内置默认集合 |

### 什么情况会降级

429 软限流 · 402 额度耗尽 · 上游 `11102` · 5xx · 无健康账号 · 上游 200 空正文（`on_empty`）。

反例（**不降级**）：内容拦截、参数错误、上下文超长、请求体解析失败 —— 这些是请求本身的问题，换任何模型都一样撞墙，直接 fail-fast。

### 具名模型也能挂同一条链

`model_fallback` 的键是客户端写的模型名（逐字匹配），这样即使客户端写死 `cn:hy3`，也能享受和 `auto` 一样的降级：

```json
"model_fallback": {
  "cn:hy3":                 ["cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:hy4-preview":         ["cn:hy3", "cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:deepseek-v4.1-flash": ["cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:glm-5.3-flash":       ["cn:hy3-x"]
}
```

两条链可叠加：链上每一项再按本表递归展开（限深 3、链长 ≤ 8、去重）。

### 怎么知道实际用了哪个模型

响应头 `X-WB2A-Routed-Model`：

```bash
curl -i http://HOST:7863/v1/chat/completions \
  -H "Authorization: Bearer <你的key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":false}'
# 白天：X-WB2A-Routed-Model: cn:hy3
# 夜里：X-WB2A-Routed-Model: cn:hy4-preview
```

**一处语义修正**：上游业务码 `14018`（`Credits exhausted`，账号积分耗尽）原本被归进 `rate_limit_exceeded` 当限流处理 —— 重试不可能成功。现已单列为 `402 upstream_credits_exhausted`，提示直接写明「需充值 / 等额度重置」。

---

## 上游已有能力（不在本文档展开）

以下能力全部来自上游，未做删改，用法见对应文档：

| 能力 | 去哪看 |
|---|---|
| 账号池调度、冷却熔断、选号策略、会话粘性 | [上游 README · 核心行为语义](https://github.com/linguo2625469/workbuddy2api-panel#readme) |
| 定时任务（签到 / 活跃 / 旅行 / 保活 / 夜猫子） | [上游 README · 定时任务](https://github.com/linguo2625469/workbuddy2api-panel#readme) |
| 成长任务一键完成、连登管家、开学季活动 | [上游 README · 成长任务](https://github.com/linguo2625469/workbuddy2api-panel#readme) |
| Web 管理面板（账号池 / 模型档位 / 在线改配置 / 日志） | [上游 README · Web 管理面板](https://github.com/linguo2625469/workbuddy2api-panel#readme) |
| 完整配置项速查、环境变量覆盖、API 端点、错误分类 | [上游 README · 配置说明](https://github.com/linguo2625469/workbuddy2api-panel#readme) |

增强注入点自检（合并上游后确认两项增强没被冲掉）：`bash tools/check-enhancements.sh`。

---

## 安全与合规

- **凭据**：`auths/` 存明文 token（0600），**切勿提交 git**（`.gitignore` 已排除 `auths/`、`data/`、`config.json`）
- **公网部署**：服务只提供明文 HTTP，**必须置于 HTTPS 反向代理之后**并设置 `api_key`
- **IP 白名单**：`trust_proxy` 默认 `false`，不读可伪造的 `X-Forwarded-For`
- **合规**：非官方网关，仅限**本人授权账号**、本机 / 私有环境测试；遵守 CodeBuddy 服务条款，作者不对账号封禁或条款违约负责

---

## License

[MIT](LICENSE)。再分发请保留原仓库 MIT 声明，注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`。本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利。

## 🙏 致谢

- 根项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)
- 直接上游：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
- 本仓库二改（2026-09）：**API 密钥分发** 与 **模型编排**，新增代码：

| 新增文件 | 作用 |
|---|---|
| `internal/apikeys/apikeys.go` | 密钥库：签发 / 校验 / 额度 / 白名单 / 落盘 |
| `internal/autoroute/autoroute.go` | 编排引擎：昼夜主模型 + 降级链展开 |
| `internal/panel/keys.go` | 面板密钥管理接口 `/panel/api/keys` |

改动的上游文件：`cmd/server/config.go`、`cmd/server/main.go`、`internal/server/handler.go`、`internal/server/resolve_model.go`、`internal/livecfg/livecfg.go`、`internal/upstream/client.go`、`internal/upstream/hint.go`、`internal/panel/panel.go`、`internal/panel/app.js`、`internal/panel/index.html`。
