#!/usr/bin/env bash
#
# 启动 home-asset-hub —— 遵循 agent-control-plane-deployment 部署规范。
#
# 平台调用：cwd = runtimeDir、注入 PORT / SERVICE_PORT / RUNTIME_DIR / APP_VERSION。
# 契约：port=8080、health_url=http://127.0.0.1:8080/health（本服务自带 /health，不需要小服务）。
#
# 为什么端口必须保持 8080：小米电视/小度 是按 `http://<mac-lan-ip>:8080/<key>` 拉字节的，
# Brain 的 assets 表里也存着同一个 key —— 换实现不换端口/URL，Edge/Brain/iOS 零改动。
#
# 运行期文件（平台部署只保留 backend/ 那几项）：
#   backend/.env          资源目录/对外地址等（脚本 source 后 export 给二进制）
#   backend/runtime.pid   本脚本写；backend/server.log stdout/err
# 资源目录默认 <runtime>/backend/data/img；生产用 ASSET_HUB_DIR 指到代码与 runtime 之外。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SELF_RUNTIME_DIR="$(cd "${DIR}/.." && pwd)"
APP_VERSION="${APP_VERSION:-dev}"

echo_log() { echo "[start] $*"; }
warn() { echo "[start][警告] $*" >&2; }
die() { echo "[start][错误] $*" >&2; exit 1; }

if [ -n "${RUNTIME_DIR:-}" ] && [ "${RUNTIME_DIR}" != "${SELF_RUNTIME_DIR}" ]; then
  warn "忽略继承来的 RUNTIME_DIR=${RUNTIME_DIR}，按脚本位置用 ${SELF_RUNTIME_DIR}"
fi
RUNTIME_DIR="${SELF_RUNTIME_DIR}"

BIN="${RUNTIME_DIR}/bin/assethub"
BACKEND_DIR="${RUNTIME_DIR}/backend"
ENV_FILE="${BACKEND_DIR}/.env"
PID_FILE="${BACKEND_DIR}/runtime.pid"
LOG_FILE="${BACKEND_DIR}/server.log"
# 端口解析：ASSET_HUB_PORT（shell）> backend/.env 的 ASSET_HUB_PORT > 平台注入的 SERVICE_PORT > 8080。
# **刻意不读继承来的 PORT**：交互式 shell 里常残留别的服务的 PORT/SERVICE_PORT
# （实测 web-cursor 4211），照它走会把本服务绑到别人的口上。
PORT="${ASSET_HUB_PORT:-}"
if [ -z "${PORT}" ]; then
  PORT="$(awk -F= '/^[[:space:]]*ASSET_HUB_PORT[[:space:]]*=/{gsub(/[[:space:]"]/,"",$2); v=$2} END{print v}' "${ENV_FILE}" 2>/dev/null || true)"
fi
if [ -z "${PORT}" ]; then PORT="${SERVICE_PORT:-8080}"; fi
if [ -n "${SERVICE_PORT:-}" ] && [ "${SERVICE_PORT}" != "${PORT}" ]; then
  warn "忽略继承来的 SERVICE_PORT=${SERVICE_PORT}（本服务的地址契约固定 :${PORT}）。"
  warn "平台登记里的端口如果是 ${SERVICE_PORT}：本脚本会把它开成『附加健康检查口』"
  warn "（只绑 127.0.0.1，不对外），平台健康检查照样能过；地址契约口仍然是 ${PORT}。"
  # 平台登记的口 ≠ 契约口 → 附加监听它（健康检查用）。
  # 显式设过 ASSET_HUB_EXTRA_PORTS（含 off）时以显式配置为准。
  if [ -z "${ASSET_HUB_EXTRA_PORTS:-}" ]; then
    ASSET_HUB_EXTRA_PORTS="${SERVICE_PORT}"
    echo_log "附加监听 127.0.0.1:${SERVICE_PORT}（平台登记的口，供健康检查；不对外、不进 mDNS）"
  fi
fi

[ -x "${BIN}" ] || die "缺少可执行文件 ${BIN}（发版包内容不完整？）"

mkdir -p "${BACKEND_DIR}"

if [ ! -f "${ENV_FILE}" ]; then
  warn "没有 ${ENV_FILE}：用默认资源目录 ${BACKEND_DIR}/data/img（生产请在 .env 里配 ASSET_HUB_DIR）"
  umask 077
  : > "${ENV_FILE}"
fi
chmod 600 "${ENV_FILE}" 2>/dev/null || true
# .env → 进程环境（二进制只认环境变量）
set -a
# shellcheck disable=SC1090
. "${ENV_FILE}"
set +a

ASSET_HUB_DIR="${ASSET_HUB_DIR:-${BACKEND_DIR}/data/img}"
export ASSET_HUB_DIR ASSET_HUB_PORT="${PORT}" ASSET_HUB_HOST="${ASSET_HUB_HOST:-0.0.0.0}"
export ASSET_HUB_EXTRA_PORTS="${ASSET_HUB_EXTRA_PORTS:-}"

# 端点落盘（发现的第一层）：本服务 runtime 里一份，共享发现目录里再一份 ——
# Brain/Edge 读它就知道端口，不必各自硬编码 8080（mDNS 只是跨设备那层的兜底）。
export ASSET_HUB_SERVICE_ID="${ASSET_HUB_SERVICE_ID:-home-asset-hub}"
export ASSET_HUB_ENDPOINT_FILE="${ASSET_HUB_ENDPOINT_FILE:-${BACKEND_DIR}/endpoint.json}"
export ASSET_HUB_DISCOVERY_DIR="${ASSET_HUB_DISCOVERY_DIR:-$(cd "${RUNTIME_DIR}/.." && pwd)/.discovery}"
mkdir -p "${ASSET_HUB_DIR}"

if [ -f "${PID_FILE}" ]; then
  old="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
    echo_log "已在运行 pid=${old}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

if command -v lsof >/dev/null 2>&1; then
  holder="$(lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  if [ -n "${holder}" ]; then
    hint=""
    if [ "${PORT}" = "8080" ]; then hint="（旧 Python img-server 也在这个口上：先停掉它）"; fi
    die "端口 ${PORT} 已被 pid=${holder} 占用：$(ps -o command= -ww -p "${holder}" 2>/dev/null | head -c 160)${hint}"
  fi
fi

echo_log "启动 部署版本=${APP_VERSION} 监听=0.0.0.0:${PORT} 资源目录=${ASSET_HUB_DIR} 健康=http://127.0.0.1:${PORT}/health"
cd "${RUNTIME_DIR}"
nohup "${BIN}" >> "${LOG_FILE}" 2>&1 < /dev/null &
echo $! > "${PID_FILE}"
pid="$(cat "${PID_FILE}")"

for _ in $(seq 1 40); do
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    echo "[start][错误] 进程已退出，最近日志：" >&2
    tail -20 "${LOG_FILE}" >&2 || true
    exit 1
  fi
  if curl -fsS -m 2 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    echo_log "启动成功 pid=${pid} 资源目录=${ASSET_HUB_DIR} 日志=${LOG_FILE}"
    exit 0
  fi
  sleep 0.5
done

echo "[start][错误] 20s 内 /health 未就绪，最近日志：" >&2
tail -20 "${LOG_FILE}" >&2 || true
kill "${pid}" 2>/dev/null || true
rm -f "${PID_FILE}"
exit 1
