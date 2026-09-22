# 回归测试脚本

针对本仓库两项二改功能（API 密钥分发、模型编排）的黑盒回归用例。纯 Python 标准库，无依赖。

## 用法

```bash
# 方式一：环境变量
export WB2A_BASE=http://<网关IP>:7863
export WB2A_KEY=<管理员 api_key>

# 方式二：命令行参数
python t_auto.py http://<网关IP>:7863 <管理员api_key>
```

| 脚本 | 覆盖 | 基线 |
|---|---|---|
| `t_auto.py` | 模型编排主流程（昼夜主模型、降级链、`X-WB2A-Routed-Model` 头） | 17 / 0 |
| `t_adv.py` | 密钥分发与编排的反向 / 边界 / 安全用例（代码走查驱动） | 37 / 0 |
| `t_audit.py` | 虚拟模型 × 白名单 × realm 交叉审计 | 20 / 0 |
| `t_realm.py` | 域隔离（cn / global 不串） | 7 / 0（1 SKIP 正常） |
| `t_edge.py` | 边界值 | 12 / 1 |

判定规则与完整验收流程见 [docs/ACCEPTANCE.md](../docs/ACCEPTANCE.md)。

## 注意事项

- 脚本会**真实消耗模型额度**（内部统一用 `max_tokens=1` 压到最低）。
- 只删除脚本自己创建的密钥（名称以测试前缀开头），不会动已有密钥。
- `auto_model` / `model_fallback` 会在开始前快照、结束后恢复。
- `t_edge.py` 基线里的 1 条失败必须是 global 域 503（上游账号额度耗尽的环境性问题）。
