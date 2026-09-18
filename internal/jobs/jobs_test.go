package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

func newTestRunner(t *testing.T, cfg Config) (*Runner, *store.Store) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "img")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(st, cfg, nil), st
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && e.Name()[0] != '.' {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestSnapshotHardlinksKeepsN(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("本机没有 rsync")
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	r, st := newTestRunner(t, Config{BackupEnabled: true, BackupDir: backupDir, BackupKeep: 2})
	ctx := context.Background()
	keyInfo, err := st.Put("a.pdf", []byte("pdf-1"))
	if err != nil {
		t.Fatal(err)
	}
	key := keyInfo.Key

	first, err := r.BackupNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first["snapshot"] == "" || first["files"].(int) != 1 {
		t.Fatalf("first = %+v", first)
	}
	// 同一秒内再触发：目录名不能撞（否则 rename 失败）
	if _, err := r.BackupNow(ctx); err != nil {
		t.Fatalf("同秒第二次快照失败：%v", err)
	}
	snaps := r.snapshotDirs()
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %v", snaps)
	}
	// 内容没变的那份文件应是硬链接（--link-dest 的效果：不占额外空间）
	f1, err := os.Stat(filepath.Join(snaps[1], key))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.Stat(filepath.Join(snaps[0], key))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(f1, f2) {
		t.Fatalf("应硬链接到上一份快照（省空间）")
	}
	// 快照内容与源一致
	if got, err := os.ReadFile(filepath.Join(snaps[0], key)); err != nil || string(got) != "pdf-1" {
		t.Fatalf("snapshot content = %q err=%v", got, err)
	}

	// 源目录加一个文件、删一个文件 → 新快照反映当下，旧快照仍留得住（这就是快照的意义）
	if _, err := st.Put("b.mp3", []byte("audio")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(key); err != nil {
		t.Fatal(err)
	}
	third, err := r.BackupNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// keep=2 → 只剩最新两份；被删的 a.pdf 仍能从「第二份」里捞回来
	if third["snapshots"].(int) != 2 || len(third["pruned"].([]string)) != 1 {
		t.Fatalf("third = %+v", third)
	}
	kept := r.snapshotDirs()
	if len(kept) != 2 {
		t.Fatalf("kept = %v", kept)
	}
	var recovered bool
	for _, s := range kept {
		if _, err := os.Stat(filepath.Join(s, key)); err == nil {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("被删的 %s 应还能从保留的快照里恢复：%v", key, kept)
	}
	if len(filesIn(t, kept[0])) != 1 { // 最新快照里只有 b.mp3
		t.Fatalf("最新快照 = %v", filesIn(t, kept[0]))
	}
}

func TestSnapshotSkipsEmptySourceAndMissingDir(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("本机没有 rsync")
	}
	r, _ := newTestRunner(t, Config{BackupEnabled: true, BackupDir: filepath.Join(t.TempDir(), "backup")})
	detail, err := r.BackupNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if detail["skipped"] == nil {
		t.Fatalf("空源目录应跳过：%+v", detail)
	}
	if len(r.snapshotDirs()) != 0 {
		t.Fatalf("不该生成空快照")
	}
	// 没配备份目录 → 明确报错（不静默）
	r2, _ := newTestRunner(t, Config{BackupEnabled: true, BackupDir: "  "})
	if _, err := r2.BackupNow(context.Background()); err == nil {
		t.Fatal("未配置备份目录应报错")
	}
}

func TestPruneSnapshotsKeepsNewest(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// .partial / 隐藏目录不算快照
	if err := os.MkdirAll(filepath.Join(root, "20260104-000000.partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	kept, pruned := pruneSnapshots(root, 1)
	if kept != 1 || len(pruned) != 2 {
		t.Fatalf("kept=%d pruned=%v", kept, pruned)
	}
	if _, err := os.Stat(filepath.Join(root, "20260103-000000")); err != nil {
		t.Fatalf("最新一份必须留着：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "20260104-000000.partial")); err != nil {
		t.Fatalf(".partial 不该被当快照处理：%v", err)
	}
}

func TestVerifyNowRecordsStateAndRetentionRuns(t *testing.T) {
	r, st := newTestRunner(t, Config{RetentionEnabled: true, RetentionDays: 30, RetentionMaxPerRun: 10})
	keep, err := st.Put("keep.pdf", []byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.VerifyNow(context.Background(), true)
	if err != nil || !rep.OK {
		t.Fatalf("verify = %+v err=%v", rep, err)
	}
	status := r.Status()
	verify, ok := status["verify"].(State)
	if !ok || verify.Runs != 1 || !verify.LastOK {
		t.Fatalf("status.verify = %+v", status["verify"])
	}

	// 过期资源（mtime 拨老）→ 保留策略真删
	oldInfo, err := st.Put("old.pdf", []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	key := oldInfo.Key
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(st.Dir(), key), past, past); err != nil {
		t.Fatal(err)
	}
	r.runRetention(context.Background())
	status = r.Status()
	retention, ok := status["retention"].(State)
	if !ok || !retention.LastOK {
		t.Fatalf("retention = %+v", status["retention"])
	}
	if retention.Detail["deleted"].(int) != 1 {
		t.Fatalf("retention detail = %+v", retention.Detail)
	}
	infos, _ := st.List(store.ListOptions{})
	if len(infos) != 1 || infos[0].Key != keep.Key {
		t.Fatalf("保留策略后应只剩 %s：%+v", keep.Key, infos)
	}
}
