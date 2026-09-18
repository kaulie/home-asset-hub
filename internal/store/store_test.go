package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPutAndOpenKeepsCompatKeyShape(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Put("paper-asset_9eb884.mp3", []byte("hello-audio"))
	if err != nil {
		t.Fatal(err)
	}
	// 现网 key = <8hex>_<原名>：Brain 的 storage.key 就长这样
	parts := strings.SplitN(info.Key, "_", 2)
	if len(parts) != 2 || len(parts[0]) != 8 {
		t.Fatalf("key shape = %q, want 8hex_suffix", info.Key)
	}
	if parts[1] != "paper-asset_9eb884.mp3" {
		t.Fatalf("suffix = %q", parts[1])
	}
	f, fi, err := st.Open(info.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi.Size() != int64(len("hello-audio")) {
		t.Fatalf("size = %d", fi.Size())
	}
}

func TestPathRejectsTraversal(t *testing.T) {
	st, _ := New(t.TempDir())
	for _, key := range []string{"", "../x", "a/b", "a\\b", "..", "a..b"} {
		if _, err := st.Path(key); err == nil {
			t.Fatalf("key %q should be rejected", key)
		}
	}
}

func TestListOrdersNewestFirstAndFiltersMime(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	audio, err := st.Put("a.mp3", []byte("audio"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put("b.jpg", []byte("image")); err != nil {
		t.Fatal(err)
	}
	// 让 a.mp3 变成「最新」
	newest := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, audio.Key), newest, newest); err != nil {
		t.Fatal(err)
	}
	all, err := st.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d", len(all))
	}
	if !strings.HasSuffix(all[0].Key, "a.mp3") {
		t.Fatalf("newest first expected a.mp3, got %q", all[0].Key)
	}
	onlyAudio, err := st.List(ListOptions{MimeType: "audio/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyAudio) != 1 || !strings.HasSuffix(onlyAudio[0].Key, "a.mp3") {
		t.Fatalf("mime filter got %+v", onlyAudio)
	}
	if onlyAudio[0].MimeType != "audio/mpeg" {
		t.Fatalf("mime = %q", onlyAudio[0].MimeType)
	}
}

func TestListSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	if _, err := st.Put("a.mp3", []byte("audio")); err != nil {
		t.Fatal(err)
	}
	// macOS 常见噪声：.DS_Store / 资源分叉
	for _, junk := range []string{".DS_Store", "._a.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, junk), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := st.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("hidden files leaked into list: %+v", infos)
	}
}

func TestLatestImage(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)
	if _, err := st.Put("note.pdf", []byte("pdf")); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.LatestImage(); ok {
		t.Fatal("pdf must not count as image")
	}
	if _, err := st.Put("shot.PNG", []byte("png")); err != nil {
		t.Fatal(err)
	}
	key, ok := st.LatestImage()
	if !ok || !strings.HasSuffix(key, "shot.PNG") {
		t.Fatalf("latest image = %q ok=%v", key, ok)
	}
}
