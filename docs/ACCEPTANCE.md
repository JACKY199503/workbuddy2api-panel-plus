# 验收基线（二改的两项功能）

适用时机：改过 `internal/apikeys/`、`internal/autoroute/`、`internal/panel/keys.go`，或合并过上游新版本源码之后。

> 从本仓库 clone 下来的代码**已经包含两项增强**，正常部署不需要执行本文任何步骤；
> 本文是「改动代码后怎么确认没改坏」的回归手册。

---

## 一、结构自检：16 个注入点

两项增强散落在 10 个上游文件里，源码被动过之后先跑这个，确认关键代码都还在：

```bash
bash tools/check-enhancements.sh
```

输出里 16 项必须**全部是 `v`**；出现任意 `x` 说明对应注入点丢失（常见于合并上游时冲突解决不当）。

16 项分别是：密钥库字段校验、`TrustProxy` IP 判定、`trust_proxy` 配置开关、编排昼夜候选链、面板密钥接口、面板密钥路由注册、`auto_model` 配置结构体、启动期密钥库初始化、编排热配置快照、网关编排注入、`ErrHardCredit` 402 语义、CST 昼夜判定、14018 额度识别、额度耗尽提示、前端编排配置页、前端密钥页。

## 二、功能回归：5 个脚本

在能访问网关的机器上跑（`tests/` 目录内）：

```bash
cd tests
export WB2A_BASE=http://<网关IP>:7863
export WB2A_KEY=<管理员 api_key>
python t_auto.py      # 也可直接 python t_auto.py http://IP:7863 <key>
python t_adv.py
python t_audit.py
python t_realm.py
python t_edge.py
```

| 脚本 | 覆盖 | 基线 |
|---|---|---|
| `t_auto.py` | 模型编排主流程（昼夜主模型、降级链、routed 头） | 17 通过 / 0 失败 |
| `t_adv.py` | 密钥分发与编排的反向 / 边界 / 安全用例 | 37 / 0 |
| `t_audit.py` | 虚拟模型 × 白名单 × realm 交叉审计 | 20 / 0 |
| `t_realm.py` | 域隔离（cn / global 不串） | 7 / 0（1 条 SKIP 正常） |
| `t_edge.py` | 边界值 | 12 / 1 |

**判定规则**：`t_edge.py` 唯一的失败必须是 **global 域 503** —— 这是上游账号额度耗尽导致的环境性问题，不是功能缺陷。出现 503 / 429 / 402 时先按「是不是上游环境问题」归因，再算 bug。

脚本只读配置、自建自删测试密钥，不会动已有密钥；`auto_model` / `model_fallback` 会做快照-恢复。

## 三、手工抽查（30 秒）

1. 面板 → API 密钥：建一把 `wbk_` 钥匙，限一个模型 + 一个 IP
2. 用它调被禁模型 → 期望 **400**
3. 换允许的模型 → 期望 **200**，响应头带 `X-WB2A-Routed-Model`
4. 把它停用 → 期望 **403**
5. `curl $WB2A_BASE/v1/chat/completions -d '{"model":"auto",...}'` → 期望 200，routed 头是当时段的主模型（白天 08:00–23:00 为 `cn:hy3`，夜间为 `cn:hy4-preview`）
6. 面板「配置」页核对 `auto_model`：`day_primary=cn:hy3` / `night_primary=cn:hy4-preview` / `fallback=[cn:hy3, cn:deepseek-v4.1-flash, cn:glm-5.3-flash, cn:hy3-x]`

## 四、回归时的几条硬约束

- 断言 routed 头时做大小写不敏感比较
- 改 `model_fallback` 想清空，必须先提交 `null`（深合并，提交 `{}` 不生效）
- 测额度用 `max_tokens=1`，credit 消耗按 0 处理
- 测试里禁用上游别名（如 `cn:fast-model`），空正文会误触发 `on_empty` 降级
- 只删测试自己建的密钥，不要动别人的
- 看日志用 `docker logs --tail 400 workbuddy2api`（窗口太小会漏掉降级链路）
