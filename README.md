<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel Plus</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  本仓库在上游之上<b>只新增三项功能</b>：🔑 API 密钥分发 · 🧠 模型编排 · 🔌 协议兼容层
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

## 这是什么

三层 fork，**上游的全部能力一个没动**，只在其上加了密钥分发、模型编排与协议兼容层：

| 层 | 项目 | 说明 |
|---|---|---|
| 根项目 | [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | 账号池调度、错误分类、提示词体系等核心设计 |
| 直接上游 | [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) | Web 面板与可视化运维层 |
| **本仓库** | `workbuddy2api-panel-plus` | 上游 + 三项增强 |

账号池调度、冷却熔断、定时任务、成长任务、Web 面板等**上游内容本文档不再复述**，直接看上游 README：

- 全部能力与配置细节 → [上游 README](https://github.com/linguo2625469/workbuddy2api-panel#readme)
- 根项目设计 → [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)

本仓库提交的源码 = 上游功能 + 三项增强，已合并好，`clone` 下来直接就是完整网关 + 面板，不需要先装上游、也不需要先打补丁。

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

---

<a id="model-orchestration"></a>

## 🆕 新增二：模型编排（auto · 免费额度薅满 · 昼夜自动切换）

**一句话目的**：把每天能白嫖的额度尽量用满；免费窗口过期、额度波动、被限流时**自动**换模型，全程不用手动改配置。

**怎么触发**：请求里 `model` 传下面这些名字，网关才接管编排（`auto_model.enabled` 为 `true` 时生效）：

| 你传的 model | 结果 |
|---|---|
| `auto` | 走编排。白天用 `day_primary`、夜里用 `night_primary`，降级链不挑版本 |
| `cn:auto` | 走编排，但候选链只留 `cn:` 开头的模型 |
| `global:auto` | **默认配置下不走编排** —— 主模型和降级链里全是 `cn:` 模型，没有 global 候选，这个名字会原样发给上游（基本是 404）。想让 global 也编排，先往 `fallback` 里加 global 模型 |
| `cn:hy3`、`cn:deepseek-v4.1-flash` 这类具体名字 | 就是那个模型，不走编排，原样透传 |

三个端点都认这几种写法。`/v1/models` 里能查到的虚拟名是 `auto` 和 `cn:auto` 两个。

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

### 具体模型名也能挂降级链

传具体模型名（比如 `cn:deepseek-v4.1-flash`）就是它自己，跟 `auto` 没关系，不受编排影响。
但可以在 `model_fallback` 里给它配一条链 —— 它挂了就往下顶：

```json
"model_fallback": {
  "cn:hy3":                 ["cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:deepseek-v4.1-flash": ["cn:glm-5.3-flash", "cn:hy3-x"]
}
```

键要跟请求里传的模型名一字不差。两条链叠加展开，自动去重，最多 8 个、递归 3 层。
没写进这张表的模型名，就是原样透传，没有降级。

---

<a id="protocol-compat"></a>

## 🆕 新增三：协议兼容层（`/v1/responses` + `/v1/messages`）

**解决什么问题**：原来网关只认 OpenAI 的 `/v1/chat/completions`。Claude Code 等客户端走的是 Anthropic Messages API（`/v1/messages`），新版 OpenAI SDK / Codex 走的是 Responses API（`/v1/responses`），接上来直接 404。现在两类协议都能直接打进来，复用同一套账号池、密钥分发与模型编排。

| 端点 | 协议 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | 原有接口，行为零变更 |
| `POST /v1/responses` | **OpenAI Responses API** | `input` 支持字符串或消息数组；`instructions` → system；`max_output_tokens` → `max_tokens`；返回 `{object:"response", output:[...], status, usage}` |
| `POST /v1/messages` | **Anthropic Messages API** | 标准 `messages` 数组 + `system`（字符串或 block 数组）+ `max_tokens`；返回 `{type:"message", content:[{type:"text"}], stop_reason, usage}` |

**实现方式（零重复调度）**：入口把请求翻译成内部 chat 请求体内的等价形式，然后**原样走内部 `/v1/chat/completions` 全链路**（选号 / 轮换 / 冷却 / 编排降级 / 密钥鉴权 / 计费），再把响应（非流式整体转写、流式逐帧转 SSE）翻回目标协议。调度逻辑只有一份，两条新链路不重复实现任何选号代码。

- 流式：`/v1/messages` 输出 `message_start → content_block_start → ping → content_block_delta* → content_block_stop → message_delta → message_stop`；`/v1/responses` 输出 `response.created → response.output_item.added → response.content_part.added → response.output_text.delta* → response.output_text.done → response.output_item.done → response.completed`。
- 鉴权与错误：鉴权沿用原 `api_key` 与 `wbk_` 子钥匙；错误按**入口协议**返回（`/v1/messages` 返回 Anthropic 形状 `{"type":"error","error":{...}}`，另两个返回 OpenAI 形状）。
- 计费：与 chat 完全一致，走同一份用量统计与额度扣减。
- 模型编排：传 `auto` / `cn:auto` 在三个端点上都生效，降级链与昼夜切换同样适用；实际命中的模型仍通过响应头 `X-WB2A-Routed-Model` 回传。
- 不支持的字段（如 Responses 的 `tools` / `previous_response_id`、Messages 的 `thinking`）**静默忽略**，不报错。

**怎么用**

```bash
# OpenAI Responses API
curl http://HOST:7863/v1/responses \
  -H "Authorization: Bearer <你的key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","input":"用一句话介绍自己"}'

# Anthropic Messages API
curl http://HOST:7863/v1/messages \
  -H "Authorization: Bearer <你的key>" \
  -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"auto","max_tokens":1024,"messages":[{"role":"user","content":"用一句话介绍自己"}]}'
```

Claude Code 直接把 `ANTHROPIC_BASE_URL` 指到网关即可（`http://HOST:7863`，key 用 `wbk_` 子钥匙或管理员 key）。

**新增文件**：`internal/server/compat.go`（转写框架 / 录制器 / SSE 发射）、`internal/server/compat_responses.go`、`internal/server/compat_messages.go`；路由注册在 `internal/server/handler.go`。

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

## License

[MIT](LICENSE)。再分发请保留原仓库 MIT 声明，注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`。本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利。

## 🙏 致谢

- 根项目：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)
- 直接上游：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
- 本仓库二改（2026-09）：**API 密钥分发**、**模型编排** 与 **协议兼容层**，新增代码：

| 新增文件 | 作用 |
|---|---|
| `internal/apikeys/apikeys.go` | 密钥库：签发 / 校验 / 额度 / 白名单 / 落盘 |
| `internal/autoroute/autoroute.go` | 编排引擎：昼夜主模型 + 降级链展开 |
| `internal/panel/keys.go` | 面板密钥管理接口 `/panel/api/keys` |
| `internal/server/compat.go` | 协议兼容层：转写框架、响应录制器、SSE 事件发射、按协议分发错误 |
| `internal/server/compat_responses.go` | OpenAI Responses API 双向转写 |
| `internal/server/compat_messages.go` | Anthropic Messages API 双向转写 |

改动的上游文件：`cmd/server/config.go`、`cmd/server/main.go`、`internal/server/handler.go`（新增两条路由）、`internal/server/resolve_model.go`、`internal/livecfg/livecfg.go`、`internal/upstream/client.go`、`internal/upstream/hint.go`、`internal/panel/panel.go`、`internal/panel/app.js`、`internal/panel/index.html`。
