package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

// fakeJobs 记录调用（维护接口的形状与参数传递靠它验证）。
type fakeJobs struct {
	mu       sync.Mutex
	backups  int
	verifies int
	prune    []store.PruneOptions
}

func (f *fakeJobs) Status() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"backup":            map[string]any{"enabled": true, "runs": f.backups, "last_ok": true},
		"verify":            map[string]any{"enabled": true, "runs": f.verifies},
		"retention":         map[string]any{"enabled": false},
		"snapshots":         2,
		"backup_dir":        "/tmp/backup",
		"backup_interval":   "24h0m0s",
		"backup_keep":       7,
		"retention_days":    0,
		"retention_enabled": false,
	}
}

func (f *fakeJobs) BackupNow(_ context.Context) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backups++
	return map[string]any{"snapshot": "/tmp/backup/snapshots/20260918-120000", "files": 3}, nil
}

func (f *fakeJobs) VerifyNow(_ context.Context, quick bool) (store.VerifyReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	return store.VerifyReport{OK: true, Files: 2, Indexed: 2, Quick: quick, Problems: []store.VerifyProblem{}}, nil
}

func (f *fakeJobs) PruneNow(opts store.PruneOptions) (store.PruneReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prune = append(f.prune, opts)
	return store.PruneReport{
		DryRun:     opts.DryRun,
		OlderThan:  opts.OlderThan.String(),
		Candidates: []store.PruneItem{{Key: "old.pdf", Bytes: 3, Kind: store.KindPDF}},
	}, nil
}

func (f *fakeJobs) lastPrune() store.PruneOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prune) == 0 {
		return store.PruneOptions{}
	}
	return f.prune[len(f.prune)-1]
}

func newTestServerWithJobs(t *testing.T, opts Options) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewWithOptions(st, opts, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func postJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAssetListKindFilterAndKindCounts(t *testing.T) {
	ts, _ := newTestServerWithJobs(t, Options{})
	upload(t, ts, "a.mp3", []byte("audio"))
	upload(t, ts, "b.pdf", []byte("pdf"))
	upload(t, ts, "c.pdf", []byte("pdf2"))

	code, body := getJSON(t, ts.URL+"/api/v1/assets?kind=pdf")
	if code != http.StatusOK {
		t.Fatalf("code = %d body=%v", code, body)
	}
	if body["count"].(float64) != 2 || body["total_files"].(float64) != 3 {
		t.Fatalf("count/total = %v/%v", body["count"], body["total_files"])
	}
	counts := body["kind_counts"].(map[string]any)
	if counts["pdf"].(map[string]any)["files"].(float64) != 2 {
		t.Fatalf("kind_counts = %v", counts)
	}
	// 分页：offset
	if _, paged := getJSON(t, ts.URL+"/api/v1/assets?kind=pdf&limit=1&offset=1"); paged["count"].(float64) != 1 {
		t.Fatalf("paged = %v", paged)
	}
	// 未知 kind 要报错（不能静默给空清单）
	if code, _ := getJSON(t, ts.URL+"/api/v1/assets?kind=movies"); code != http.StatusBadRequest {
		t.Fatalf("bad kind code = %d", code)
	}
}

func TestDeleteAsset(t *testing.T) {
	ts, _ := newTestServerWithJobs(t, Options{})
	key := upload(t, ts, "gone.jpg", []byte("bytes"))["saved_as"].(string)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/assets/"+key, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete code = %d", resp.StatusCode)
	}
	if code, _ := getJSON(t, ts.URL+"/api/v1/assets/"+key); code != http.StatusNotFound {
		t.Fatalf("after delete code = %d", code)
	}
	// 再删一次还是 404
	if resp2, _ := http.DefaultClient.Do(req); resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete code = %d", resp2.StatusCode)
	}
}

func TestHealthHasKindsAndIndex(t *testing.T) {
	jobs := &fakeJobs{}
	ts, _ := newTestServerWithJobs(t, Options{Dedupe: true, Jobs: jobs})
	upload(t, ts, "a.mp3", []byte("audio"))
	code, health := getJSON(t, ts.URL+"/health")
	if code != http.StatusOK || health["ok"] != true {
		t.Fatalf("health = %v", health)
	}
	if health["dedupe"] != true {
		t.Fatalf("dedupe = %v", health["dedupe"])
	}
	kinds := health["kinds"].(map[string]any)
	if kinds["audio"].(map[string]any)["files"].(float64) != 1 {
		t.Fatalf("kinds = %v", kinds)
	}
	index := health["index"].(map[string]any)
	if index["hashed"].(float64) != 1 {
		t.Fatalf("index = %v", index)
	}
	if health["snapshots"].(float64) != 2 {
		t.Fatalf("snapshots = %v", health["snapshots"])
	}
	if health["retention_on"] != false {
		t.Fatalf("retention_on = %v", health["retention_on"])
	}
}

func TestMaintenanceUnavailableWithoutJobs(t *testing.T) {
	ts, _ := newTestServerWithJobs(t, Options{})
	for _, path := range []string{"/api/v1/maintenance/verify"} {
		if code, _ := getJSON(t, ts.URL+path); code != http.StatusServiceUnavailable {
			t.Fatalf("%s code = %d, want 503（未注入作业）", path, code)
		}
	}
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/backup"); code != http.StatusServiceUnavailable {
		t.Fatalf("backup code = %d", code)
	}
}

