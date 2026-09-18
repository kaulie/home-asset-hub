package store

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// IndexFileName：sha256 索引（资源管理 v2 的地基）。
//
// 放在资源目录里、以 `.` 开头：清单/最新图本来就跳过隐藏文件（isHidden），
// 所以它对现网调用方（电视/小度/Edge/Brain）完全不可见；备份只要同步资源目录，
// 索引就跟着一起被备走 —— 不需要第二处状态。
//
// 形态是 append-only JSONL：一行一个事件（登记/删除墓碑），崩溃半行也不会毁掉已有索引；
// 读到坏行直接跳过。一个 key 只有最后一行生效。
const IndexFileName = ".assethub-index.jsonl"

// IndexEntry 一个 key 的索引行。
type IndexEntry struct {
	Key      string `json:"key"`
	SHA256   string `json:"sha256,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Kind     Kind   `json:"kind,omitempty"`
	Source   string `json:"source,omitempty"`  // 来源标记（Edge 上传可带；审计/分类用）
	Created  string `json:"created,omitempty"` // 首次入库时间（RFC3339）
	Deleted  bool   `json:"deleted,omitempty"` // 墓碑：key 已删除
}

type index struct {
	path    string
	mu      sync.Mutex
	entries map[string]IndexEntry
	byHash  map[string][]string
}

func loadIndex(path string) *index {
	ix := &index{path: path, entries: map[string]IndexEntry{}, byHash: map[string][]string{}}
	f, err := os.Open(path)
	if err != nil {
		return ix
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e IndexEntry
		if err := json.Unmarshal(line, &e); err != nil || e.Key == "" {
			continue // 坏行（半写/手工改动）忽略，不影响其余索引
		}
		ix.apply(e)
	}
	return ix
}

// apply 把一行登记进内存表（不落盘）。
func (ix *index) apply(e IndexEntry) {
	prev, existed := ix.entries[e.Key]
	if existed && prev.SHA256 != "" {
		ix.dropHash(prev.SHA256, e.Key)
	}
	if e.Deleted {
		delete(ix.entries, e.Key)
		return
	}
	ix.entries[e.Key] = e
	if e.SHA256 != "" {
		ix.byHash[e.SHA256] = append(ix.byHash[e.SHA256], e.Key)
	}
}

func (ix *index) dropHash(sum, key string) {
	keys := ix.byHash[sum]
	out := keys[:0]
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		delete(ix.byHash, sum)
		return
	}
	ix.byHash[sum] = out
}

func (ix *index) entry(key string) (IndexEntry, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	e, ok := ix.entries[key]
	return e, ok
}

// keysByHash 返回同 sha256 的全部 key（不含墓碑）。
func (ix *index) keysByHash(sum string) []string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	keys := ix.byHash[sum]
	out := make([]string, len(keys))
	copy(out, keys)
	return out
}

// snapshot 拷贝一份内存索引（遍历/校验用；避免长时间持锁）。
func (ix *index) snapshot() map[string]IndexEntry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make(map[string]IndexEntry, len(ix.entries))
	for k, v := range ix.entries {
		out[k] = v
	}
	return out
}

// record 追加一行并更新内存表（append-only：只写不重写，进程被杀也不丢已登记的行）。
func (ix *index) record(e IndexEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	f, err := os.OpenFile(ix.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ix.apply(e)
	return nil
}

// indexStats 索引概览：登记条数、有 hash 的条数、去重（同 hash 多 key）组数。
func (ix *index) indexStats() (entries, hashed, hashGroups, dupGroups int) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	entries = len(ix.entries)
	for sum, keys := range ix.byHash {
		hashed += len(keys)
		hashGroups++
		if len(keys) > 1 {
			dupGroups++
		}
		_ = sum
	}
	return entries, hashed, hashGroups, dupGroups
}

func (ix *index) allKeys() []string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]string, 0, len(ix.entries))
	for k := range ix.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
