// Package jobs：资源托管服务器的后台作业 ——
//
//	backup     定时 rsync 快照备份（硬链接增量，保留 N 份）
//	verify     启动补建 sha256 索引 + 手动/定时巡检（内容被动过能查出来）
//	retention  （可选，默认关）按保留策略真删过期资源
//
// 设计取舍：
//   - 备份用 rsync --link-dest 做「快照」而不是简单镜像：镜像是「删了就没了」，
//     快照是「每个时间点都还查得到」，误删/误覆盖能回到上一份。
//   - 源目录为空时**跳过**，不生成空快照 —— 配错目录不至于把历史快照的语义搞乱。
//   - rsync 先写 <stamp>.partial、成功再 rename：半成品不会被当成可用快照。
//   - 保留策略默认关（ASSET_HUB_RETENTION_DAYS=0）。它会**真删**文件，而 Brain 的
//     assets 引用不会跟着清，所以必须先看 prune 的 dry-run 候选再决定开不开。
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

// Config 后台作业配置（全部来自环境变量，见 cmd/assethub/main.go）。
type Config struct {
	BackupEnabled  bool
	BackupDir      string
	BackupInterval time.Duration
	BackupKeep     int

	RetentionEnabled   bool
	RetentionDays      int
	RetentionInterval  time.Duration
	RetentionMaxPerRun int

	IndexBuildOnStart bool // 启动时若索引不全 → 后台补建（v1 老文件第一次开机就补上）
	StartDelay        time.Duration
}

