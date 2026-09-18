// Package httpapi：把 Asset Hub 的 HTTP 契约钉住。
//
// 与现网 img-server **完全兼容**的那一组（电视/小度/Edge/Brain 都在用）：
//
//	GET  /health
//	POST /api/v1/photos/upload        multipart 字段 file → {ok, filename, saved_as, bytes, path, url}
//	GET  /{key}、GET /img/{key}        取字节（inline；支持 Range/206 —— DLNA 播放器友好）
//	GET  /api/v1/photos/{key}          取字节（attachment）
//	GET  /latest、GET /api/v1/photos/download_latest
//	GET  /                             路由自述 JSON
//	OPTIONS *                          204 + CORS
//
// 新增（资源管理 v1）：
//
//	GET /api/v1/assets                清单（?limit=&offset=&mime=&kind=），按修改时间倒序
//	GET /api/v1/assets/{key}          单条元数据
//
// 新增（资源管理 v2：sha256 索引 / 分类 / 巡检 / 备份 / 保留）：
//
//	DELETE /api/v1/assets/{key}           删一个资产（写索引墓碑）
//	GET    /api/v1/maintenance/status     全貌：数量体积、分类、索引、备份/保留状态
//	GET|POST /api/v1/maintenance/verify   巡检 ?quick=1（默认全量重算 sha256，顺带补索引）
//	GET    /api/v1/maintenance/duplicates 同内容多 key 的重复组（能省多少空间）
//	POST   /api/v1/maintenance/prune      清理候选/执行 ?older_than=720h&kind=&max=&dry_run=1
//	POST   /api/v1/maintenance/backup     立刻做一份 rsync 快照
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

const (
	ServiceName     = "home-asset-hub"
	UploadPath      = "/api/v1/photos/upload"
	MaxUploadBytes  = 64 << 20 // 64 MiB，与现网一致
	noStoreHeader   = "no-store, no-cache, must-revalidate, max-age=0"
	defaultMimeType = "application/octet-stream"
)

// Jobs 后台作业（快照备份 / 巡检 / 保留策略）——由 cmd 层注入；nil 表示没有维护能力。
type Jobs interface {
	Status() map[string]any
	BackupNow(ctx context.Context) (map[string]any, error)
	VerifyNow(ctx context.Context, quick bool) (store.VerifyReport, error)
	PruneNow(opts store.PruneOptions) (store.PruneReport, error)
}

// Options 组装参数。
type Options struct {
	PublicBase string // 上传响应里的 url 前缀；空则按请求 Host 兜底
	Dedupe     bool   // 上传时同 sha256 复用已存在的 key
	Jobs       Jobs   // 维护作业；nil=维护接口返回 503
}

// Server 组装路由。
type Server struct {
	store      *store.Store
	publicBase string
	dedupe     bool
	jobs       Jobs
	logger     *slog.Logger
	startedAt  time.Time

	maintMu sync.Mutex // 维护操作串行化：verify/prune 不互相踩（别一边删一边算 hash）
}

// New v1 兼容构造（不去重、无维护作业）。
func New(st *store.Store, publicBase string, logger *slog.Logger) *Server {
	return NewWithOptions(st, Options{PublicBase: publicBase}, logger)
}

func NewWithOptions(st *store.Store, opts Options, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:      st,
		publicBase: strings.TrimRight(strings.TrimSpace(opts.PublicBase), "/"),
		dedupe:     opts.Dedupe,
		jobs:       opts.Jobs,
		logger:     logger,
		startedAt:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc(UploadPath, s.handleUpload)
	mux.HandleFunc("/api/v1/assets", s.handleAssets)
	mux.HandleFunc("/api/v1/assets/", s.handleAssetOne)
	mux.HandleFunc("/api/v1/maintenance/status", s.handleMaintStatus)
	mux.HandleFunc("/api/v1/maintenance/verify", s.handleMaintVerify)
	mux.HandleFunc("/api/v1/maintenance/duplicates", s.handleMaintDuplicates)
	mux.HandleFunc("/api/v1/maintenance/prune", s.handleMaintPrune)
	mux.HandleFunc("/api/v1/maintenance/backup", s.handleMaintBackup)
	mux.HandleFunc("/api/v1/photos/download_latest", s.handleDownloadLatest)
	mux.HandleFunc("/api/v1/photos/", s.handlePhotoByName)
	mux.HandleFunc("/img/", s.handleInline)
	mux.HandleFunc("/latest", s.handleLatest)
	mux.HandleFunc("/", s.handleRoot)
	return s.withCommonHeaders(mux)
}

