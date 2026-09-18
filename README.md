# home-asset-hub

家庭**资源托管服务器**（图片 / 音频 / 视频 / PDF）：资源管理 + 备份。Go 实现，纯标准库、离线可构建。

第一步先把**小米电视 / 小度依赖的那条链路**接管过来 —— 这条链路原本由 `home-agent-os` 里的
Python `img-server/serve.py`（`:8080`）提供，现在换成这里。第二步补上**资源管理**：
sha256 索引（去重与校验的地基）、按类型分类、巡检、快照备份、保留策略。

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
- **安全名**：key 只允许单层文件名（含 `..`、`/`、`\` 一律拒），上传名做控制字符清洗

### 资源管理 v1（清单）

- `GET /api/v1/assets?limit=&offset=&mime=&kind=`（按修改时间倒序；带全库分类统计）
- `GET /api/v1/assets/{key}`（单条元数据，含 kind / mime / sha256）
- `GET /health` 带 `files` / `bytes` / `kinds` / `index` / `public_base`

### 资源管理 v2（去重 / 分类 / 巡检 / 备份 / 保留）

| 能力 | 怎么做 | 从哪里看 |
|---|---|---|
| **sha256 索引** | 资源目录里的 append-only JSONL `.assethub-index.jsonl`（隐藏文件 → 现网调用方完全看不见；备份顺带备走）。一 key 一行，删除写墓碑 | `GET /health` 的 `index`、`GET /api/v1/maintenance/status` |
| **上传去重** | 同 sha256 复用已存在的 key（不写第二份字节），**并把该文件 mtime 顶新** —— 保住「重传同一张图 → 变成最新」这条现网用法。默认开，`ASSET_HUB_DEDUPE=0` 或 `?dedupe=0` 关 | 上传响应 `deduped` / `sha256` |
| **按类型分类** | 固定 7 类 `image/audio/video/pdf/text/document/other`；扩展名走显式映射表（不依赖宿主机 mime.types，跨机器一致） | `?kind=pdf`、`GET /health` 的 `kinds`、`kind_counts` |
| **巡检（内容被动过能查出来）** | 重算 sha256 与索引比对：`hash_mismatch` / `size_changed` / `orphan_index` / `read_error`；缺索引的顺带补上（自愈） | `GET /api/v1/maintenance/verify?quick=1` |
| **重复内容报告** | 同 sha256 多 key 分组 + 可省空间 | `GET /api/v1/maintenance/duplicates` |
| **快照备份** | 进程内定时 rsync 快照：`--link-dest` 硬链接增量（内容没变不占空间）、`--delete` 让每份快照等于当时的样子、先写 `.partial` 成功再 rename（半成品不会被当快照）、保留最新 N 份 | `GET /health` 的 `backup`/`snapshots`、`POST /api/v1/maintenance/backup`、`scripts/backup.sh` |
| **保留策略** | 按 `older_than`（时间窗）/ 点名 `keys` 清理，`kind` 只作过滤器；**默认 dry-run 只报候选**，真删要显式 `dry_run=0` 或配 `ASSET_HUB_RETENTION_DAYS>0` | `POST /api/v1/maintenance/prune` |
| **删除** | `DELETE /api/v1/assets/{key}`：删文件 + 写索引墓碑（不留指向空文件的活行） | — |

几处刻意的选择：

- **保留策略默认关**。它会真删文件，而 Brain 的 `assets` 引用不会跟着清 —— 所以先看 dry-run 候选再开。
  真跑时会逐条 `Warn` 记日志（删了什么、多大、什么时候的）。
- **启动时补建索引**（`ASSET_HUB_INDEX_BUILD_ON_START=1`）：v1 时代入库的 181 个文件本来没有 sha256，
  首次开机后台补一遍（158 MB 量级 < 1s），不阻塞 `/health` 就绪。
- 备份与资源目录同盘 —— 目标是「误删/误覆盖能回滚」，异地容灾不在本版范围（见路线图）。
- `ASSET_HUB_BACKUP=0` 只关**定时**；手动 `POST /api/v1/maintenance/backup`／`scripts/backup.sh` 仍然可用
  （用户显式要一份快照时不该被配置挡住）。
- 快照的保证是「**拍快照那一刻**的样子」：两次快照之间被改/被删的字节回不去（拍之前没留过）。
  误删/误清理能救，是因为删除发生在某份快照之后 → 从它之前那份里拿回来：

  ```bash
  SNAP=/Users/gaolei/artifact-storage/home-asset-hub/backup/snapshots/20260918-224445
  cp "$SNAP/<key>" /Users/gaolei/artifact-storage/home-asset-hub/img/    # 回滚一份
  ```

- 去重的**已知边界**：复用 key 意味着「两条 asset 记录可能指向同一个文件」——删其中一条
  （`DELETE` / prune）会让另一条取不到字节。清理走的是显式 dry-run 流程、删除逐条 `Warn` 记日志；
  真在意这点就 `ASSET_HUB_DEDUPE=0`（本版取「默认开省空间，清理靠人工确认」）。

## 运行

```bash
# 本地/开发：直接跑（默认 data/img，端口 8080）
ASSET_HUB_DIR=/tmp/asset-hub-dev go run ./cmd/assethub

