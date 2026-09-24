<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel Plus</h1>

<p align="center">
  <b>把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  本仓库在上游之上<b>只新增三项功能</b>：🔑 API 密钥分发 · 🧠 模型编排 · 🔌 多协议接入
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

## 这是什么

三层 fork，**上游的全部能力一个没动**，只在其上加了密钥分发、模型编排与多协议接入：

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

## 🆕 新增一：API 密钥分发（子钥匙）

**解决什么问题**：原来全站只有一把管理员钥匙 —— 要么不给人用，要么给人用就等于把全部权限交出去。现在可以按需签发子钥匙，每把自带额度和边界，随时停用、随时改额度。

**在哪操作**：面板左侧 **API 密钥** 页 → 右上角「新建密钥」。列表里每一行都能直接停用、改额度、看用量。

**一把钥匙能限制什么**（都在新建窗口里填，填 0 或留空 = 不限制）

| 窗口里的项 | 作用 |
|---|---|
| 密钥名称 | 用来区分用途，必填 |
| 版本归属 | 限定这把钥匙只能走国内版或国际版，默认不限 |
| 有效期（天） | 到期自动失效，填 0 = 长期有效 |
| 最大来源 IP 数 | 限制最多允许几个不同 IP 用过这把钥匙 |
| Token 用量上限 | 用超了就拒绝 |
| 积分用量上限 | 用超了就拒绝 |
| 来源 IP 白名单 | 只允许名单内的 IP 调用，支持网段写法 |
| 模型白名单 | 只允许调用名单内的模型（含虚拟模型 `auto`），留空 = 全部放行 |

钥匙明文以 `wbk_` 开头，**只在创建弹窗里显示这一次**，关掉就找不回来了 —— 服务端只存它的摘要，不存明文，谁也捞不出来。

带 `wbk_` 前缀的钥匙走这套判定，其余的仍按原来的管理员钥匙校验。**一把子钥匙都不建的话，跟以前完全一样**。

---

<a id="model-orchestration"></a>

## 🆕 新增二：模型编排（auto · 免费额度薅满 · 昼夜自动切换）

**一句话目的**：把每天能白嫖的额度尽量用满；免费窗口过期、额度波动、被限流时**自动**换模型，全程不用手动改配置。

**怎么触发**：得先在面板「配置」页 →「模型编排」那里打开「启用模型编排」开关并点「保存配置」，然后请求里模型名传下面这几种，网关才会接管：

| 你传的模型名 | 结果 |
|---|---|
| `auto` | 交给编排：按当前钟点在「白天主模型」和「夜间主模型」之间挑，降级链不挑版本 |
| `cn:auto` | 交给编排，但降级链只保留国内版模型 |
| `global:auto` | **默认配置下不生效** —— 面板里填的主模型和降级链全是国内版，没有国际版候选，这个名字会原样发给上游（基本上是 404）。想让国际版也编排，先往「降级链」里加国际版模型 |
| `cn:hy3`、`cn:deepseek-v4.1-flash` 这类具体名字 | 就是那个模型，原样透传，不参与编排 |

上面几行提到的「白天主模型」「夜间主模型」「降级链」，就是面板「配置」页 →「模型编排」里那几个输入框，改完点「保存配置」立刻生效，不用重启。

三个接口都认这几种写法。模型列表里能查到的虚拟名是 `auto` 和 `cn:auto` 两个。

- **按钟点自动换主模型**：白天吃白天的免费额度，夜里吃夜里的免费窗口，到点自动切回。
- **出问题自动往下顶**：首选模型被限流 / 积分耗尽 / 上游报错 / 返回空正文，就按降级链往下挑下一个能用的。
- **优惠到期不用管**：链是按「免费 → 低倍率 → 兜底」排的，免费额度没了自然落到下一个，配置一行不用动。

### 自动切换逻辑

下面是面板里的默认配置，每一项都能在「配置」页 →「模型编排」里改：白天主模型、夜间主模型、白天窗口（起 / 止 小时，按北京时间）、降级链（逗号分隔，按顺序尝试）。

