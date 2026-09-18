package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// sha256Hex 内存字节的摘要（上传路径用）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashPath 文件摘要（巡检/去重/补索引用）。
func hashPath(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Delete 删除一个资产并写墓碑（索引里不留指向已删文件的活行）。
func (s *Store) Delete(key string) (Info, error) {
	info, err := s.Stat(key)
	if err != nil {
		return Info{}, err
	}
	path, err := s.Path(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.Remove(path); err != nil {
		return Info{}, err
	}
	entry := IndexEntry{Key: key, Deleted: true, Created: time.Now().Format(time.RFC3339)}
	if prev, ok := s.idx.entry(key); ok {
		entry.SHA256 = prev.SHA256
		entry.Bytes = prev.Bytes
		entry.MimeType = prev.MimeType
		entry.Kind = prev.Kind
		if prev.Created != "" {
			entry.Created = prev.Created
		}
	}
	_ = s.idx.record(entry)
	return info, nil
}

// VerifyOptions 巡检选项。
type VerifyOptions struct {
	// Quick：索引里大小一致就算过（不重算 sha256）—— 快，但查不出「同大小被改写」。
	Quick bool
}

// VerifyProblem 一处异常。
type VerifyProblem struct {
	Key    string `json:"key"`
	Status string `json:"status"` // hash_mismatch | size_changed | orphan_index | read_error
	Index  string `json:"index_sha256,omitempty"`
	Actual string `json:"actual_sha256,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// VerifyReport 巡检报告：索引对不对、文件有没有被动过、索引有没有指向已消失的文件。
type VerifyReport struct {
	OK         bool            `json:"ok"`
	Dir        string          `json:"dir"`
	Files      int             `json:"files"`
	FilesBytes int64           `json:"files_bytes"`
	Indexed    int             `json:"indexed"`     // 巡检前就有 sha256 的
	Hashed     int             `json:"hashed"`      // 本次真算过 sha256 的
	NotIndexed int             `json:"not_indexed"` // 无索引（已顺带补上）
	Problems   []VerifyProblem `json:"problems"`
	TookMS     int64           `json:"took_ms"`
	Quick      bool            `json:"quick"`
	Repaired   int             `json:"repaired"` // 补写/修正的索引条数
}

func (r *VerifyReport) addProblem(p VerifyProblem) {
	r.Problems = append(r.Problems, p)
}

// Verify 全目录巡检：算 sha256 与索引比对，顺带把缺的索引补上（自愈）。
//
// 判定只看「真实字节 vs 索引」：索引缺失算 not_indexed（第一次开巡检的目录必然如此），
// 不算故障；只有 hash/size 对不上、或索引指向已消失的文件才算 problem。
func (s *Store) Verify(opts VerifyOptions) (VerifyReport, error) {
	start := time.Now()
	rep := VerifyReport{Dir: s.dir, Quick: opts.Quick, Problems: []VerifyProblem{}}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			rep.OK = true
			rep.TookMS = time.Since(start).Milliseconds()
			return rep, nil
		}
		return rep, err
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || isHidden(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		seen[name] = true
		rep.Files++
		rep.FilesBytes += fi.Size()
		rec, indexed := s.idx.entry(name)
		if indexed && rec.SHA256 != "" && !rec.Deleted {
			rep.Indexed++
			if opts.Quick && rec.Bytes == fi.Size() {
				continue
			}
			sum, err := hashPath(filepath.Join(s.dir, name))
			if err != nil {
				rep.addProblem(VerifyProblem{Key: name, Status: "read_error", Detail: err.Error()})
				continue
			}
			rep.Hashed++
			if sum != rec.SHA256 {
				rep.addProblem(VerifyProblem{Key: name, Status: "hash_mismatch", Index: rec.SHA256, Actual: sum})
				s.record(name, fi, sum, rec.Source, false)
				rep.Repaired++
				continue
			}
			if rec.Bytes != fi.Size() {
				rep.addProblem(VerifyProblem{Key: name, Status: "size_changed", Index: rec.SHA256, Actual: sum})
				s.record(name, fi, sum, rec.Source, false)
				rep.Repaired++
			}
			continue
		}
		// 无索引（或墓碑）：补一次 sha256 并登记
		sum, err := hashPath(filepath.Join(s.dir, name))
		if err != nil {
			rep.addProblem(VerifyProblem{Key: name, Status: "read_error", Detail: err.Error()})
			continue
		}
		rep.Hashed++
		rep.NotIndexed++
		s.record(name, fi, sum, rec.Source, false)
		rep.Repaired++
	}
	// 反向：索引里有、磁盘上没了 → 孤行。
	// 刻意不自动删行：文件消失可能是 rsync/手工干的，「谁消失了」这条线索要留着。
	for key, rec := range s.idx.snapshot() {
		if rec.Deleted || seen[key] {
			continue
		}
		rep.addProblem(VerifyProblem{Key: key, Status: "orphan_index", Index: rec.SHA256,
			Detail: "索引里有、目录里没有（文件被外部删除？）"})
	}
	rep.OK = len(rep.Problems) == 0
	rep.TookMS = time.Since(start).Milliseconds()
	return rep, nil
}

// DuplicateGroup 一组内容相同的资产（同 sha256、多个 key）。
type DuplicateGroup struct {
	SHA256 string   `json:"sha256"`
	Bytes  int64    `json:"bytes"`
	Keys   []string `json:"keys"`
	Slack  int64    `json:"slack_bytes"` // 可省下的空间 = Bytes*(len(Keys)-1)
}

// Duplicates 找出内容重复的资产（索引缺失的文件会顺带补 hash）。
func (s *Store) Duplicates() ([]DuplicateGroup, error) {
	infos, err := s.List(ListOptions{})
	if err != nil {
		return nil, err
	}
	byHash := map[string]*DuplicateGroup{}
	for _, info := range infos {
		sum, err := s.SHA256(info.Key)
		if err != nil || sum == "" {
			continue
		}
		g, ok := byHash[sum]
		if !ok {
			g = &DuplicateGroup{SHA256: sum, Bytes: info.Bytes}
			byHash[sum] = g
		}
		g.Keys = append(g.Keys, info.Key)
	}
	out := make([]DuplicateGroup, 0, len(byHash))
	for _, g := range byHash {
		if len(g.Keys) < 2 {
			continue
		}
		sort.Strings(g.Keys)
		g.Slack = g.Bytes * int64(len(g.Keys)-1)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slack == out[j].Slack {
			return out[i].SHA256 < out[j].SHA256
		}
		return out[i].Slack > out[j].Slack
	})
	return out, nil
}

// PruneOptions 删除/保留选项。
//
// 目标必须明确：`OlderThan>0`（时间窗）或 `Keys`（点名）。**Kind 只是过滤器**，
// 单给 kind 等价于「删掉这类全部」—— 那正是最危险的误删，故不认。
type PruneOptions struct {
	// OlderThan：只动 mtime 早于 now-OlderThan 的资产。
	OlderThan time.Duration
	Kind      Kind     // 只动这一类（空=不限；必须与 OlderThan/Keys 合用）
	Keys      []string // 只动这些 key（显式点名，忽略 OlderThan）
	Max       int      // 单次最多处理多少个（0=不限）
	DryRun    bool     // true=只报候选，不删
}

// PruneItem 一条候选/已删记录。
type PruneItem struct {
	Key      string `json:"key"`
	Bytes    int64  `json:"bytes"`
	Kind     Kind   `json:"kind"`
	Modified string `json:"modified"`
}

// PruneReport 清理报告。
type PruneReport struct {
	DryRun     bool        `json:"dry_run"`
	OlderThan  string      `json:"older_than,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Candidates []PruneItem `json:"candidates"`
	Deleted    []PruneItem `json:"deleted"`
	FreedBytes int64       `json:"freed_bytes"`
	Truncated  int         `json:"truncated"` // 因 Max 被截掉的候选数
}

// ErrPruneNeedsTarget：既没给时间窗、也没点名 key —— 拒绝执行（避免「删空目录」）。
var ErrPruneNeedsTarget = fmt.Errorf("prune 需要明确目标：older_than>0 或 keys（只给 kind 等于「删掉这类全部」，不认）")

// Prune 按时间窗/类型/key 清理资产。DryRun=true 只报候选。
func (s *Store) Prune(opts PruneOptions) (PruneReport, error) {
	explicitKeys := len(opts.Keys) > 0
	if !explicitKeys && opts.OlderThan <= 0 {
		return PruneReport{}, ErrPruneNeedsTarget
	}
	rep := PruneReport{DryRun: opts.DryRun, Kind: string(opts.Kind), Candidates: []PruneItem{}, Deleted: []PruneItem{}}
	if opts.OlderThan > 0 {
		rep.OlderThan = opts.OlderThan.String()
	}
	infos, err := s.List(ListOptions{Kind: opts.Kind})
	if err != nil {
		return rep, err
	}
	want := map[string]bool{}
	for _, k := range opts.Keys {
		want[k] = true
	}
	cutoff := time.Now().Add(-opts.OlderThan)
	for _, info := range infos {
		if explicitKeys {
			if !want[info.Key] {
				continue
			}
		} else if opts.OlderThan > 0 {
			ts, err := time.Parse(time.RFC3339, info.Modified)
			if err != nil || !ts.Before(cutoff) {
				continue
			}
		}
		rep.Candidates = append(rep.Candidates, PruneItem{Key: info.Key, Bytes: info.Bytes, Kind: info.Kind, Modified: info.Modified})
	}
	if opts.Max > 0 && len(rep.Candidates) > opts.Max {
		rep.Truncated = len(rep.Candidates) - opts.Max
		rep.Candidates = rep.Candidates[:opts.Max]
	}
	if opts.DryRun {
		return rep, nil
	}
	for _, item := range rep.Candidates {
		if _, err := s.Delete(item.Key); err != nil {
			continue
		}
		rep.Deleted = append(rep.Deleted, item)
		rep.FreedBytes += item.Bytes
	}
	return rep, nil
}

// KindsInUse 目录里当前出现的 kind（参数校验/巡检展示用）。
func (s *Store) KindsInUse() []Kind {
	counts, _, _, err := s.KindCounts()
	if err != nil {
		return nil
	}
	var out []Kind
	for _, k := range Kinds {
		if st, ok := counts[string(k)]; ok && st.Files > 0 {
			out = append(out, k)
		}
	}
	return out
}
