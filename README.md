# home-asset-hub

家庭**资源托管服务器**（图片 / 音频 / 视频 / PDF）：资源管理 + 备份。Go 实现，纯标准库、离线可构建。

第一步先把**小米电视 / 小度依赖的那条链路**接管过来 —— 这条链路原本由 `home-agent-os` 里的
Python `img-server/serve.py`（`:8080`）提供，现在换成这里。

## 为什么换实现不改调用方

电视/音箱拿到的 URL 与 Edge 的上传接口**保持逐字兼容**：

| 用途 | 契约（与现网 img-server 相同） |
|---|---|
| 电视/小度拉字节 | `GET /{key}`、`GET /img/{key}` → `http://<mac-lan-ip>:8080/<key>` |
| Edge 上传（TTS/PDF/音频产物） | `POST /api/v1/photos/upload`（multipart 字段 `file`）→ `{ok, filename, saved_as, bytes, path, url}` |
| 其它 | `GET /health`、`GET /latest`、`GET /api/v1/photos/{key}`（attachment）、`GET /api/v1/photos/download_latest` |
| 发现 | mDNS `_ha-img-server._tcp`（实例名 `Home Agent img-server`，`img-server.local`） |
| 存储 | 扁平文件库 `<dir>/<8hex>_<原名>`；**key 就是 Brain `assets.storage.key/saved_as`** |

因此 Edge（`mac/src/mac_edge/asset/img_upload.py`）、Brain（`storage.backend=img_server` + `/content` 代理）、
iOS（mDNS 发现）**都不需要改一行**。

## 比现网实现多出来的

- **Range / 206**：用 `http.ServeContent` → `Accept-Ranges: bytes`、`If-Modified-Since`、分片请求都支持
  （现网 Python 版恒 200；DLNA 播放器对可 seek 的流更友好）
- **资源管理 v1**：`GET /api/v1/assets?limit=&mime=`（按修改时间倒序清单：key/字节数/时间/mime）、
  `GET /api/v1/assets/{key}`（单条元数据）；`GET /health` 带 `files`/`bytes`/`public_base`
- **安全名**：key 只允许单层文件名（含 `..`、`/`、`\` 一律拒），上传名做控制字符清洗

## 运行

```bash
# 本地/开发：直接跑（默认 data/img，端口 8080）
ASSET_HUB_DIR=/tmp/asset-hub-dev go run ./cmd/assethub

# runtime（部署系统托管，<runtime> = /Users/gaolei/runtime/home-asset-hub）
bash scripts/start.sh     # 或 scripts/restart.sh / scripts/stop.sh
curl 127.0.0.1:8080/health
```

### 部署（agent-control-plane-deployment 规范）

- 仓库根 `build.sh`：`go build` → `outputs/{bin/assethub, scripts/*.sh}`（**只用标准库 → 离线可构建**；
  构建前跑 `go vet ./...` + `go test ./...`）；`VERSION/COMMIT/GIT_REPO_URL` 由平台写
- 平台部署 `rsync -a --delete --filter='P backend/{.env,data/,runtime.pid,server.log,...}'`：
  **runtime 目录里只有 `backend/` 那几项能活过部署**
- 契约：`port=8080`（电视/小度的 URL 就是它）、`healthUrl=http://127.0.0.1:8080/health`、
  `startCmd/stopCmd/restartCmd = bash scripts/{start,stop,restart}.sh`

```
<runtime>/home-asset-hub/
├── bin/assethub  scripts/            ← 发版包
└── backend/                          ← 平台保留位
    ├── .env                          资源目录/对外地址（见 .env.example）
    ├── runtime.pid  server.log
    └── data/img/                     没配 ASSET_HUB_DIR 时的默认资源目录
```

**资源目录本机生产**：`/Users/gaolei/artifact-storage/home-asset-hub/img`（代码与 runtime 之外；
备份/巡检直接对这个目录做即可）。

## 环境变量

| 新名 | 兼容旧名 | 默认 |
|---|---|---|
| `ASSET_HUB_HOST` | `PHOTO_UPLOAD_HOST` | `0.0.0.0` |
| `ASSET_HUB_PORT` | `PHOTO_UPLOAD_PORT` | `8080` |
| `ASSET_HUB_DIR` | `PHOTO_UPLOAD_DIR` | `<runtime>/backend/data/img` |
| `ASSET_HUB_PUBLIC_BASE` | `PHOTO_PUBLIC_BASE` | 按 LAN IP 自动探测 |
| `ASSET_HUB_MDNS` | `MAC_EDGE_IMG_MDNS` | `1`（广告 `_ha-img-server._tcp`） |
| `ASSET_HUB_MAX_UPLOAD_BYTES` | — | 64 MiB |

## 路线图

- **v1（本版）**：接管 img-server 契约（取字节 + 上传 + 发现）+ 资源清单 API
- v2：去重/校验（sha256 索引）、按类型/来源分类、定时备份（rsync/对象存储）、删除与保留策略

## 布局

```
cmd/assethub/       入口：环境解析、LAN IP 探测、HTTP 服务、mDNS、优雅退出
internal/store/     扁平文件库（key 安全校验、写入、清单、最新图片）
internal/httpapi/   HTTP 契约（兼容现网 + 资源管理接口）
internal/mdns/      `dns-sd -R` 广告 `_ha-img-server._tcp`
scripts/            平台启停脚本
build.sh            打包（平台流水线调用）
```