# runtime（部署系统托管，<runtime> = /Users/gaolei/runtime/home-asset-hub）
bash scripts/start.sh     # 或 scripts/restart.sh / scripts/stop.sh
curl 127.0.0.1:8080/health

# 运维（v2）：巡检（重算 sha256 + 重复报告）与手动快照
bash scripts/verify.sh
bash scripts/backup.sh
```

### 部署（agent-control-plane-deployment 规范）

- 仓库根 `build.sh`：`go build` → `outputs/{bin/assethub, scripts/*.sh}`（**只用标准库 → 离线可构建**；
  构建前跑 `go vet ./...` + `go test ./...`）；`VERSION/COMMIT/GIT_REPO_URL` 由平台写
- 平台部署 `rsync -a --delete --filter='P backend/{.env,data/,runtime.pid,server.log,...}'`：
  **runtime 目录里只有 `backend/` 那几项能活过部署**
- 契约：`port=8080`（电视/小度的 URL 就是它）、`healthUrl=http://127.0.0.1:8080/health`、
  `startCmd/stopCmd/restartCmd = bash scripts/{start,stop,restart}.sh`

#### 端口：契约口 8080 + 可配的「附加健康检查口」

**8080 不是我们的内部约定，是四个外部消费方的默认值** —— 所以它由外部契约决定，不能跟着平台分配走：

| 消费方 | 端口从哪来 | 换成别的口要改哪 |
|---|---|---|
| Brain 取字节 / 上传 | `BRAIN_IMG_UPLOAD_URL` / `PHOTO_UPLOAD_URL`，默认 `http://127.0.0.1:8080` | Brain env |
| Brain 给的 LAN 地址（电视/小度拉流） | `BRAIN_IMG_PUBLIC_BASE` / `PHOTO_PUBLIC_BASE`，否则探测 `http://<lan-ip>:8080` | Brain env |
| Mac Edge 上传 / 投屏 URL | `MAC_EDGE_LAN_PHOTO_PUBLIC_BASE` 或探测 `http://<lan-ip>:8080`；上传口默认 `127.0.0.1:8080` | Edge env |
| iOS | mDNS `_ha-img-server._tcp` 的 **SRV 端口**（`Endpoint.port`） | 不用改（跟着我们广告的口） |
| 云侧静态站 | `http://115.190.153.53:8080`（另一台机器，与本机口无关） | 与本服务无关 |

技术上换口可行，但要**同步改 4 处配置 + 平台登记**，且任何一处漏改不是报错而是「静默拿不到字节」
（电视投屏黑屏 / 上传丢到无人监听的口）。而 Brain 库里 74 行历史 asset 的 `public_base` 还写着
旧 IP（`192.168.3.73:8080`）也说明：**存下来的 URL 不是权威**（`sdk/asset_bytes.py` 优先现算 loopback base）。
收益为零、风险是全链路断一次 —— 所以契约口保持 8080。

**但端口是配置化的**，而且「平台登记填了别的口」不再致命：

```
ASSET_HUB_PORT=8080            # 契约口（电视/小度/Edge/Brain 用的）
ASSET_HUB_EXTRA_PORTS=4236,9000 # 附加监听口（逗号分隔；off/-/none=不附加；默认不附加）
ASSET_HUB_EXTRA_HOST=127.0.0.1 # 附加口绑定地址，默认只绑 loopback（不对外、不进 mDNS）
```

- `scripts/start.sh` 检测到平台注入的 `SERVICE_PORT` ≠ 契约口时，**自动**把它放进 `ASSET_HUB_EXTRA_PORTS`
  （日志会写「附加监听 127.0.0.1:4236 …」）→ **平台健康检查填哪个口都能 200**，而外部契约一点没动。
- `/health` 新增 `listeners` 字段，一眼看到实际监听了哪些地址。
- 想彻底关掉：`ASSET_HUB_EXTRA_PORTS=off`。

#### 踩过的坑：健康检查查错口（2026-09-18）

首次走平台发版时服务登记页**自动填了空闲口 4236**：

```
[deploy] pipeline-edf703c6 restart via contract (SERVICE_PORT=4236): bash scripts/restart.sh
restart finished but health check failed: http://127.0.0.1:4236/health
```

服务其实起得好好的（绑 8080），失败的是平台在 4236 上做健康检查。当时的修法是**改登记**
（`PUT /api/services/home-asset-hub`，`port=8080` / `healthUrl=http://127.0.0.1:8080/health`，
平台 UI「服务契约 → 配置」等价），重跑流水线即绿 `ok version=<hash>`。

现在已经双保险：登记按契约填 8080 **最好**（健康检查指向真身），填错了也有上面的附加监听兜住。


```
<runtime>/home-asset-hub/
├── bin/assethub  scripts/            ← 发版包
└── backend/                          ← 平台保留位
    ├── .env                          资源目录/对外地址（见 .env.example）
    ├── runtime.pid  server.log
    └── data/img/                     没配 ASSET_HUB_DIR 时的默认资源目录
```

**资源目录本机生产**：`/Users/gaolei/artifact-storage/home-asset-hub/img`（代码与 runtime 之外；
备份/巡检直接对这个目录做即可）。**默认快照目录**：同级的 `/Users/gaolei/artifact-storage/home-asset-hub/backup/snapshots/<时间戳>/`。

## 环境变量

| 新名 | 兼容旧名 | 默认 |
|---|---|---|
| `ASSET_HUB_HOST` | `PHOTO_UPLOAD_HOST` | `0.0.0.0` |
| `ASSET_HUB_PORT` | `PHOTO_UPLOAD_PORT` | `8080`（外部契约口） |
| `ASSET_HUB_EXTRA_PORTS` | — | 空（附加健康检查口，逗号分隔；只绑 loopback；`off` 关） |
| `ASSET_HUB_EXTRA_HOST` | — | `127.0.0.1` |
| `ASSET_HUB_DIR` | `PHOTO_UPLOAD_DIR` | `<runtime>/backend/data/img` |
| `ASSET_HUB_PUBLIC_BASE` | `PHOTO_PUBLIC_BASE` | 按 LAN IP 自动探测 |
| `ASSET_HUB_MDNS` | `MAC_EDGE_IMG_MDNS` | `1`（广告 `_ha-img-server._tcp`） |
| `ASSET_HUB_MAX_UPLOAD_BYTES` | — | 64 MiB |

资源管理 v2（不配也能跑，括号里是默认值）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ASSET_HUB_DEDUPE` | `1` | 上传同 sha256 复用已存在 key（省空间）；`0` 关，或单请求 `?dedupe=0` |
| `ASSET_HUB_BACKUP` | `1` | 定时快照备份；`0` 关 |
| `ASSET_HUB_BACKUP_DIR` | `<资源目录>/../backup` | 快照根目录（`snapshots/<时间戳>/`） |
| `ASSET_HUB_BACKUP_INTERVAL` | `24h` | 快照间隔（Go 时长或秒数） |
| `ASSET_HUB_BACKUP_KEEP` | `7` | 保留最新几份快照（`<=0` 视为 1） |
| `ASSET_HUB_INDEX_BUILD_ON_START` | `1` | 启动后台补建 sha256 索引 |
| `ASSET_HUB_RETENTION_DAYS` | `0`（关） | >0 才开保留策略，**会真删**更老的资源 |
| `ASSET_HUB_RETENTION_INTERVAL` | `24h` | 保留策略间隔 |
| `ASSET_HUB_RETENTION_MAX` | `200` | 单次最多删多少个 |

## HTTP 接口一览（v2 新增部分）

```
GET    /api/v1/assets?limit=&offset=&mime=&kind=   清单 + 全库分类统计
GET    /api/v1/assets/{key}                        单条元数据（含 sha256）
DELETE /api/v1/assets/{key}                        删一个（写索引墓碑）
GET    /api/v1/maintenance/status                  全貌：数量/体积/分类/索引/备份/保留
GET    /api/v1/maintenance/verify[?quick=1]        巡检（默认全量重算 sha256，顺带补索引）
GET    /api/v1/maintenance/duplicates              同内容多 key 分组 + 可省空间
POST   /api/v1/maintenance/prune?older_than=30d&kind=pdf&max=200&dry_run=1
POST   /api/v1/maintenance/backup                  立刻做一份 rsync 快照
```

`prune` 的 `dry_run` 默认 `1`（只报候选）；真删要显式 `dry_run=0`。
`older_than` 支持 `720h` / `30d` / 秒数；**必须给时间窗或点名 `keys`**（只给 `kind` 等于「删掉这类全部」，会被 400 拒掉）。

## 路线图

- **v1**：接管 img-server 契约（取字节 + 上传 + 发现）+ 资源清单 API
- **v2（本版）**：sha256 索引（去重/校验地基）、按类型分类、巡检（内容被动过能查出来）、
  rsync 硬链接快照备份（保留 N 份）、保留策略（默认 dry-run 只报候选）、删除
- v3 候选：快照 **异地**（外置盘/对象存储，rsync 目标可配）、索引定期压缩（append-only JSONL
  长期增长后重写）、上传来源（`source` 字段）驱动更细的分类与保留（如「TTS 产物保留 7 天」）、
  与 Brain `assets` 表对账（孤儿资源自动回收）

## 布局

```
cmd/assethub/       入口：环境解析、LAN IP 探测、HTTP 服务、mDNS、后台作业、优雅退出
internal/store/     扁平文件库（key 安全校验、写入、清单、最新图片）
  kind.go           类型分类（image/audio/video/pdf/text/document/other）
  index.go          sha256 索引（append-only JSONL，含墓碑）
  maintenance.go    巡检 / 重复分组 / 删除 / 保留清理
internal/httpapi/   HTTP 契约（兼容现网 + 资源管理 + 维护接口）
internal/jobs/      后台作业：rsync 硬链接快照备份、启动补索引、保留策略
internal/mdns/      `dns-sd -R` 广告 `_ha-img-server._tcp`
scripts/            平台启停脚本 + backup.sh / verify.sh（运维用）
build.sh            打包（平台流水线调用）
```
