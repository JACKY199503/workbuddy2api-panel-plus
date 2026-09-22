#!/usr/bin/env bash
# 自检：两项二改功能（API 密钥分发 + 模型编排）的 16 个关键注入点是否都在源码里。
#
# 用法：bash tools/check-enhancements.sh        （在本仓库任意位置）
#
# 用途：合并上游新版本、或手工改过 internal/ 之后，先跑这个确认增强没被冲掉。
#      从本仓库 clone 的干净代码 16 项应当全为 v。
# 只读检查，不修改任何文件。
set -u

S=$(cd "$(dirname "$0")/.." && pwd)
cd "$S" || exit 1

echo "源码根目录：$S"
echo "===== 16 项注入点 ====="
MISSING=0
chk() { # chk <相对路径> <标记串> <说明>
  if [ ! -f "$S/$1" ]; then
    echo "  x 缺失文件 $1"; MISSING=$((MISSING+1)); return
  fi
  if grep -qF "$2" "$S/$1"; then
    echo "  v $3"
  else
    echo "  x $1 未找到标记 [$2]"; MISSING=$((MISSING+1))
  fi
}

chk internal/apikeys/apikeys.go     'func Validate'      '密钥库：字段校验'
chk internal/apikeys/apikeys.go     'TrustProxy'         '密钥库：IP 判定（防 XFF 伪造）'
chk cmd/server/config.go            'trust_proxy'        '配置：trust_proxy 开关'
chk internal/autoroute/autoroute.go 'Chain(model string' '编排：昼夜候选链'
chk internal/panel/keys.go          'panel/api/keys'     '面板：密钥分发接口'
chk internal/panel/panel.go         'panel/api/keys'     '面板：密钥路由注册'
chk cmd/server/config.go            'AutoModel'          '配置：编排结构体'
chk cmd/server/main.go              'apikeys.New'        '启动：密钥库初始化'
chk internal/livecfg/livecfg.go     'Auto '              '热配置：编排快照'
chk internal/server/handler.go      'autoroute'          '网关：编排注入'
chk internal/server/handler.go      'ErrHardCredit'      '网关：额度耗尽 402'
chk internal/server/resolve_model.go 'hourCST'           '网关：CST 昼夜判定'
chk internal/upstream/client.go     '14018'              '上游：14018 额度识别'
chk internal/upstream/hint.go       'credits exhausted'  '上游：额度耗尽提示'
chk internal/panel/app.js           'auto_day_primary'   '前端：编排配置页'
chk internal/panel/index.html       'API'                '前端：密钥页'

if [ "$MISSING" -ne 0 ]; then
  echo "!! $MISSING 项缺失，两项增强不完整。"
  exit 1
fi
echo "全部 16 项就位：API 密钥分发 + 模型编排"