func TestMaintenanceStatusDuplicatesVerifyBackup(t *testing.T) {
	jobs := &fakeJobs{}
	ts, _ := newTestServerWithJobs(t, Options{Jobs: jobs})
	upload(t, ts, "x.pdf", []byte("dup"))
	upload(t, ts, "y.pdf", []byte("dup"))

	// status：分类 + 索引 + 作业状态
	code, status := getJSON(t, ts.URL+"/api/v1/maintenance/status")
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if status["files"].(float64) != 2 || status["index_path"] == "" {
		t.Fatalf("status = %v", status)
	}
	if _, ok := status["jobs"].(map[string]any); !ok {
		t.Fatalf("status.jobs = %v", status["jobs"])
	}

	// duplicates：同内容两份 → 一组，可省 3 字节
	_, dups := getJSON(t, ts.URL+"/api/v1/maintenance/duplicates")
	if dups["group_count"].(float64) != 1 || dups["slack_bytes"].(float64) != 3 {
		t.Fatalf("duplicates = %v", dups)
	}

	// verify：quick 透传
	if code, body := getJSON(t, ts.URL+"/api/v1/maintenance/verify?quick=1"); code != http.StatusOK || body["ok"] != true {
		t.Fatalf("verify code=%d body=%v", code, body)
	}
	rep := func() map[string]any {
		_, b := getJSON(t, ts.URL+"/api/v1/maintenance/verify")
		return b["verify"].(map[string]any)
	}()
	if rep["ok"] != true {
		t.Fatalf("verify report = %v", rep)
	}
	if jobs.verifies != 2 {
		t.Fatalf("verify calls = %d", jobs.verifies)
	}

	// backup：立刻快照
	if code, body := postJSON(t, ts.URL+"/api/v1/maintenance/backup"); code != http.StatusOK || body["backup"] == nil {
		t.Fatalf("backup code=%d body=%v", code, body)
	}
	if jobs.backups != 1 {
		t.Fatalf("backup calls = %d", jobs.backups)
	}
}

func TestPruneDefaultsToDryRun(t *testing.T) {
	jobs := &fakeJobs{}
	ts, _ := newTestServerWithJobs(t, Options{Jobs: jobs})

	// 不带参数：dry_run 默认 true；但没给 older_than/kind/keys 时上游会拒（这里 fake 不拒，
	// 只验证默认值确实按 dry-run 传下去）
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/prune?older_than_days=30"); code != http.StatusOK {
		t.Fatalf("prune code = %d", code)
	}
	if opts := jobs.lastPrune(); !opts.DryRun || opts.OlderThan != 30*24*time.Hour {
		t.Fatalf("prune opts = %+v", opts)
	}
	// 显式 dry_run=0 + older_than=30d + kind
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/prune?dry_run=0&older_than=30d&kind=pdf&max=5"); code != http.StatusOK {
		t.Fatalf("prune real code = %d", code)
	}
	opts := jobs.lastPrune()
	if opts.DryRun || opts.OlderThan != 30*24*time.Hour || opts.Kind != store.KindPDF || opts.Max != 5 {
		t.Fatalf("prune opts = %+v", opts)
	}
	// 非法 kind / 非法时长 → 400
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/prune?kind=movies"); code != http.StatusBadRequest {
		t.Fatalf("bad kind code = %d", code)
	}
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/prune?older_than=abc"); code != http.StatusBadRequest {
		t.Fatalf("bad duration code = %d", code)
	}
	// kind 只能当过滤器：单给 kind（没时间窗/点名）会「删掉这类全部」，必须 400
	if code, _ := postJSON(t, ts.URL+"/api/v1/maintenance/prune?kind=pdf&dry_run=0"); code != http.StatusBadRequest {
		t.Fatalf("kind-only prune code = %d, want 400", code)
	}
	// GET 不允许（会改数据）
	if code, _ := getJSON(t, ts.URL+"/api/v1/maintenance/prune"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET prune code = %d", code)
	}
}

func TestUploadDedupeSwitch(t *testing.T) {
	ts, st := newTestServerWithJobs(t, Options{Dedupe: true})
	first := upload(t, ts, "same.jpg", []byte("same-bytes"))
	second := upload(t, ts, "same-again.jpg", []byte("same-bytes"))
	if second["deduped"] != true || second["saved_as"] != first["saved_as"] {
		t.Fatalf("second = %v (first=%v)", second, first)
	}
	infos, _ := st.List(store.ListOptions{})
	if len(infos) != 1 {
		t.Fatalf("files = %d, want 1", len(infos))
	}
	// 单请求可以覆盖开关（?dedupe=0）
	third := uploadQuery(t, ts, "?dedupe=0", "same.jpg", []byte("same-bytes"))
	if third["deduped"] == true {
		t.Fatalf("third = %v", third)
	}
	infos, _ = st.List(store.ListOptions{})
	if len(infos) != 2 {
		t.Fatalf("files = %d, want 2", len(infos))
	}
}

// uploadQuery 带查询串的上传（验证「单请求覆盖 dedupe 开关」）。
func uploadQuery(t *testing.T, ts *httptest.Server, query, name string, data []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	resp, err := http.Post(ts.URL+UploadPath+query, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
