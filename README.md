<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel Plus</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  在上游之上<b>只新增三项功能</b>：🔑 API 密钥分发 · 🧠 模型编排 · 🔌 多协议接入
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

## 这是什么

上游是 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) —— CodeBuddy 多账号网关 + Web 面板。本仓库**上游功能一个没动**，只在其上加了三项：API 密钥分发、模型编排、多协议接入。

账号池调度、冷却熔断、定时任务、成长任务、面板运维这些上游能力，本文档不再复述，直接看 [上游 README](https://github.com/linguo2625469/workbuddy2api-panel#readme)。

clone 下来就是完整网关 + 面板，不用先装上游、也不用打补丁。

## 使用声明（必读）

近期我们发现有人将本项目用于以下行为：

- 批量注册小号 / 收购账号，对外提供付费 API、共享池、代充等业务；
- 二次加壳、捆绑卡密（授权码）售卖，或以「公益服」「低价中转」等名义变相收费分发。

我们对上述行为表达最强烈的反对，并声明如下：

- **一切商用 / 售卖行为与本项目及作者无关。** 本项目不授权、不支持、不参与任何面向公众的 API 售卖、账号池出租、卡密收费分发；行为人由此产生的一切后果（包括但不限于账号封禁、条款违约与法律风险）由其自行承担，与作者和贡献者无任何关系。
- **密钥分发功能仅限自用。** 面板里的「API 密钥」是给自己多个项目、多台机器分别配钥匙、分别限额度用的，不是拿来给别人发钥匙的。拿它对外分发、转售、共享，本项目不支持，也不提供任何与之相关的能力；由此产生的额度消耗、接口限流、账号封禁等后果一律自负。
- **批量注册与转售接口配额违反目标平台服务条款。** CodeBuddy / 腾讯系服务条款禁止批量注册账号及商业转售接口。上游仓库已删除、停止公开维护——我们无法断定具体原因，但此类滥用行为正在毁掉所有正常使用者的环境，请勿再消耗社区的善意。
- **请勿购买任何「收费版」「卡密版」「公益中转版」。** 本项目永远免费开源。任何加壳、加密、捆绑收费的「版本」都是他人篡改的产物，与本项目无关；且此类分发无法审计，存在被植入后门、回传并窃取你 CodeBuddy 凭证的风险（`auths/` 中保存的是明文 accessToken / refreshToken）。你付钱买到的不是本项目，而是把自己账号交给陌生人的机会。
- **关于开源协议的诚实说明。** 本项目基于 MIT 协议开源，协议允许自由使用与修改源码——这是开源的本意，我们不会收回；但 MIT 赋予的是代码层面的自由，不赋予以本项目名义宣传、售卖、捆绑分发，或要求作者提供支持与背书的权利。作者不为任何第三方分发版本提供支持、更新承诺或安全保证。
- **作者保留止损的权利。** 若滥用行为持续，作者可能随时停止维护、关闭或删除仓库，且不另行通知。上游的今天可能就是本项目的明天，望自重。

如果你的用途是管理自己的账号，欢迎正常使用、反馈问题与提交 PR。

---

## 🚀 安装（Docker Compose）

```bash
# 1. 克隆
git clone https://github.com/JACKY199503/workbuddy2api-panel-plus.git
cd workbuddy2api-panel-plus

# 2. 准备配置（缺这个文件容器起不来）
mkdir -p config auths data && cp config.example.json config/config.json

# 3. 容器以 uid 10001 运行，挂载目录属主不对会 permission denied
sudo chown -R 10001:10001 config auths data

# 4. 启动
docker compose up -d
```

打开 `http://<你的机器IP>:7863/panel/`，用面板「添加账号」走 OAuth 登录。

常用命令：

```bash
docker compose logs -f       # 跟踪日志
docker compose restart       # 重启
docker compose down          # 停止并移除容器（数据在 ./auths 与 ./data，不受影响）
curl -s http://localhost:7863/healthz   # 健康检查，无可用账号时返回 503
```

---

## 🆕 新增一：API 密钥分发（子钥匙）

**解决什么**：原来全站只有一把管理员钥匙，要么不给人用，要么给人就等于把全部权限交出去。现在可以按用途签发子钥匙，每把自带额度和边界，随时停用、随时改额度。

**在哪操作**：面板左侧 **API 密钥** 页 →「新建密钥」。列表里每行都能直接停用、改额度、看用量。

新建窗口里能限制的项（填 0 或留空 = 不限制）：

| 项 | 作用 |
|---|---|
| 密钥名称 | 区分用途，必填 |
| 版本归属 | 限定只能走国内版或国际版，默认不限 |
| 有效期（天） | 到期自动失效，0 = 长期有效 |
| 最大来源 IP 数 | 最多允许几个不同 IP 用过这把钥匙 |
| Token 用量上限 | 用超了就拒绝 |
| 积分用量上限 | 用超了就拒绝 |
| 来源 IP 白名单 | 只允许名单内 IP 调用，支持网段写法 |
| 模型白名单 | 只允许调用名单内模型（含 `auto`），留空 = 全部放行 |

钥匙明文以 `wbk_` 开头，**只在创建弹窗里显示这一次**，关掉就找不回来了 —— 服务端只存摘要、不存明文。

一把子钥匙都不建的话，跟以前完全一样。

---

<a id="model-orchestration"></a>

## 🆕 新增二：模型编排（按时间自动换模型）

请求里模型名传 `auto`，网关按当前时间自动挑模型；首选被限流、积分耗尽或报错，就顺着降级链往下换。目的就一个：把每天能免费用的额度吃满，出问题不用手动改配置。

**先开启**：面板「配置」页 →「模型编排」→ 打开「启用模型编排」→ 保存配置。

请求里模型名怎么传：

| 你传的模型名 | 结果 |
|---|---|
| `auto` | 按当前时间选白天主模型或夜间主模型 |
| `cn:auto` | 同上，但只在国内版模型里挑 |
| `global:auto` | 默认不生效 —— 默认配置里没有国际版候选。想让国际版也编排，先往降级链里加国际版模型 |
| `cn:hy3`、`cn:deepseek-v4.1-flash` 这类具体名字 | 就是那个模型，原样透传，不参与编排 |

### 切换逻辑

按北京时间，每次请求进来时按当前钟点判定 —— **不是定时任务**，到点自然切换，不用重启、不用改配置。

| 时段 | 首选模型 | 首选不行就依次往下试 |
|---|---|---|
| **白天 08:00–23:00** | `cn:hy3`（免费） | `cn:deepseek-v4.1-flash`（0.03x）→ `cn:glm-5.3-flash`（0.06x，1M 上下文、能看图）→ `cn:hy3-x`（0.05x，兜底） |
| **夜间 23:00–08:00** | `cn:hy4-preview`（夜间免费窗口） | `cn:hy3` → `cn:deepseek-v4.1-flash` → `cn:glm-5.3-flash` → `cn:hy3-x` |

上面这几个值（白天主模型、夜间主模型、白天窗口起止、降级链）都在面板「配置」页 →「模型编排」里改，保存后立即生效。

- 免费额度用完不用管：链是按「免费 → 低倍率 → 兜底」排的，自然落到下一个。
- 链尾 `cn:hy3-x` 是兜底位，账号还有额度就一定有响应，不会把请求打空。

### 给具体模型单独配降级

传具体模型名默认是原样透传、没有降级。可以在面板「模型编排」的降级表里给它单独配一条链：

```json
"model_fallback": {
  "cn:hy3":                 ["cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:deepseek-v4.1-flash": ["cn:glm-5.3-flash", "cn:hy3-x"]
}
```

左边写模型名，必须跟请求里传的一字不差；右边写它挂了之后要顶上的候选。自动去重，最多 8 个、递归 3 层。没写进这张表的模型，就是原样透传。

---

<a id="protocol-compat"></a>

## 🆕 新增三：多协议接入（Claude Code / Codex 直接连）

原来网关只认 OpenAI 那一个聊天接口，Claude Code 走的是 `/v1/messages`、新版 OpenAI SDK 和 Codex 走的是 `/v1/responses`，接上来直接 404。现在三种都能打进来，共用同一套账号池、子钥匙和模型编排。

| 接口 | 谁在用 |
|---|---|
| `POST /v1/chat/completions` | 绝大多数 OpenAI 兼容客户端（一直在用的那个，行为没动） |
| `POST /v1/responses` | 新版 OpenAI SDK / Codex |
| `POST /v1/messages` | Claude Code 等 Anthropic 系客户端 |

- **鉴权**：沿用管理员钥匙和 `wbk_` 子钥匙；报错按你进的是哪个口返回对应格式，客户端不用额外适配
- **计费**：跟聊天接口完全一致，同一份用量统计、同一套额度扣减
- **模型编排**：`auto` / `cn:auto` 三个接口都生效，实际用了哪个模型在响应头里带出来
- **用不上的参数直接忽略**，不报错

```bash
# 新版 OpenAI SDK / Codex
curl http://HOST:7863/v1/responses \
  -H "Authorization: Bearer <你的key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","input":"用一句话介绍自己"}'

# Claude Code 等 Anthropic 系客户端
curl http://HOST:7863/v1/messages \
  -H "Authorization: Bearer <你的key>" \
  -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"auto","max_tokens":1024,"messages":[{"role":"user","content":"用一句话介绍自己"}]}'
```

Claude Code 把环境变量 `ANTHROPIC_BASE_URL` 填成网关地址就行（例如 `http://HOST:7863`），钥匙用子钥匙或管理员钥匙都可以。

---

## License

[MIT](LICENSE)。再分发请保留 MIT 声明。本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利。

## 🙏 致谢

- 上游：[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
- 本仓库二改（2026-09）：**API 密钥分发**、**模型编排** 与 **多协议接入**，新增代码：

| 新增文件 | 作用 |
|---|---|
| `internal/apikeys/apikeys.go` | 密钥库：签发 / 校验 / 额度 / 白名单 / 落盘 |
| `internal/autoroute/autoroute.go` | 编排引擎：昼夜主模型 + 降级链展开 |
| `internal/panel/keys.go` | 面板密钥管理接口 `/panel/api/keys` |
| `internal/server/compat.go` | 多协议接入：转写框架、响应录制器、流式事件发射、按协议分发错误 |
| `internal/server/compat_responses.go` | OpenAI Responses API 双向转写 |
| `internal/server/compat_messages.go` | Anthropic Messages API 双向转写 |

改动的上游文件：`cmd/server/config.go`、`cmd/server/main.go`、`internal/server/handler.go`（新增两条路由）、`internal/server/resolve_model.go`、`internal/livecfg/livecfg.go`、`internal/upstream/client.go`、`internal/upstream/hint.go`、`internal/panel/panel.go`、`internal/panel/app.js`、`internal/panel/index.html`。
