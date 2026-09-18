#!/usr/bin/env bash
#
# 资源巡检（资源管理 v2）：重算 sha256 与索引比对 + 列重复内容 + 打印全貌。
# 只读操作（verify 会顺带补建索引，不动资源文件）。
#
# 用法：bash scripts/verify.sh
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF_RUNTIME_DIR="$(cd "${DIR}/.." && pwd)"
ENV_FILE="${SELF_RUNTIME_DIR}/backend/.env"

PORT="${ASSET_HUB_PORT:-}"
if [ -z "${PORT}" ] && [ -f "${ENV_FILE}" ]; then
  PORT="$(awk -F= '/^[[:space:]]*ASSET_HUB_PORT[[:space:]]*=/{gsub(/[[:space:]\"]/,"",$2); v=$2} END{print v}' "${ENV_FILE}" 2>/dev/null || true)"
fi
if [ -z "${PORT}" ]; then PORT="${SERVICE_PORT:-8080}"; fi
BASE="http://127.0.0.1:${PORT}"
pretty() { python3 -m json.tool 2>/dev/null || cat; }

echo "=== 全貌 ==="
curl -fsS -m 15 "${BASE}/api/v1/maintenance/status" | pretty
echo
echo "=== 巡检（全量重算 sha256）==="
curl -fsS -m 900 "${BASE}/api/v1/maintenance/verify" | pretty
echo
echo "=== 重复内容 ==="
curl -fsS -m 120 "${BASE}/api/v1/maintenance/duplicates" | pretty
