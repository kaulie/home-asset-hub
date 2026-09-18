#!/usr/bin/env bash
#
# 停止 home-asset-hub（TERM → 15s → KILL）。
# 兜底：pid 文件丢了就按端口找命令行含 assethub 的监听进程（只认本服务）。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF_RUNTIME_DIR="$(cd "${DIR}/.." && pwd)"
log() { echo "[stop] $*"; }

if [ -n "${RUNTIME_DIR:-}" ] && [ "${RUNTIME_DIR}" != "${SELF_RUNTIME_DIR}" ]; then
  log "忽略继承来的 RUNTIME_DIR=${RUNTIME_DIR}，按脚本位置用 ${SELF_RUNTIME_DIR}"
fi
RUNTIME_DIR="${SELF_RUNTIME_DIR}"
PID_FILE="${RUNTIME_DIR}/backend/runtime.pid"
PORT="${SERVICE_PORT:-${ASSET_HUB_PORT:-8080}}"

PIDS=()
if [ -f "${PID_FILE}" ]; then
  pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    PIDS+=("${pid}")
  else
    log "pid 文件里的 ${pid:-?} 已不存在，清理"
  fi
  rm -f "${PID_FILE}"
fi

if [ "${#PIDS[@]}" -eq 0 ] && command -v lsof >/dev/null 2>&1; then
  for cand in $(lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null || true); do
    cmd="$(ps -o command= -ww -p "${cand}" 2>/dev/null || true)"
    case "${cmd}" in
      *assethub*)
        log "兜底：端口 ${PORT} 上的 pid=${cand} 是 Asset Hub"
        PIDS+=("${cand}")
        ;;
    esac
  done
fi

if [ "${#PIDS[@]}" -eq 0 ]; then
  log "没有运行中的 Asset Hub"
  exit 0
fi

for pid in "${PIDS[@]}"; do
  log "TERM → pid=${pid}"
  kill "${pid}" 2>/dev/null || true
  for _ in $(seq 1 30); do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 0.5
  done
  if kill -0 "${pid}" 2>/dev/null; then
    log "15s 内未退出，KILL → pid=${pid}"
    kill -9 "${pid}" 2>/dev/null || true
  fi
done
log "已停止"
