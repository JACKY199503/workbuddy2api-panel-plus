#!/usr/bin/env bash
# 自检：容器状态 / 健康 / 补丁 / 账号 / 快照 / 最近日志
# 用法：bash /opt/wb2api/tools/check.sh
DIR=$(cd "$(dirname "$0")/.." && pwd)
TOOLS=$DIR/tools
cd "$DIR"

echo "===== 容器 ====="
docker compose ps 2>&1 | tail -2
curl -s -m 5 http://127.0.0.1:7863/healthz || echo "!! 无响应"
echo

echo "===== 补丁 ====="
grep -q 'goproxy.cn' Dockerfile 2>/dev/null && echo "  [OK] GOPROXY" || echo "  [FAIL] GOPROXY"
grep -q 'mirrors.aliyun.com' Dockerfile 2>/dev/null && echo "  [OK] apk 源" || echo "  [FAIL] apk 源"
grep -q './config:/app/config' docker-compose.yml 2>/dev/null && echo "  [OK] config 目录挂载" || echo "  [FAIL] config 目录挂载"
grep -q "entrypoint:" docker-compose.yml 2>/dev/null && echo "  [OK] entrypoint" || echo "  [FAIL] entrypoint"
grep -q '\\"' docker-compose.yml 2>/dev/null && echo "  [WARN] compose 含转义引号" || echo "  [OK] compose 无转义引号"

echo
echo "===== 账号 ====="
WB2A_DIR="$DIR" python3 - <<'PY'
import json, glob, os, datetime
base = os.environ['WB2A_DIR']
auths = sorted(glob.glob(os.path.join(base, 'auths', '*.json')))
try:
    state = json.load(open(os.path.join(base, 'data', 'state.json'))).get('accounts', {})
except Exception:
    state = {}
for p in auths:
    try:
        d = json.load(open(p))
    except Exception as e:
        print('  %s 解析失败 %s' % (os.path.basename(p), e)); continue
    # 1.10.0+ 是嵌套格式（auth / account 两段），早期是扁平格式，两种都认
    if 'auth' in d or 'account' in d:
        a = d.get('auth') or {}
        acct = d.get('account') or {}
        uid = acct.get('uid') or '?'
        nick = acct.get('nickname') or '?'
        realm = a.get('realm') or '?'
        exp = a.get('expiresAt') or 0
        atok = a.get('accessToken') or ''
        rtok = a.get('refreshToken') or ''
    else:
        uid = d.get('uid') or '?'
        nick = d.get('nickname') or '?'
        realm = d.get('realm') or '?'
        exp = d.get('expiresAt') or 0
        atok = d.get('accessToken') or ''
        rtok = d.get('refreshToken') or ''
    st = state.get(uid, {}) if uid != '?' else {}
    exp_s = datetime.datetime.fromtimestamp(exp).strftime('%Y-%m-%d') if exp else '?'
    print('  %-24s %s  %-6s 积分 %s/%s  到期 %s  token %d/%d' % (
        nick, uid[:8], realm,
        st.get('credits', '-'), st.get('credits_total', '-'), exp_s,
        len(atok), len(rtok)))
print('  共 %d 个' % len(auths))
PY

echo
echo "===== 快照 ====="
ls -1dt "$TOOLS"/snapshots/wb2api-* 2>/dev/null | head -5 || echo "  无"
du -sh "$TOOLS/snapshots" 2>/dev/null

echo
echo "===== 最近日志 ====="
docker compose logs --tail 8 --no-color 2>/dev/null | tail -8