// withCommonHeaders：CORS + 预检（现网行为）。
func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// 现网语义：GET /<key> 直接取字节（电视拉流就是这条路）
		s.serveKey(w, r, strings.TrimPrefix(r.URL.Path, "/"), false)
		return
	}
	infos, _ := s.store.List(store.ListOptions{})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": ServiceName,
		"dir":     s.store.Dir(),
		"files":   len(infos),
		"routes": map[string]string{
			"health":           "/health",
			"upload":           UploadPath,
			"list":             "/api/v1/assets",
			"meta":             "/api/v1/assets/<key>",
			"delete":           "DELETE /api/v1/assets/<key>",
			"static":           "/<key>",
			"static_img":       "/img/<key>",
			"download_by_name": "/api/v1/photos/<key>",
			"latest":           "/latest",
			"download_latest":  "/api/v1/photos/download_latest",
			"maintenance":      "/api/v1/maintenance/status",
			"verify":           "/api/v1/maintenance/verify",
			"duplicates":       "/api/v1/maintenance/duplicates",
			"prune":            "POST /api/v1/maintenance/prune",
			"backup":           "POST /api/v1/maintenance/backup",
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	counts, files, bytes, err := s.store.KindCounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 分类统计（只给出现过的 kind，避免每条 health 都带 7 个 0）
	kinds := map[string]any{}
	for _, k := range store.Kinds {
		if st, ok := counts[string(k)]; ok && st.Files > 0 {
			kinds[string(k)] = st
		}
	}
	entries, hashed, dupGroups := s.store.IndexStats()
	ms := s.maintenanceStatus()
	payload := map[string]any{
		"ok":           true,
		"service":      ServiceName,
		"dir":          s.store.Dir(),
		"files":        files,
		"bytes":        bytes,
		"kinds":        kinds,
		"public_base":  s.publicBase,
		"uptime_sec":   int(time.Since(s.startedAt).Seconds()),
		"index":        map[string]any{"entries": entries, "hashed": hashed, "duplicate_groups": dupGroups},
		"dedupe":       s.dedupe,
		"backup":       ms["backup"],
		"verify":       ms["verify"],
		"snapshots":    ms["snapshots"],
		"retention_on": s.retentionEnabled(),
	}
	writeJSON(w, http.StatusOK, payload)
}

// maintenanceStatus 作业状态（没有作业时给一个明确的 enabled=false 空壳）。
func (s *Server) maintenanceStatus() map[string]any {
	if s.jobs == nil {
		return map[string]any{
			"backup":    map[string]any{"enabled": false},
			"verify":    map[string]any{"enabled": false},
			"retention": map[string]any{"enabled": false},
			"snapshots": 0,
		}
	}
	return s.jobs.Status()
}

