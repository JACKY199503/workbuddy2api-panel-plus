#!/usr/bin/env bash
# 整容器快照：docker commit 抓容器整体（含可写层）+ 源码 + 脚本 + 编排 + 配置 + 数据
#
# 用法：
#   bash /opt/wb2api/tools/snapshot.sh                 # -> tools/snapshots/wb2api-<时间戳>
#   bash /opt/wb2api/tools/snapshot.sh /root/mybackup  # 存到指定目录
#   bash /opt/wb2api/tools/snapshot.sh --keep 3        # 只保留最近 3 份（默认 5）
set -euo pipefail

DIR=/opt/wb2api
TOOLS=$DIR/tools
SNAPDIR=$TOOLS/snapshots
KEEP=5
OUT=""
AUTO=1

while [ $# -gt 0 ]; do
  case "$1" in
    --keep)   KEEP="$2"; shift 2 ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) OUT="$1"; AUTO=0; shift ;;
  esac
done

TS=$(date +%Y%m%d-%H%M%S)
if [ -z "$OUT" ]; then OUT="$SNAPDIR/wb2api-$TS"; AUTO=1; fi
mkdir -p "$OUT"
cd "$DIR"
echo "[快照] 目标：$OUT"

cp -a config/config.json "$OUT/"   2>/dev/null || true
cp -a auths              "$OUT/"   2>/dev/null || true
cp -a data               "$OUT/"   2>/dev/null || true
cp -a docker-compose.yml "$OUT/"   2>/dev/null || true
cp -a Dockerfile go.mod go.sum "$OUT/" 2>/dev/null || true
for d in cmd internal scripts; do
  [ -d "$d" ] && cp -a "$d" "$OUT/" || true
done
mkdir -p "$OUT/host-scripts"
cp -a "$TOOLS"/*.sh "$OUT/host-scripts/" 2>/dev/null || true
echo "      文件已收集（脚本 $(ls -1 "$OUT/host-scripts" 2>/dev/null | wc -l) 个）"

SNAP_TAG=""
if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx workbuddy2api; then
  SNAP_TAG="wb2api-snap:$TS"
  echo "[快照] docker commit 容器 -> $SNAP_TAG（含可写层）..."
  docker commit --pause=false workbuddy2api "$SNAP_TAG" >/dev/null
  docker diff workbuddy2api 2>/dev/null | head -60 > "$OUT/container-diff.txt" || true
  docker save "$SNAP_TAG" | gzip -1 > "$OUT/image.container.tar.gz"
elif docker image inspect wb2api-wb2api:latest >/dev/null 2>&1; then
  echo "[快照] 容器未运行，退化为导出镜像 wb2api-wb2api:latest"
  docker save wb2api-wb2api:latest | gzip -1 > "$OUT/image.container.tar.gz"
else
  echo "[快照] !! 无容器无镜像，这份快照无法回滚程序版本"
fi
[ -f "$OUT/image.container.tar.gz" ] && echo "      镜像包 $(du -h "$OUT/image.container.tar.gz" | cut -f1)"

python3 - "$OUT" "$SNAP_TAG" "$KEEP" <<'PY'
import json, os, subprocess, sys, datetime
out, snap_tag, keep = sys.argv[1], sys.argv[2], sys.argv[3]
def sh(c):
    return subprocess.run(c, shell=True, capture_output=True, text=True).stdout.strip()
m = {"created": datetime.datetime.now().isoformat(timespec="seconds"),
     "restore_hint": "bash /opt/wb2api/tools/restore.sh " + out}
if snap_tag:
    m["snapshot"] = {"method": "docker commit（容器整体，含可写层）", "tag": snap_tag,
                     "id": sh("docker image inspect %s --format '{{.Id}}' 2>/dev/null" % snap_tag)}
m["image"] = {"tag": "wb2api-wb2api:latest",
              "id": sh("docker image inspect wb2api-wb2api:latest --format '{{.Id}}' 2>/dev/null")}
raw = sh("docker inspect workbuddy2api --format '{{json .}}' 2>/dev/null")
if raw:
    try:
        arr = json.loads(raw); d = arr[0] if isinstance(arr, list) else arr
        cfg, hc = d.get("Config", {}), d.get("HostConfig", {})
        m["container"] = {"image_id": d.get("Image"), "entrypoint": cfg.get("Entrypoint"),
                          "env": cfg.get("Env"), "user": cfg.get("User"), "binds": hc.get("Binds"),
                          "port_bindings": hc.get("PortBindings"),
                          "restart_policy": hc.get("RestartPolicy"),
                          "state": (d.get("State") or {}).get("Status")}
    except Exception:
        pass
m["files"] = sorted(os.listdir(out))
json.dump(m, open(os.path.join(out, "manifest.json"), "w"), ensure_ascii=False, indent=2)
PY

[ -n "$SNAP_TAG" ] && docker rmi "$SNAP_TAG" >/dev/null 2>&1 || true
if [ "$AUTO" = "1" ]; then
  ls -1dt "$SNAPDIR"/wb2api-* 2>/dev/null | tail -n +$((KEEP+1)) | xargs -r rm -rf
  echo "[快照] 保留最近 $KEEP 份"
fi

echo
echo "[快照] 完成：$OUT  ($(du -sh "$OUT" | cut -f1))"
echo "恢复：bash /opt/wb2api/tools/restore.sh $OUT"
