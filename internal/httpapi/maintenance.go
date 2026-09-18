package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

// validKind / kindNames：kind 参数校验（避免 typo 静默返回空清单）。
func validKind(k store.Kind) bool {
	for _, known := range store.Kinds {
		if k == known {
			return true
		}
	}
	return false
}

func kindNames() []string {
	out := make([]string, 0, len(store.Kinds))
	for _, k := range store.Kinds {
		out = append(out, string(k))
	}
	return out
}

func (s *Server) requireJobs(w http.ResponseWriter) bool {
	if s.jobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "maintenance jobs not enabled",
		})
		return false
	}
	return true
}

// handleMaintStatus 资源管理全貌：数量/体积/分类 + sha256 索引 + 备份/保留策略状态。
func (s *Server) handleMaintStatus(w http.ResponseWriter, r *http.Request) {
	counts, files, bytes, err := s.store.KindCounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	entries, hashed, dupGroups := s.store.IndexStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"service":          ServiceName,
		"dir":              s.store.Dir(),
		"index_path":       s.store.IndexPath(),
		"files":            files,
		"bytes":            bytes,
		"kind_counts":      counts,
		"kinds_in_use":     s.store.KindsInUse(),
		"index":            map[string]any{"entries": entries, "hashed": hashed, "duplicate_groups": dupGroups},
		"dedupe":           s.dedupe,
		"jobs":             s.maintenanceStatus(),
		"max_upload_bytes": MaxUploadBytes,
	})
}

// handleMaintVerify 巡检：重算 sha256 与索引比对，顺带补建缺的索引（自愈）。
func (s *Server) handleMaintVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET or POST required"})
		return
	}
	if !s.requireJobs(w) {
		return
	}
	quick := r.URL.Query().Get("quick") == "1" || strings.EqualFold(r.URL.Query().Get("quick"), "true")
	s.maintMu.Lock() // 与 prune 串行：别一边删一边算 hash
	rep, err := s.jobs.VerifyNow(r.Context(), quick)
	s.maintMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.logger.Info("verify done", "files", rep.Files, "problems", len(rep.Problems),
		"not_indexed", rep.NotIndexed, "took_ms", rep.TookMS, "quick", rep.Quick)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verify": rep})
}

// handleMaintDuplicates 内容重复组（同 sha256 多个 key）+ 可省空间。
func (s *Server) handleMaintDuplicates(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.Duplicates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var slack int64
	for _, g := range groups {
		slack += g.Slack
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"groups":      groups,
		"group_count": len(groups),
		"slack_bytes": slack,
		"note":        "同内容多份（历史重复上传）。删其中一份前先确认 Brain 的 assets 引用；上传去重见 dedupe 开关。",
	})
}

// parseDurationOrSeconds：接受 "720h"/"30d"（d 由本函数折算）/纯秒数。
func parseDurationOrSeconds(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil || n < 0 {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	if n, err := strconv.Atoi(raw); err == nil { // 纯数字=秒
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

func queryBool(r *http.Request, key string, def bool) bool {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	return raw != "0" && !strings.EqualFold(raw, "false") && !strings.EqualFold(raw, "no")
}

// handleMaintPrune 清理：默认 **dry-run**（只报候选）。
//
//	POST /api/v1/maintenance/prune?older_than=720h&kind=pdf&max=200&dry_run=1
//	POST /api/v1/maintenance/prune?keys=a.pdf,b.jpg&dry_run=0
//
// 真删必须显式 dry_run=0 —— 删掉的文件 Brain 的 assets 引用不会跟着清，
// 所以默认把「看候选」和「下手」分开。
func (s *Server) handleMaintPrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	if !s.requireJobs(w) {
		return
	}
	dryRun := queryBool(r, "dry_run", true)
	opts := store.PruneOptions{DryRun: dryRun}
	kind := store.Kind(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind"))))
	if kind != "" {
		if !validKind(kind) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown kind", "kinds": kindNames()})
			return
		}
		opts.Kind = kind
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("older_than")); raw != "" {
		d, ok := parseDurationOrSeconds(raw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid older_than（示例 720h / 30d / 秒数）"})
			return
		}
		opts.OlderThan = d
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("older_than_days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid older_than_days"})
			return
		}
		opts.OlderThan = time.Duration(n) * 24 * time.Hour
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("keys")); raw != "" {
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				opts.Keys = append(opts.Keys, k)
			}
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("max")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid max"})
			return
		}
		opts.Max = n
	}
	// 参数校验放在调作业之前：请求本身不合法（没给目标）不该被记成「保留策略失败」。
	// 注意：kind 只是过滤器 —— 单给 kind 会把这类全删，必须配时间窗或点名 keys。
	if opts.OlderThan <= 0 && len(opts.Keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "需要明确目标：older_than>0（或 older_than_days）或 keys；kind 只能作为过滤器",
		})
		return
	}
	s.maintMu.Lock()
	rep, err := s.jobs.PruneNow(opts)
	s.maintMu.Unlock()
	if err != nil {
		code := http.StatusInternalServerError
		if err == store.ErrPruneNeedsTarget {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !rep.DryRun {
		for _, item := range rep.Deleted {
			s.logger.Warn("prune deleted asset", "key", item.Key, "bytes", item.Bytes, "kind", item.Kind)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "prune": rep})
}

// handleMaintBackup 立刻做一份 rsync 快照（scripts/backup.sh 走这里，逻辑只在 Go 里一份）。
func (s *Server) handleMaintBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST required"})
		return
	}
	if !s.requireJobs(w) {
		return
	}
	detail, err := s.jobs.BackupNow(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backup": detail, "jobs": s.maintenanceStatus()})
}