func (s *Server) retentionEnabled() bool {
	if s.jobs == nil {
		return false
	}
	enabled, _ := s.maintenanceStatus()["retention_enabled"].(bool)
	if enabled {
		return true
	}
	st, ok := s.maintenanceStatus()["retention"].(map[string]any)
	if !ok {
		return false
	}
	b, _ := st["enabled"].(bool)
	return b
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	if r.ContentLength > MaxUploadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "file too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	if err := r.ParseMultipartForm(MaxUploadBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": `expected multipart/form-data with field name "file"`,
		})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": `expected multipart/form-data with field name "file"`,
		})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxUploadBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "empty file"})
		return
	}
	name := "asset.bin"
	if header != nil {
		name = header.Filename
	}
	source := strings.TrimSpace(r.FormValue("source")) // 可选：调用方标记来源（审计/分类）
	dedupe := s.dedupe
	if raw := strings.TrimSpace(r.URL.Query().Get("dedupe")); raw != "" {
		dedupe = raw != "0" && !strings.EqualFold(raw, "false")
	}
	info, err := s.store.PutWithOptions(name, data, store.PutOptions{Dedupe: dedupe, Source: source})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if info.Deduped {
		s.logger.Info("upload deduped", "key", info.Key, "bytes", info.Bytes, "sha256", info.SHA256, "remote", r.RemoteAddr)
	} else {
		s.logger.Info("upload ok", "key", info.Key, "bytes", info.Bytes, "kind", info.Kind, "remote", r.RemoteAddr)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"filename": store.SanitizeName(name),
		"saved_as": info.Key, // 现网字段名：Brain 存 storage.key/saved_as 都用它
		"key":      info.Key,
		"bytes":    info.Bytes,
		"kind":     info.Kind,
		"sha256":   info.SHA256,
		"deduped":  info.Deduped,
		"path":     s.store.Dir() + "/" + info.Key,
		"url":      s.publicURL(info.Key),
	})
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}
	kind := store.Kind(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind"))))
	if kind != "" && !validKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "unknown kind", "kinds": kindNames(),
		})
		return
	}
	infos, err := s.store.List(store.ListOptions{
		Limit:    limit,
		Offset:   offset,
		MimeType: strings.TrimSpace(r.URL.Query().Get("mime")),
		Kind:     kind,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var total int64
	for _, i := range infos {
		total += i.Bytes
	}
	counts, allFiles, allBytes, _ := s.store.KindCounts()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"dir":         s.store.Dir(),
		"public_base": s.publicBase,
		"count":       len(infos), // 本次返回条数
		"bytes":       total,      // 本次返回条数的体积
		"offset":      offset,
		"total_files": allFiles, // 全库数量（分类统计口径）
		"total_bytes": allBytes,
		"kind_counts": counts, // 全库按类型分布
		"assets":      infos,
	})
}

func (s *Server) handleAssetOne(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/assets/")
	switch r.Method {
	case http.MethodGet:
		info, err := s.store.Stat(key)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "asset not found"})
			return
		}
		if info.SHA256 == "" {
			if sum, err := s.store.SHA256(key); err == nil {
				info.SHA256 = sum
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    true,
			"asset": info,
			"url":   s.publicURL(info.Key),
		})
	case http.MethodDelete:
		info, err := s.store.Delete(key)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "asset not found"})
			return
		}
		s.logger.Warn("asset deleted", "key", info.Key, "bytes", info.Bytes, "kind", info.Kind, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"deleted": info.Key,
			"bytes":   info.Bytes,
			"kind":    info.Kind,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET or DELETE required"})
	}
}

func (s *Server) handleInline(w http.ResponseWriter, r *http.Request) {
	s.serveKey(w, r, strings.TrimPrefix(r.URL.Path, "/img/"), false)
}

func (s *Server) handlePhotoByName(w http.ResponseWriter, r *http.Request) {
	s.serveKey(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/photos/"), true)
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	s.serveLatest(w, r, false)
}

func (s *Server) handleDownloadLatest(w http.ResponseWriter, r *http.Request) {
	s.serveLatest(w, r, true)
}

func (s *Server) serveLatest(w http.ResponseWriter, r *http.Request, asAttachment bool) {
	key, ok := s.store.LatestImage()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no photos on server"})
		return
	}
	s.serveKey(w, r, key, asAttachment)
}

// serveKey：取字节。用 http.ServeContent → 自动支持 Range/206 与 If-Modified-Since
// （DLNA 播放器/电视更友好；现网实现恒 200，是本 Hub 的一处改进）。
func (s *Server) serveKey(w http.ResponseWriter, r *http.Request, key string, asAttachment bool) {
	f, info, err := s.store.Open(key)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	defer f.Close()
	if asAttachment {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	}
	w.Header().Set("Cache-Control", noStoreHeader)
	w.Header().Set("Pragma", "no-cache")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (s *Server) publicURL(key string) string {
	if s.publicBase == "" {
		return "/" + key
	}
	return s.publicBase + "/" + key
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", noStoreHeader)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// PublicBaseFromEnv / StoreDirFromEnv 之类的环境解析放在 cmd 层；这里只保证
// 「未显式给 public base」时用请求 Host 兜底（与现网 MAC_EDGE_LAN_PHOTO_PUBLIC_BASE 语义一致）。
func PublicBaseFromRequest(r *http.Request) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return "http://" + host
}

var _ = os.Getenv
