#!/usr/bin/env bash
#
# 打包脚本 —— 遵循「agent-control-plane-deployment」部署系统规范。
#
# 调用方（二选一，均从仓库根执行）：
#   - 控制面流水线：POST /api/deploy-notify {serviceId:"home-asset-hub"}
#   - 独立发版：~/deployment/bin/release.sh home-asset-hub [ref]
#
# 约定：cwd = 仓库根、APP_VERSION = <8 位短 hash>；产出 outputs/，其中必须含 scripts/restart.sh；
# VERSION / COMMIT / GIT_REPO_URL 由调用方写。运行时可变内容（资源目录、.env、pid、日志）放
# <runtime>/backend/（平台保留）或 runtime 之外（ASSET_HUB_DIR）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT}"

# 平台打包环境 PATH 可能不含 Homebrew（go 在 /usr/local/bin 或 /opt/homebrew/bin）——实测踩过
# build.sh 找不到工具链的情况，这里显式补上。
export PATH="/usr/local/bin:/opt/homebrew/bin:${PATH}"

VERSION="${APP_VERSION:-dev}"
OUT="${ROOT}/outputs"

command -v go >/dev/null 2>&1 || { echo "[build][错误] 本机没有 go 工具链" >&2; exit 1; }
[ -f "${ROOT}/scripts/restart.sh" ] || { echo "[build][错误] 缺少 scripts/restart.sh（平台硬性要求）" >&2; exit 1; }

# Go 缓存隔离在仓库之外（控制面调用时 HOME 可能指向 runtime 目录，缓存落那里会被 rsync 波及）
BUILD_CACHE="${ASSET_HUB_BUILD_CACHE:-${TMPDIR:-/tmp}/home-asset-hub-build-cache}"
export GOMODCACHE="${BUILD_CACHE}/gomodcache" GOCACHE="${BUILD_CACHE}/gocache"
export GOPATH="${BUILD_CACHE}/gopath" GOTMPDIR="${BUILD_CACHE}/gotmp"
mkdir -p "${GOMODCACHE}" "${GOCACHE}" "${GOPATH}" "${GOTMPDIR}"
[ -n "${GOPROXY:-}" ] || export GOPROXY="https://goproxy.cn,direct"

rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/scripts"

echo "[build] home-asset-hub version=${VERSION}"
# 只用标准库 → 离线也能构建
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "${OUT}/bin/assethub" ./cmd/assethub
echo "[build] vet + test"
go vet ./...
go test ./... >/dev/null

cp "${ROOT}/scripts/start.sh" "${ROOT}/scripts/stop.sh" "${ROOT}/scripts/restart.sh" \
   "${ROOT}/scripts/backup.sh" "${ROOT}/scripts/verify.sh" "${OUT}/scripts/"
cp "${ROOT}/README.md" "${OUT}/README.md" 2>/dev/null || true
[ -f "${ROOT}/.env.example" ] && cp "${ROOT}/.env.example" "${OUT}/.env.example"
chmod +x "${OUT}/bin/"* "${OUT}/scripts/"*.sh

echo "[build] outputs 就绪："
ls -1 "${OUT}" "${OUT}/bin" "${OUT}/scripts" | sed 's/^/  /'
