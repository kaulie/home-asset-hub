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
	Kind      Kind   `json:"kind"`              // image/audio/video/pdf/text/document/other
	URLSuffix string `json:"url_suffix"`        // 就是 /<key>，便于调用方拼 public base
	SHA256    string `json:"sha256,omitempty"`  // 索引里有才算（v1 老文件由巡检/去重补齐）
	Source    string `json:"source,omitempty"`  // 来源标记（上传时可选带）
	Deduped   bool   `json:"deduped,omitempty"` // 本次写入是否复用了同内容已存在的 key
	Created   string `json:"created,omitempty"` // 首次入库时间（索引里有才算）
}

// Store 文件库。
type Store struct {
	dir string
	idx *index
}

// New 打开（不存在的目录会在首次写入时创建）。会顺带载入 sha256 索引。
func New(dir string) (*Store, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil || strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("store dir required: %q", dir)
	}
	return &Store{dir: abs, idx: loadIndex(filepath.Join(abs, IndexFileName))}, nil
}

// IndexPath 索引文件路径（巡检/日志用）。
func (s *Store) IndexPath() string { return s.idx.path }

// IndexStats 索引概览：entries=登记条数、hashed=有 sha256 的条数、duplicateGroups=同内容多 key 的组数。
func (s *Store) IndexStats() (entries, hashed, duplicateGroups int) {
	e, h, _, d := s.idx.indexStats()
	return e, h, d
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
//
// 不做去重（等价于 PutWithOptions{Dedupe:false}）—— HTTP 层按配置决定是否去重。
func (s *Store) Put(filename string, data []byte) (Info, error) {
	return s.PutWithOptions(filename, data, PutOptions{})
}

// PutOptions 写入选项。
type PutOptions struct {
	// Dedupe：同 sha256 已存在时复用已存在的 key（不再写第二份字节）。
	// 复用时会把该文件的 mtime 顶到当前时间 —— 否则「重传同一张图让它变成最新」
	// 这条现网用法（/latest、asset.inventory newest_first）会失效。
	Dedupe bool
	// Source：来源标记（audit/分类用，原样记进索引）。
	Source string
}

// PutWithOptions 写入并登记 sha256 索引。
func (s *Store) PutWithOptions(filename string, data []byte, opts PutOptions) (Info, error) {
	if len(data) == 0 {
		return Info{}, errors.New("empty file")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return Info{}, err
	}
	sum := sha256Hex(data)

	if opts.Dedupe {
		if info, ok := s.reuseByHash(sum, opts.Source); ok {
			return info, nil
		}
	}

	key := NewKey(filename)
	if err := os.WriteFile(filepath.Join(s.dir, key), data, 0o644); err != nil {
		return Info{}, err
	}
	fi, err := os.Stat(filepath.Join(s.dir, key))
	if err != nil {
		return Info{}, err
	}
	s.record(key, fi, sum, opts.Source, false)
	return s.infoOf(key, fi), nil
}

// reuseByHash：同内容已存在 → 顶 mtime 后复用（索引里所有候选都缺文件时返回 false，退化成新写）。
func (s *Store) reuseByHash(sum, source string) (Info, bool) {
	now := time.Now()
	for _, key := range s.idx.keysByHash(sum) {
		fi, err := os.Stat(filepath.Join(s.dir, key))
		if err != nil || fi.IsDir() {
			continue
		}
		if err := os.Chtimes(filepath.Join(s.dir, key), now, now); err != nil {
			continue
		}
		fi, err = os.Stat(filepath.Join(s.dir, key))
		if err != nil {
			continue
		}
		if prev, ok := s.idx.entry(key); ok && source != "" && prev.Source == "" {
			prev.Source = source
			_ = s.idx.record(prev)
		}
		info := s.infoOf(key, fi)
		info.Deduped = true
		return info, true
	}
	return Info{}, false
}

// record 登记一条索引（Created 只在首见时写）。
func (s *Store) record(key string, fi fs.FileInfo, sum, source string, deleted bool) {
	entry := IndexEntry{
		Key:      key,
		SHA256:   sum,
		Bytes:    fi.Size(),
		MimeType: mimeTypeOf(key),
		Kind:     KindOf(mimeTypeOf(key)),
		Source:   source,
		Created:  fi.ModTime().Format(time.RFC3339),
		Deleted:  deleted,
	}
	if prev, ok := s.idx.entry(key); ok && prev.Created != "" {
		entry.Created = prev.Created
		if entry.Source == "" {
			entry.Source = prev.Source
		}
	}
	_ = s.idx.record(entry)
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
	if fi.IsDir() {
		return Info{}, fmt.Errorf("asset %q is a directory", key)
	}
	return s.infoOf(key, fi), nil
}

// SHA256 单条内容的 sha256：索引里有且大小一致就直接用，否则读文件算一次并补进索引。
func (s *Store) SHA256(key string) (string, error) {
	if e, ok := s.idx.entry(key); ok && e.SHA256 != "" && !e.Deleted {
		if fi, err := os.Stat(filepath.Join(s.dir, key)); err == nil && fi.Size() == e.Bytes {
			return e.SHA256, nil
		}
	}
	fi, err := os.Stat(filepath.Join(s.dir, key))
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", ErrBadKey
	}
	sum, err := hashPath(filepath.Join(s.dir, key))
	if err != nil {
		return "", err
	}
	s.record(key, fi, sum, "", false)
	return sum, nil
}

// List 列出全部资产（按修改时间倒序；limit<=0 表示全部）。
type ListOptions struct {
	Limit    int
	Offset   int
	MimeType string // 前缀匹配，如 "audio/"
	Kind     Kind   // 精确匹配（由 mime_type 推出）
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
		info := s.infoOf(e.Name(), fi)
		if opts.MimeType != "" && !strings.HasPrefix(info.MimeType, opts.MimeType) {
			continue
		}
		if opts.Kind != "" && info.Kind != opts.Kind {
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
	if opts.Offset > 0 {
		if opts.Offset >= len(out) {
			return nil, nil
		}
		out = out[opts.Offset:]
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// KindStat 一类资源的数量与体积。
type KindStat struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// KindCounts 全量目录按 kind 的统计（一次 ReadDir，不算 sha256；/health 与清单接口用）。
func (s *Store) KindCounts() (map[string]KindStat, int, int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]KindStat{}, 0, 0, nil
		}
		return nil, 0, 0, err
	}
	counts := map[string]KindStat{}
	var files int
	var bytes int64
	for _, e := range entries {
		if e.IsDir() || isHidden(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		k := string(KindOf(mimeTypeOf(e.Name())))
		st := counts[k]
		st.Files++
		st.Bytes += fi.Size()
		counts[k] = st
		files++
		bytes += fi.Size()
	}
	return counts, files, bytes, nil
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

// infoOf 组装元数据：mtime/size 来自文件本身，sha256/source/created 来自索引（有才算）。
func (s *Store) infoOf(key string, fi fs.FileInfo) Info {
	mt := mimeTypeOf(key)
	info := Info{
		Key:       key,
		Bytes:     fi.Size(),
		Modified:  fi.ModTime().Format(time.RFC3339),
		MimeType:  mt,
		Kind:      KindOf(mt),
		URLSuffix: "/" + key,
	}
	if e, ok := s.idx.entry(key); ok && !e.Deleted {
		info.SHA256 = e.SHA256
		info.Source = e.Source
		info.Created = e.Created
	}
	return info
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