// State 一个作业的状态（挂到 /health 与 /api/v1/maintenance/status）。
type State struct {
	Enabled   bool           `json:"enabled"`
	Runs      int            `json:"runs"`
	LastAt    string         `json:"last_at,omitempty"`
	LastOK    bool           `json:"last_ok"`
	LastError string         `json:"last_error,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Runner 后台作业宿主。
type Runner struct {
	cfg    Config
	st     *store.Store
	logger *slog.Logger

	mu        sync.Mutex
	snapMu    sync.Mutex // 快照串行化（并发触发时别抢同一份 .partial）
	backup    State
	verify    State
	retention State
}

// New 组装（不启动）。
func New(st *store.Store, cfg Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.StartDelay <= 0 {
		cfg.StartDelay = 5 * time.Second
	}
	return &Runner{
		cfg:       cfg,
		st:        st,
		logger:    logger,
		backup:    State{Enabled: cfg.BackupEnabled},
		verify:    State{Enabled: cfg.IndexBuildOnStart},
		retention: State{Enabled: cfg.RetentionEnabled},
	}
}

// Start 启动后台循环（非阻塞；ctx 取消即退出）。
func (r *Runner) Start(ctx context.Context) {
	first := func() {
		if r.cfg.IndexBuildOnStart {
			if rep, err := r.VerifyNow(ctx, false); err != nil {
				r.logger.Warn("startup index build failed", "err", err)
			} else {
				r.logger.Info("startup index build done",
					"files", rep.Files, "not_indexed", rep.NotIndexed,
					"problems", len(rep.Problems), "took_ms", rep.TookMS)
			}
		}
		if r.cfg.BackupEnabled {
			r.runBackup(ctx)
		}
		if r.cfg.RetentionEnabled {
			r.runRetention(ctx)
		}
	}
	if r.cfg.IndexBuildOnStart || r.cfg.BackupEnabled || r.cfg.RetentionEnabled {
		go func() {
			// 开机先等几秒：让 /health 先就绪，平台健康检查不被重活挡住
			if !sleepCtx(ctx, r.cfg.StartDelay) {
				return
			}
			first()
		}()
	}
	if r.cfg.BackupEnabled {
		go r.loop(ctx, "backup", r.cfg.BackupInterval, r.runBackup)
	}
	if r.cfg.RetentionEnabled {
		go r.loop(ctx, "retention", r.cfg.RetentionInterval, r.runRetention)
	}
}

func (r *Runner) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.logger.Info("job loop started", "job", name, "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Status 全部作业状态 + 快照目录现状（/health 与维护接口用）。
func (r *Runner) Status() map[string]any {
	r.mu.Lock()
	backup, verify, retention := r.backup, r.verify, r.retention
	r.mu.Unlock()
	snaps := r.snapshotDirs()
	return map[string]any{
		"backup":            backup,
		"verify":            verify,
		"retention":         retention,
		"backup_dir":        r.cfg.BackupDir,
		"backup_interval":   r.cfg.BackupInterval.String(),
		"backup_keep":       r.cfg.BackupKeep,
		"snapshots":         len(snaps),
		"latest_snapshot":   latestOf(snaps),
		"retention_days":    r.cfg.RetentionDays,
		"retention_enabled": retention.Enabled,
	}
}

func (r *Runner) recordBackup(detail map[string]any, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backup.Runs++
	r.backup.LastAt = time.Now().Format(time.RFC3339)
	r.backup.LastOK = err == nil
	r.backup.Detail = detail
	if err == nil {
		r.backup.LastError = ""
	} else {
		r.backup.LastError = err.Error()
	}
}

// VerifyNow 巡检（并在需要时补建索引）。quick=true 只比大小、不重算 sha256。
func (r *Runner) VerifyNow(_ context.Context, quick bool) (store.VerifyReport, error) {
	rep, err := r.st.Verify(store.VerifyOptions{Quick: quick})
	detail := map[string]any{
		"dir": rep.Dir, "files": rep.Files, "indexed": rep.Indexed, "hashed": rep.Hashed,
		"not_indexed": rep.NotIndexed, "repaired": rep.Repaired,
		"problems": len(rep.Problems), "took_ms": rep.TookMS, "quick": rep.Quick,
	}
	if n := len(rep.Problems); n > 0 {
		head := rep.Problems
		if n > 20 {
			head = head[:20]
		}
		detail["problem_head"] = head
	}
	r.mu.Lock()
	r.verify.Runs++
	r.verify.LastAt = time.Now().Format(time.RFC3339)
	r.verify.LastOK = err == nil && rep.OK
	r.verify.Detail = detail
	if err == nil {
		r.verify.LastError = ""
	} else {
		r.verify.LastError = err.Error()
	}
	r.mu.Unlock()
	return rep, err
}

// recordRetention 记录一次保留策略执行的结果。
func (r *Runner) recordRetention(detail map[string]any, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retention.Runs++
	r.retention.LastAt = time.Now().Format(time.RFC3339)
	r.retention.LastOK = err == nil
	r.retention.Detail = detail
	if err == nil {
		r.retention.LastError = ""
	} else {
		r.retention.LastError = err.Error()
	}
}

// PruneNow 按保留策略清理（DryRun 由调用方给）。
func (r *Runner) PruneNow(opts store.PruneOptions) (store.PruneReport, error) {
	rep, err := r.st.Prune(opts)
	r.recordRetention(map[string]any{
		"dry_run": rep.DryRun, "candidates": len(rep.Candidates), "deleted": len(rep.Deleted),
		"freed_bytes": rep.FreedBytes, "older_than": rep.OlderThan, "kind": rep.Kind,
		"truncated": rep.Truncated,
	}, err)
	return rep, err
}

func (r *Runner) runRetention(ctx context.Context) {
	if r.cfg.RetentionDays <= 0 {
		return
	}
	_ = ctx
	rep, err := r.PruneNow(store.PruneOptions{
		OlderThan: time.Duration(r.cfg.RetentionDays) * 24 * time.Hour,
		Max:       r.cfg.RetentionMaxPerRun,
	})
	if err != nil {
		r.logger.Warn("retention prune failed", "err", err)
		return
	}
	for _, item := range rep.Deleted {
		// 真删过的东西必须留痕：Brain 的 assets 引用不会跟着清。
		r.logger.Warn("retention deleted asset",
			"key", item.Key, "bytes", item.Bytes, "kind", item.Kind, "modified", item.Modified)
	}
	r.logger.Info("retention done", "deleted", len(rep.Deleted), "freed_bytes", rep.FreedBytes,
		"older_than", rep.OlderThan, "max_per_run", r.cfg.RetentionMaxPerRun)
}

// BackupNow 立刻做一份快照（/api/v1/maintenance/backup 与 scripts/backup.sh 用）。
func (r *Runner) BackupNow(ctx context.Context) (map[string]any, error) {
	detail, err := r.snapshot(ctx)
	r.recordBackup(detail, err)
	return detail, err
}

func (r *Runner) runBackup(ctx context.Context) {
	detail, err := r.snapshot(ctx)
	r.recordBackup(detail, err)
	if err != nil {
		r.logger.Warn("backup failed", "err", err)
		return
	}
	r.logger.Info("backup done",
		"snapshot", detail["snapshot"], "files", detail["files"], "bytes", detail["bytes"],
		"snapshots", detail["snapshots"], "pruned", detail["pruned"], "took_ms", detail["took_ms"])
}

// snapshot 一次快照：rsync -a --delete --link-dest=<上一份> src/ <stamp>.partial/ → rename。
func (r *Runner) snapshot(ctx context.Context) (map[string]any, error) {
	r.snapMu.Lock() // 串行化：并发触发（开机 + 手动）不抢同一份 .partial
	defer r.snapMu.Unlock()
	if strings.TrimSpace(r.cfg.BackupDir) == "" {
		return nil, errors.New("未配置备份目录（ASSET_HUB_BACKUP_DIR）")
	}
	infos, err := r.st.List(store.ListOptions{})
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return map[string]any{"skipped": "源目录为空，跳过（不生成空快照）"}, nil
	}
	rsync, err := exec.LookPath("rsync")
	if err != nil {
		return nil, fmt.Errorf("rsync 不可用：%w", err)
	}
	root := filepath.Join(r.cfg.BackupDir, "snapshots")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	final := uniqueSnapshotPath(root, time.Now().Format("20060102-150405"))
	partial := final + ".partial"
	_ = os.RemoveAll(partial)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return nil, err
	}
	args := []string{"-a", "--delete", "--numeric-ids"}
	if prev := latestOf(r.snapshotDirs()); prev != "" {
		args = append(args, "--link-dest="+prev)
	}
	args = append(args, r.st.Dir()+"/", partial+"/")
	started := time.Now()
	out, err := exec.CommandContext(ctx, rsync, args...).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(partial) // 半成品不留：否则会被当成一份可用快照
		return nil, fmt.Errorf("rsync 失败：%v：%s", err, tailText(string(out), 300))
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.RemoveAll(partial)
		return nil, err
	}
	var bytes int64
	for _, info := range infos {
		bytes += info.Bytes
	}
	kept, pruned := pruneSnapshots(root, r.cfg.BackupKeep)
	if pruned == nil {
		pruned = []string{}
	}
	return map[string]any{
		"snapshot": final, "files": len(infos), "bytes": bytes,
		"took_ms":   time.Since(started).Milliseconds(),
		"snapshots": kept, "pruned": pruned, "keep": r.cfg.BackupKeep,
	}, nil
}

// snapshotDirs 现有快照目录（跳过 .partial/隐藏），按名字倒序（时间戳字典序=时间序）。
func (r *Runner) snapshotDirs() []string {
	root := filepath.Join(r.cfg.BackupDir, "snapshots")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(root, e.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// pruneSnapshots 只保留最新 keep 份（keep<=0 视为 1：永远留最后一份好的）。
func pruneSnapshots(root string, keep int) (kept int, pruned []string) {
	if keep <= 0 {
		keep = 1
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".partial") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, name := range names {
		path := filepath.Join(root, name)
		if i < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			pruned = append(pruned, path)
		}
	}
	return kept, pruned
}

// uniqueSnapshotPath：同一秒内连续触发两次快照时，目录名加 -2/-3 后缀（时间戳字典序仍是时间序）。
func uniqueSnapshotPath(root, stamp string) string {
	candidate := filepath.Join(root, stamp)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}
	for i := 2; i < 1000; i++ {
		candidate = filepath.Join(root, fmt.Sprintf("%s-%d", stamp, i))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return filepath.Join(root, fmt.Sprintf("%s-%d", stamp, time.Now().UnixNano()))
}

func latestOf(paths []string) string {
	best := ""
	for _, p := range paths {
		if p > best {
			best = p
		}
	}
	return best
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