| 时段 | 首选模型 | 首选不行就依次往下试 |
|---|---|---|
| **白天 08:00–23:00** | `cn:hy3`（免费） | `cn:deepseek-v4.1-flash`（0.03x）→ `cn:glm-5.3-flash`（0.06x，1M 上下文、能看图）→ `cn:hy3-x`（0.05x，兜底） |
| **夜间 23:00–08:00** | `cn:hy4-preview`（夜间老用户免费窗口） | `cn:hy3` → `cn:deepseek-v4.1-flash` → `cn:glm-5.3-flash` → `cn:hy3-x` |

- 切换**不是定时任务**：每次请求进来按当前钟点判定，08:00 一到请求自然回到 `cn:hy3`，无需重启、无需改配置。
- 白天主模型 `cn:hy3` 本身也在降级链里 —— 夜里 `cn:hy4-preview` 挂掉时接上的就是它；白天它已经是首选，链里重复的那一项会被**自动跳过**。所以一条降级链同时服务昼夜两个时段。
- 链尾 `cn:hy3-x` 是兜底位：只要账号还有额度就一定有响应，不会把请求打空。

### 具体模型名也能挂降级链

传具体模型名（比如 `cn:deepseek-v4.1-flash`）就是它自己，跟 `auto` 没关系，不受编排影响。
但可以在面板「配置」页 →「模型编排」的「具名模型降级表」里给它单独配一条链 —— 它挂了就往下顶：

```json
"model_fallback": {
  "cn:hy3":                 ["cn:deepseek-v4.1-flash", "cn:glm-5.3-flash", "cn:hy3-x"],
  "cn:deepseek-v4.1-flash": ["cn:glm-5.3-flash", "cn:hy3-x"]
}
```

左边写模型名，必须跟请求里传的一字不差；右边写它挂了之后要顶上的候选。两条链叠加展开，自动去重，最多 8 个、递归 3 层。
没写进这张表的模型名，就是原样透传，没有降级。

---

<a id="protocol-compat"></a>

## 🆕 新增三：多协议接入（Claude Code / Codex 等客户端直接连）

**解决什么问题**：原来网关只认 OpenAI 那一个聊天接口地址。Claude Code 这类客户端走的是 Anthropic 那套（`/v1/messages`），新版 OpenAI SDK 和 Codex 走的是 Responses 那套（`/v1/responses`），照原来的网关接上来直接就是 404。现在两种都能直接打进来，共用同一套账号池、子钥匙和模型编排。

| 接口地址 | 谁在用 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | 绝大多数 OpenAI 兼容客户端 | 一直在用的那个，行为没动过 |
| `POST /v1/responses` | 新版 OpenAI SDK / Codex | 提问内容写一句话或者一段多轮对话都行，另有系统提示词和最大输出长度，返回值按 Responses 规范给 |
| `POST /v1/messages` | Claude Code 等 Anthropic 系客户端 | 多轮对话 + 系统提示 + 最大输出长度，返回值按 Messages 规范给 |

**怎么做的**：请求进来后先翻成内部聊天接口能看懂的样子，然后走同一条完整链路（挑账号 / 换号 / 冷却 / 编排降级 / 钥匙校验 / 记用量），最后把回答翻回对应协议的格式 —— 非流式一次性转写，流式按各协议的标准一段一段往外推。调度逻辑只有一份，两个新接口没有另起炉灶。

- **鉴权**：沿用原来的管理员钥匙和 `wbk_` 子钥匙；出错时按**你这个请求进的是哪个口**来返回错误格式，客户端不用额外适配。
- **计费**：跟聊天接口完全一致，同一份用量统计、同一套额度扣减。
- **模型编排**：传 `auto` / `cn:auto` 在三个接口上都生效，降级和昼夜切换照常；实际用了哪个模型，响应头里会带出来。
- **用不上的参数直接忽略**，不报错。

**怎么用**

```bash
# 新版 OpenAI SDK / Codex 走这个
curl http://HOST:7863/v1/responses \
  -H "Authorization: Bearer <你的key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","input":"用一句话介绍自己"}'

# Claude Code 等 Anthropic 系客户端走这个
curl http://HOST:7863/v1/messages \
  -H "Authorization: Bearer <你的key>" \
  -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
  -d '{"model":"auto","max_tokens":1024,"messages":[{"role":"user","content":"用一句话介绍自己"}]}'
```

Claude Code 把环境变量 `ANTHROPIC_BASE_URL` 填成网关地址就行（例如 `http://HOST:7863`），钥匙用子钥匙或管理员钥匙都可以，别的不用改。

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
