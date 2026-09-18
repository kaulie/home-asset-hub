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
//	GET /api/v1/assets                清单（?limit=&mime=），按修改时间倒序
//	GET /api/v1/assets/{key}          单条元数据
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// Server 组装路由。publicBase 仅用于上传响应里的 url（现网语义）。
type Server struct {
	store      *store.Store
	publicBase string
	logger     *slog.Logger
	startedAt  time.Time
}

func New(st *store.Store, publicBase string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:      st,
		publicBase: strings.TrimRight(strings.TrimSpace(publicBase), "/"),
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
			"static":           "/<key>",
			"static_img":       "/img/<key>",
			"download_by_name": "/api/v1/photos/<key>",
			"latest":           "/latest",
			"download_latest":  "/api/v1/photos/download_latest",
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.List(store.ListOptions{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var total int64
	for _, i := range infos {
		total += i.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"service":     ServiceName,
		"dir":         s.store.Dir(),
		"files":       len(infos),
		"bytes":       total,
		"public_base": s.publicBase,
		"uptime_sec":  int(time.Since(s.startedAt).Seconds()),
	})
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
	info, err := s.store.Put(name, data)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.logger.Info("upload ok", "key", info.Key, "bytes", info.Bytes, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"filename": store.SanitizeName(name),
		"saved_as": info.Key, // 现网字段名：Brain 存 storage.key/saved_as 都用它
		"key":      info.Key,
		"bytes":    info.Bytes,
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
	infos, err := s.store.List(store.ListOptions{
		Limit:    limit,
		MimeType: strings.TrimSpace(r.URL.Query().Get("mime")),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var total int64
	for _, i := range infos {
		total += i.Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"dir":         s.store.Dir(),
		"public_base": s.publicBase,
		"count":       len(infos),
		"bytes":       total,
		"assets":      infos,
	})
}

func (s *Server) handleAssetOne(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/v1/assets/")
	info, err := s.store.Stat(key)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "asset not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"asset": info,
		"url":   s.publicURL(info.Key),
	})
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
