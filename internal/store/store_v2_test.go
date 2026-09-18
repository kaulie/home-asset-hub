package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKindOfClassifiesFamilyResources(t *testing.T) {
	cases := map[string]Kind{
		"image/jpeg":               KindImage,
		"image/heic":               KindImage,
		"audio/mpeg":               KindAudio,
		"audio/mp4":                KindAudio,
		"video/mp4":                KindVideo,
		"video/quicktime":          KindVideo,
		"application/pdf":          KindPDF,
		"text/markdown":            KindText,
		"text/plain":               KindText,
		"application/json":         KindText,
		"application/zip":          KindDocument,
		"application/octet-stream": KindOther,
		"":                         KindOther,
	}
	for mime, want := range cases {
		if got := KindOf(mime); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", mime, got, want)
		}
	}
	// 扩展名 → kind 走显式映射（不依赖宿主机 mime.types，跨机器一致）
	for name, want := range map[string]Kind{
		"a.JPG": KindImage, "p.pdf": KindPDF, "p.PDF": KindPDF,
		"m.mp3": KindAudio, "v.mov": KindVideo, "n.md": KindText,
		"x.docx": KindDocument, "weird.xyz": KindOther,
	} {
		if got := KindOf(mimeTypeOf(name)); got != want {
			t.Errorf("kind(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestPutRecordsSHA256AndKind(t *testing.T) {
	st, _ := New(t.TempDir())
	info, err := st.Put("paper.pdf", []byte("pdf-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != KindPDF || info.SHA256 == "" || info.Created == "" {
		t.Fatalf("info = %+v", info)
	}
	// 现网 key 形态不变：<8hex>_<原名>
	if len(strings.SplitN(info.Key, "_", 2)[0]) != 8 {
		t.Fatalf("key shape = %q", info.Key)
	}
	if _, err := os.Stat(st.IndexPath()); err != nil {
		t.Fatalf("index file missing: %v", err)
	}
	// 重启（重新打开）后索引仍在
	reopened, _ := New(st.Dir())
	again, err := reopened.Stat(info.Key)
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != info.SHA256 || again.Kind != KindPDF {
		t.Fatalf("reopened info = %+v", again)
	}
}

func TestDedupeReusesKeyAndTouchesModified(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	body := []byte("same-bytes")
	first, err := st.PutWithOptions("album.jpg", body, PutOptions{Dedupe: true, Source: "ios"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, first.Key), old, old); err != nil {
		t.Fatal(err)
	}
	second, err := st.PutWithOptions("album-again.jpg", body, PutOptions{Dedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduped || second.Key != first.Key {
		t.Fatalf("dedupe miss: first=%+v second=%+v", first, second)
	}
	infos, _ := st.List(ListOptions{})
	if len(infos) != 1 {
		t.Fatalf("files = %d, want 1（只该有一份字节）", len(infos))
	}
	// mtime 被顶到当前：否则「重传同一张图 → 变成最新」的现网用法会失效
	ts, err := time.Parse(time.RFC3339, second.Modified)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(ts) > time.Minute {
		t.Fatalf("modified = %s（重传应把 mtime 顶新）", second.Modified)
	}
	if second.Source != "ios" {
		t.Fatalf("source = %q, want 保留首次来源", second.Source)
	}
	// 关掉去重 → 写第二份
	third, err := st.PutWithOptions("album.jpg", body, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if third.Deduped || third.Key == first.Key {
		t.Fatalf("去重关闭时不该复用：%+v", third)
	}
}

func TestVerifyCatchesTamperingAndBuildsIndex(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	kept, err := st.Put("ok.pdf", []byte("good-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := st.Put("tampered.pdf", []byte("original-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	// 外部改内容（同大小：只有比 sha256 才看得出来）
	if err := os.WriteFile(filepath.Join(dir, tampered.Key), []byte("original-XXXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 外部新增一个没索引的文件
	if err := os.WriteFile(filepath.Join(dir, "ghost_manual.pdf"), []byte("ghost"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 外部删掉一个索引里有的文件
	vanish, err := st.Put("vanish.pdf", []byte("bye"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, vanish.Key)); err != nil {
		t.Fatal(err)
	}

	rep, err := st.Verify(VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatalf("should not be OK: %+v", rep)
	}
	statuses := map[string]int{}
	for _, p := range rep.Problems {
		statuses[p.Status]++
		if p.Key == tampered.Key && p.Status != "hash_mismatch" {
			t.Fatalf("tampered problem = %+v", p)
		}
	}
	if statuses["hash_mismatch"] != 1 || statuses["orphan_index"] != 1 {
		t.Fatalf("problems = %+v (rep=%+v)", rep.Problems, rep)
	}
	if rep.NotIndexed != 1 || rep.Repaired != 2 { // ghost 补索引 + tampered 修索引
		t.Fatalf("not_indexed=%d repaired=%d（应补 ghost、修 tampered）", rep.NotIndexed, rep.Repaired)
	}
	// 补/修之后索引与磁盘一致（只剩孤行仍要报）
	rep2, err := st.Verify(VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep2.Problems {
		if p.Status != "orphan_index" {
			t.Fatalf("二次巡检还有没修好的问题：%+v", p)
		}
	}
	if sum, err := st.SHA256(kept.Key); err != nil || sum != kept.SHA256 {
		t.Fatalf("kept sha256 = %q err=%v", sum, err)
	}
}

func TestDuplicatesGroupsSameContent(t *testing.T) {
	st, _ := New(t.TempDir())
	a, _ := st.Put("one.jpg", []byte("dupe-bytes"))
	b, _ := st.Put("two.jpg", []byte("dupe-bytes"))
	if _, err := st.Put("solo.jpg", []byte("unique")); err != nil {
		t.Fatal(err)
	}
	groups, err := st.Duplicates()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Keys) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Slack != int64(len("dupe-bytes")) {
		t.Fatalf("slack = %d", groups[0].Slack)
	}
	keys := strings.Join(groups[0].Keys, ",")
	if !strings.Contains(keys, a.Key) || !strings.Contains(keys, b.Key) {
		t.Fatalf("keys = %v", groups[0].Keys)
	}
}

func TestDeleteAndPruneSemantics(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	old, err := st.Put("old.pdf", []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put("fresh.pdf", []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	// 把 old 的 mtime 拨到 40 天前
	past := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, old.Key), past, past); err != nil {
		t.Fatal(err)
	}

	// 没给目标 → 拒绝（避免「删空目录」）
	if _, err := st.Prune(PruneOptions{}); err != ErrPruneNeedsTarget {
		t.Fatalf("err = %v", err)
	}
	// dry-run：只报候选，不动文件
	rep, err := st.Prune(PruneOptions{OlderThan: 30 * 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Candidates) != 1 || rep.Candidates[0].Key != old.Key || len(rep.Deleted) != 0 {
		t.Fatalf("dry-run rep = %+v", rep)
	}
	// Max 截断 + 点名 key（keys 忽略时间窗）
	before, _ := st.List(ListOptions{})
	named := []string{before[0].Key, before[1].Key}
	if rep, _ := st.Prune(PruneOptions{Keys: named, DryRun: true, Max: 1}); rep.Truncated != 1 || len(rep.Candidates) != 1 {
		t.Fatalf("named/max rep = %+v", rep)
	}
	// kind 只是过滤器：单给 kind（没有时间窗/点名）必须拒 —— 否则就是「删掉这类全部」
	if _, err := st.Prune(PruneOptions{Kind: KindPDF}); err != ErrPruneNeedsTarget {
		t.Fatalf("kind-only err = %v", err)
	}
	if rep, _ := st.Prune(PruneOptions{Kind: KindPDF, OlderThan: 30 * 24 * time.Hour, DryRun: true}); len(rep.Candidates) != 1 {
		t.Fatalf("kind 过滤候选 = %+v", rep)
	}
	// 真删：过期的那份走，fresh 留着
	rep, err = st.Prune(PruneOptions{OlderThan: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Deleted) != 1 || rep.FreedBytes != int64(len("old")) {
		t.Fatalf("prune rep = %+v", rep)
	}
	infos, _ := st.List(ListOptions{})
	if len(infos) != 1 || !strings.HasSuffix(infos[0].Key, "fresh.pdf") {
		t.Fatalf("after prune = %+v", infos)
	}
	if _, err := st.Delete(old.Key); err == nil {
		t.Fatal("再删应报 not found（墓碑不留活行）")
	}
	// 删干净后巡检应 OK（Delete 不制造 orphan_index）
	freshKey := infos[0].Key
	if _, err := st.Delete(freshKey); err != nil {
		t.Fatal(err)
	}
	rep2, err := st.Verify(VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.OK {
		t.Fatalf("删干净后巡检应 OK：%+v", rep2.Problems)
	}
	if _, _, dups := st.IndexStats(); dups != 0 {
		t.Fatalf("duplicate groups = %d", dups)
	}
}
