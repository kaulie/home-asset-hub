// Package store：把「引擎」定成一个扁平的文件库 —— 与现网 img-server 的存储完全兼容：
//
//	<dir>/<8hex>_<原名>     # 一个 Asset 一个文件，key 就是文件名
//
// 为什么保持扁平：小米电视/小度拿到的 URL 是 `http://<mac-lan-ip>:8080/<key>`，
// Brain 的 assets 表里存的也是 `storage.key`/`saved_as`（同一个 key）——
// 换实现不改 key，就不用动 Edge / Brain / iOS 任何配置。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrBadKey：key 不合法（含路径分隔符、`..`、空）。调用方翻成 400/404。
var ErrBadKey = errors.New("invalid asset key")

// Info 一条资产元数据（资源管理用；不读文件内容）。
type Info struct {
	Key       string `json:"key"`
	Bytes     int64  `json:"bytes"`
	Modified  string `json:"modified"`
	MimeType  string `json:"mime_type"`
	URLSuffix string `json:"url_suffix"` // 就是 /<key>，便于调用方拼 public base
}

// Store 文件库。
type Store struct {
	dir string
}

// New 打开（不存在的目录会在首次写入时创建）。
func New(dir string) (*Store, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil || strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("store dir required: %q", dir)
	}
	return &Store{dir: abs}, nil
}

// Dir 返回绝对路径（日志/健康检查用）。
func (s *Store) Dir() string { return s.dir }

// Path 把 key 安全地映射成文件路径：只允许单层文件名，禁止分隔符/`..`。
func (s *Store) Path(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" || strings.ContainsAny(k, "/\\") || k == "." || k == ".." || strings.Contains(k, "..") {
		return "", ErrBadKey
	}
	return filepath.Join(s.dir, k), nil
}

// Open 打开一个 key（调用方负责 Close；http.ServeContent 需要 io.ReadSeeker）。
func (s *Store) Open(key string) (*os.File, fs.FileInfo, error) {
	path, err := s.Path(key)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, nil, fmt.Errorf("asset %q is a directory", key)
	}
	return f, info, nil
}

// Put 落盘一份字节，key = `<8hex>_<安全名>`（与现网 img-server 一致）。
func (s *Store) Put(filename string, data []byte) (Info, error) {
	if len(data) == 0 {
		return Info{}, errors.New("empty file")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Info{}, err
	}
	key := NewKey(filename)
	if err := os.WriteFile(filepath.Join(s.dir, key), data, 0o644); err != nil {
		return Info{}, err
	}
	return s.Stat(key)
}

// NewKey 生成 key：8 位随机 hex + `_` + 安全文件名（与现网一致，不改 key 语义）。
func NewKey(filename string) string {
	safe := SanitizeName(filename)
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d_%s", time.Now().UnixNano()&0xffffffff, safe)
	}
	return hex.EncodeToString(buf) + "_" + safe
}

// SanitizeName 只留基名、去掉路径分隔符与控制字符。
func SanitizeName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, "/", "_")
	base = strings.ReplaceAll(base, "\\", "_")
	base = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, base)
	if base == "" || base == "." || base == ".." {
		return "asset.bin"
	}
	return base
}

// Stat 单个 key 的元数据。
func (s *Store) Stat(key string) (Info, error) {
	path, err := s.Path(key)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	return infoOf(key, fi), nil
}

// List 列出全部资产（按修改时间倒序；limit<=0 表示全部）。
type ListOptions struct {
	Limit    int
	MimeType string // 前缀匹配，如 "audio/"
}

func (s *Store) List(opts ListOptions) ([]Info, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || isHidden(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info := infoOf(e.Name(), fi)
		if opts.MimeType != "" && !strings.HasPrefix(info.MimeType, opts.MimeType) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Modified == out[j].Modified {
			return out[i].Key > out[j].Key
		}
		return out[i].Modified > out[j].Modified
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// LatestImage 最新一张图片（兼容现网 /latest）：按扩展名判断。
func (s *Store) LatestImage() (string, bool) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", false
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || isHidden(e.Name()) || !isImageName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || fi.ModTime().After(newestAt) {
			newest, newestAt = e.Name(), fi.ModTime()
		}
	}
	return newest, newest != ""
}

func infoOf(key string, fi fs.FileInfo) Info {
	return Info{
		Key:       key,
		Bytes:     fi.Size(),
		Modified:  fi.ModTime().Format(time.RFC3339),
		MimeType:  mimeTypeOf(key),
		URLSuffix: "/" + key,
	}
}

func mimeTypeOf(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		if i := strings.IndexByte(ct, ';'); i > 0 {
			return strings.TrimSpace(ct[:i])
		}
		return ct
	}
	return "application/octet-stream"
}

// isHidden：`.DS_Store`、资源分叉文件（`._x`）之类不该出现在清单/最新图里。
func isHidden(name string) bool { return strings.HasPrefix(name, ".") }

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".heic", ".webp", ".gif":
		return true
	default:
		return false
	}
}
