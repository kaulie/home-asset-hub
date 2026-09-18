package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kaulie/home-asset-hub/internal/store"
)

func newTestServer(t *testing.T, publicBase string) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, publicBase, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func upload(t *testing.T, ts *httptest.Server, name string, data []byte) map[string]any {
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
	resp, err := http.Post(ts.URL+UploadPath, mw.FormDataContentType(), &buf)
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

func TestUploadMatchesLegacyResponseShape(t *testing.T) {
	ts, _ := newTestServer(t, "http://192.168.3.84:8080")
	out := upload(t, ts, "paper-asset_9eb884.mp3", []byte("audio-bytes"))
	// Edge 读的是 saved_as（Brain 存 storage.key/saved_as 用的也是它）与 url
	key, _ := out["saved_as"].(string)
	if key == "" {
		t.Fatalf("saved_as missing: %+v", out)
	}
	if !strings.HasSuffix(key, "paper-asset_9eb884.mp3") {
		t.Fatalf("saved_as = %q", key)
	}
	url, _ := out["url"].(string)
	if url != "http://192.168.3.84:8080/"+key {
		t.Fatalf("url = %q", url)
	}
	if out["ok"] != true {
		t.Fatalf("ok = %v", out["ok"])
	}
}

func TestStaticServeAndRangeForDlna(t *testing.T) {
	ts, _ := newTestServer(t, "")
	body := []byte("0123456789")
	key, _ := upload(t, ts, "a.mp3", body)["saved_as"].(string)

	// 电视/小度就是拉 GET /<key>
	resp, err := http.Get(ts.URL + "/" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("static status=%d body=%q", resp.StatusCode, got)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q（DLNA 播放器友好性）", resp.Header.Get("Accept-Ranges"))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/mpeg") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// Range 请求必须 206 + 正确的分片（现网 python 版恒 200，这是 Hub 的改进）
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+key, nil)
	req.Header.Set("Range", "bytes=2-4")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	ranged, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusPartialContent || string(ranged) != "234" {
		t.Fatalf("range status=%d body=%q", resp2.StatusCode, ranged)
	}
}

func TestHealthAndAssetList(t *testing.T) {
	ts, _ := newTestServer(t, "")
	upload(t, ts, "a.mp3", []byte("audio"))
	upload(t, ts, "b.jpg", []byte("image"))

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != true || health["service"] != ServiceName {
		t.Fatalf("health = %+v", health)
	}
	if int(health["files"].(float64)) != 2 {
		t.Fatalf("health files = %v", health["files"])
	}

	resp2, err := http.Get(ts.URL + "/api/v1/assets?mime=audio/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var list map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	assets := list["assets"].([]any)
	if len(assets) != 1 {
		t.Fatalf("audio assets = %d", len(assets))
	}
	first := assets[0].(map[string]any)
	if !strings.HasSuffix(first["key"].(string), "a.mp3") {
		t.Fatalf("asset key = %v", first["key"])
	}
}

func TestTraversalAndMissingAre404(t *testing.T) {
	ts, _ := newTestServer(t, "")
	for _, path := range []string{"/../../etc/passwd", "/img/..%2Fsecret", "/nope.mp3"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestDownloadByNameIsAttachmentAndLatestIsInline(t *testing.T) {
	ts, _ := newTestServer(t, "")
	key, _ := upload(t, ts, "pic.jpg", []byte("jpeg-bytes"))["saved_as"].(string)

	resp, err := http.Get(ts.URL + "/api/v1/photos/" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
	}

	resp2, err := http.Get(ts.URL + "/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || string(body) != "jpeg-bytes" {
		t.Fatalf("latest status=%d body=%q", resp2.StatusCode, body)
	}
	if resp2.Header.Get("Content-Disposition") != "" {
		t.Fatalf("latest should be inline, got %q", resp2.Header.Get("Content-Disposition"))
	}

	// 非图片不进 /latest
	ts2, _ := newTestServer(t, "")
	upload(t, ts2, "doc.pdf", []byte("pdf"))
	resp3, err := http.Get(ts2.URL + "/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("latest(pdf only) status = %d", resp3.StatusCode)
	}
}

var _ = fmt.Sprintf
var _ = time.Now
