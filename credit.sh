#!/usr/bin/env bash
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制升级: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
