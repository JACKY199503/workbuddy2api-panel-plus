#!/usr/bin/env bash
# 恢复指定快照：把容器整体还原到快照时刻（镜像 / 源码 / 脚本 / 编排 / 配置 / 账号 / 数据）
#
# 用法：bash /opt/wb2api/tools/restore.sh <快照目录>
#   例：bash /opt/wb2api/tools/restore.sh /opt/wb2api/tools/snapshots/wb2api-20260916-223118
set -euo pipefail

if [ "${_RESTORE_SELF:-}" != "1" ]; then
  cp -f /opt/wb2api/tools/restore.sh /tmp/.restore-self.sh 2>/dev/null || true
  _RESTORE_SELF=1 exec bash /tmp/.restore-self.sh "$@"
fi

DIR=/opt/wb2api
TOOLS=$DIR/tools
SNAPDIR=$TOOLS/snapshots

if [ $# -lt 1 ]; then
  echo "用法：bash /opt/wb2api/tools/restore.sh <快照目录>"
  echo "现有快照："
  ls -1dt "$SNAPDIR"/wb2api-* 2>/dev/null || echo "  （无）"
  exit 1
fi

BK="$1"
[ -d "$BK" ] || { echo "快照目录不存在：$BK"; exit 1; }
cd "$DIR"
echo "快照：$BK"

# 校验
python3 -c "import json,sys;json.load(open('$BK/config.json'))" 2>/dev/null \
  || { echo "config.json 缺失或损坏，中止。"; exit 1; }
[ -d "$BK/auths" ] || { echo "auths/ 缺失，中止。"; exit 1; }
echo "  校验通过（账号 $(ls -1 "$BK/auths" | wc -l) 个）"

# 停容器
echo "[恢复] 停止容器..."
docker stop workbuddy2api >/dev/null 2>&1 || docker compose stop >/dev/null 2>&1 || true

# 镜像
IMG_TAR=$(ls -1 "$BK"/image.*.tar.gz 2>/dev/null | head -1 || true)
if [ -n "$IMG_TAR" ]; then
  echo "[恢复] 载入镜像 $(basename "$IMG_TAR") ..."
  gunzip -c "$IMG_TAR" | docker load | tail -1
  WANT=$(python3 -c "
import json;m=json.load(open('$BK/manifest.json'))
print((m.get('snapshot') or {}).get('id') or m.get('image',{}).get('id') or '')" 2>/dev/null || true)
  if [ -n "$WANT" ] && docker image inspect "$WANT" >/dev/null 2>&1; then
    docker tag "$WANT" wb2api-wb2api:latest
    echo "      镜像就位"
  else
    echo "      ! 镜像 ID 校验未通过，请检查 docker images"
  fi
else
  echo "[恢复] 快照无镜像包，程序版本保持当前"
fi

# 文件
cp -a "$BK/config.json" "$DIR/config/config.json"
rm -rf "$DIR/auths"; cp -a "$BK/auths" "$DIR/auths"
[ -d "$BK/data" ] && { rm -rf "$DIR/data"; cp -a "$BK/data" "$DIR/data"; } || true
for f in Dockerfile go.mod go.sum docker-compose.yml; do
  [ -f "$BK/$f" ] && cp -a "$BK/$f" "$DIR/$f" || true
done
for d in cmd internal scripts; do
  [ -d "$BK/$d" ] && { rm -rf "$DIR/$d"; cp -a "$BK/$d" "$DIR/$d"; } || true
done
[ -d "$BK/host-scripts" ] && cp -a "$BK/host-scripts"/. "$TOOLS/" || true
chown -R 10001:10001 "$DIR/config" "$DIR/auths" "$DIR/data" 2>/dev/null || true
chmod +x "$TOOLS"/*.sh 2>/dev/null || true
echo "      文件已恢复"

docker compose config --quiet 2>/dev/null || echo "      ! compose 语法有问题，请手工检查"

# 起容器
echo "[恢复] 启动容器..."
docker compose up -d 2>&1 | tail -2
for i in $(seq 1 30); do
  curl -sf -m 3 http://127.0.0.1:7863/healthz >/dev/null 2>&1 && break
  sleep 2
done
echo
docker compose ps
curl -s -m 5 http://127.0.0.1:7863/healthz || true
echo
