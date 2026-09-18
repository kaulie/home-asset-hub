#!/usr/bin/env bash
#
# 手动做一份资源快照备份（资源管理 v2）。
#
# 快照逻辑只在 Go 里有一份（internal/jobs）：本脚本只是按端口找到本服务、
# 打一个 POST /api/v1/maintenance/backup，避免「shell 版 rsync」和「服务版 rsync」两套规则漂移。
#
# 用法：bash scripts/backup.sh
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
echo "[backup] POST ${BASE}/api/v1/maintenance/backup"
if ! curl -fsS -m 600 -X POST "${BASE}/api/v1/maintenance/backup"; then
  echo "[backup][错误] 触发失败：服务没在 ${PORT} 上跑？（bash scripts/start.sh）" >&2
  exit 1
fi
echo
echo "[backup] 当前状态："
curl -fsS -m 15 "${BASE}/api/v1/maintenance/status" | python3 -m json.tool 2>/dev/null || true
