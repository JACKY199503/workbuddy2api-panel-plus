#!/usr/bin/env bash
# 一键升级：整容器快照 -> 拉源码 -> 重打构建补丁 -> compose 补丁 -> 语法校验 -> 重建镜像 -> 重启自检
# 用法：bash /opt/wb2api/tools/upgrade.sh
set -euo pipefail

DIR=/opt/wb2api
TOOLS=$DIR/tools
REPO=https://codeload.github.com/linguo2625469/workbuddy2api-panel/zip/refs/heads/main
TS=$(date +%Y%m%d-%H%M%S)

cd "$DIR"

echo "[1/6] 整容器快照..."
bash "$TOOLS/snapshot.sh" 2>&1 | tail -3 || echo "  !! 快照失败，仍继续升级"

echo "[2/6] 下载最新源码..."
rm -f /tmp/wb2api-src.zip
curl -fsSL -m 180 -o /tmp/wb2api-src.zip "$REPO"
rm -rf /tmp/wb2api-src && mkdir -p /tmp/wb2api-src
unzip -q -o /tmp/wb2api-src.zip -d /tmp/wb2api-src

echo "[3/6] 覆盖源码（保留 config / auths / data / tools）..."
SRC=$(ls -d /tmp/wb2api-src/*/ | head -1)
rm -rf "$DIR/cmd" "$DIR/internal" "$DIR/scripts"
cp -a "$SRC". "$DIR/"
rm -rf /tmp/wb2api-src /tmp/wb2api-src.zip

echo "[4/6] 重新打构建补丁..."
DF="$DIR/Dockerfile"
sed -i '1{/^# syntax=docker\/dockerfile:1$/d}' "$DF"
grep -q '^ENV GOPROXY=' "$DF" || sed -i 's|^RUN go mod download$|ENV GOPROXY=https://goproxy.cn,direct\nRUN go mod download|' "$DF"
grep -q 'mirrors.aliyun.com' "$DF" || sed -i "s|^RUN apk add --no-cache|RUN sed -i 's\|dl-cdn.alpinelinux.org\|mirrors.aliyun.com\|g' /etc/apk/repositories \\\\\n \&\& apk add --no-cache|" "$DF"

echo "[4.5/6] 重新打 compose 补丁..."
python3 - <<'PY'
import re
p = '/opt/wb2api/docker-compose.yml'
s = open(p).read()
if '- ./config.json:/app/config.json' in s:
    s = s.replace('- ./config.json:/app/config.json', '- ./config:/app/config')
s = re.sub(r'^\s*entrypoint:.*$', '', s, flags=re.M)
s = re.sub(r'^(\s*)container_name: workbuddy2api\s*$',
           r"\1container_name: workbuddy2api\n    entrypoint: ['/app/wb2api', '-config', '/app/config/config.json']",
           s, flags=re.M, count=1)
s = re.sub(r'\n{3,}', '\n\n', s)
open(p, 'w').write(s)
PY

echo "[4.6/6] compose 语法校验..."
docker compose config --quiet || { echo "compose 语法错误，中止升级"; exit 1; }

echo "[5/6] 重建镜像（约 2-4 分钟）..."
docker compose build

echo "[6/6] 重启并自检..."
docker compose up -d
for i in $(seq 1 30); do
  curl -sf -m 3 http://127.0.0.1:7863/healthz >/dev/null 2>&1 && break
  sleep 2
done
docker compose ps
curl -s -m 5 http://127.0.0.1:7863/healthz || true
echo
echo "[7/7] 清理构建垃圾（只清超出部分，保留可用缓存）..."
# builder prune：保留最近 512MB 缓存供下次构建复用，只清超出的。
# 全清会让下次升级重新下载 go 依赖，从 2-4 分钟变 5-8 分钟。
docker builder prune --keep-storage 512MB -f 2>&1 | tail -2 || true
# image prune：只删 72 小时前的悬空镜像（升级后变 <none> 的旧镜像）。
# 保留最近 3 天的，万一新版有问题还能直接 docker tag 回去应急。
docker image prune -f --filter "until=72h" 2>&1 | tail -2 || true
echo
echo "清理后磁盘："
docker system df 2>/dev/null | head -4

echo
echo "升级完成。回滚：bash $TOOLS/restore.sh $TOOLS/snapshots/wb2api-$TS"
