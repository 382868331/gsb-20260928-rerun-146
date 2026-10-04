package assetpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 包二进制格式 (v1)，大端序：
//
//	magic[8] = "ASTPKG01"
//	uint32 version
//	uint64 fragmentSize
//	uint64 fragmentCount
//	uint64 manifestLen
//	for each fragment (按 digest 排序, 去重):
//	    recMagic[4] = "FRAG"
//	    uint8 digestLen ; digest
//	    uint64 dataLen ; data
//	manifest JSON
//	trailer (56 字节):
//	    uint64 indexOffset (= manifestOffset)
//	    uint64 manifestLen
//	    [32]byte manifestDigest
//	    magic[8] = "ASTPKEND"

const (
	fileMagic      = "ASTPKG01"
	endMagic       = "ASTPKEND"
	fragMagic      = "FRAG"
	FormatVersion  = uint32(1)
	DefaultFragLen = 64 * 1024
	trailerLen     = 8 + 8 + 32 + 8
)

var (
	ErrCorrupt       = errors.New("assetpack: package corrupt")
	ErrPathTraversal = errors.New("assetpack: path traversal rejected")
	ErrNotFound      = errors.New("assetpack: resource not found")
	ErrBadDigest     = errors.New("assetpack: digest mismatch")
)

// FragmentRef 引用一段去重后的内容片段。
type FragmentRef struct {
	Digest string `json:"d"`
	Start  int64  `json:"s"` // 片段在资源逻辑字节流中的起始偏移
	Length int64  `json:"l"` // 该片段占用的字节数
}

// Resource 是一个被打包的素材单元。
type Resource struct {
	Path      string        `json:"p"`              // 包内逻辑路径, 形如 dir/file
	ID        string        `json:"i"`              // 内容身份 = sha256(规范化内容)
	Size      int64         `json:"z"`              // 逻辑大小
	Type      string        `json:"t"`              // "file" | "dir"
	Fragments []FragmentRef `json:"f"`              // file: 有序片段; dir: 单片段存目录清单
	DependsOn []string      `json:"deps,omitempty"` // 所依赖资源的逻辑路径
}

// Manifest 是包索引, 完成前不会落盘发布。
type Manifest struct {
	Version      uint32     `json:"version"`
	FragmentSize int64      `json:"fragmentSize"`
	Resources    []Resource `json:"resources"`
	BaseIndex    string     `json:"baseIndex,omitempty"` // 增量发布时引用的前一版索引摘要
	IndexDigest  string     `json:"indexDigest,omitempty"`
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// canonicalJSON 产出键有序、无多余空白的稳定 JSON。
func canonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Options 控制打包行为。
type Options struct {
	FragmentSize int64
	// Base 为上一版包的 manifest; 非空表示增量发布。
	Base *Manifest
}

// AddRequest 描述一个待打包素材。Data 为文件内容; Children 为目录的直接子项路径。
type AddRequest struct {
	Path      string
	Data      []byte
	IsDir     bool
	Remove    bool     // 增量发布时删除基础包中的同名资源
	Children  []string // IsDir 时使用, 子项逻辑路径
	DependsOn []string
}

type fragEntry struct {
	digest string
	data   []byte
}

// Build 依据一组固定输入构建可复现的包。
func Build(reqs []AddRequest, opts Options) ([]byte, *Manifest, error) {
	fragSize := opts.FragmentSize
	if fragSize <= 0 {
		if opts.Base != nil {
			fragSize = opts.Base.FragmentSize
		} else {
			fragSize = DefaultFragLen
		}
	}
	if opts.Base != nil && opts.Base.FragmentSize != fragSize {
		return nil, nil, fmt.Errorf("assetpack: incremental fragment size %d differs from base %d", fragSize, opts.Base.FragmentSize)
	}

	// 规范化并校验路径
	seen := map[string]bool{}
	baseFrags := map[string]bool{}
	basePaths := map[string]bool{}
	if opts.Base != nil {
		for _, br := range opts.Base.Resources {
			basePaths[br.Path] = true
			for _, bf := range br.Fragments {
				baseFrags[bf.Digest] = true
			}
		}
	}
	for i := range reqs {
		reqs[i].Path = cleanLogicalPath(reqs[i].Path)
		if reqs[i].Path == "" || reqs[i].Path == "." {
			return nil, nil, fmt.Errorf("%w: empty path", ErrPathTraversal)
		}
		if reqs[i].Path == metaDir || strings.HasPrefix(reqs[i].Path, metaDir+"/") {
			return nil, nil, fmt.Errorf("%w: reserved name %q", ErrPathTraversal, reqs[i].Path)
		}
		if seen[reqs[i].Path] {
			return nil, nil, fmt.Errorf("assetpack: duplicate resource path %q", reqs[i].Path)
		}
		seen[reqs[i].Path] = true
	}

	// 片段去重表: digest -> entry
	fragStore := map[string]*fragEntry{}
	var orderedDigests []string

	storeFragment := func(data []byte) string {
		d := hashBytes(data)
		if baseFrags[d] {
			return d // 增量发布: 内容身份不变的片段由基础包提供, 不重复保存
		}
		if _, ok := fragStore[d]; !ok {
			cp := append([]byte(nil), data...)
			fragStore[d] = &fragEntry{digest: d, data: cp}
			orderedDigests = append(orderedDigests, d)
		}
		return d
	}

	resources := make([]Resource, 0, len(reqs))
	for _, req := range reqs {
		res := Resource{Path: req.Path, Type: "file"}
		if req.Remove {
			if !basePaths[req.Path] {
				return nil, nil, fmt.Errorf("assetpack: remove of unknown resource %q", req.Path)
			}
			res.Type = "removed" // 墓碑: 合并包链时删除该资源
			resources = append(resources, res)
			continue
		}
		var body []byte
		if req.IsDir {
			res.Type = "dir"
			ch := append([]string(nil), req.Children...)
			sort.Strings(ch)
			listing, _ := canonicalJSON(struct {
				Type     string   `json:"type"`
				Path     string   `json:"path"`
				Children []string `json:"children"`
			}{"dir", req.Path, ch})
			body = listing
		} else {
			body = req.Data
		}

		res.Size = int64(len(body))
		res.ID = hashBytes(body)
		var refs []FragmentRef
		for off := int64(0); off < int64(len(body)); off += fragSize {
			end := off + fragSize
			if end > int64(len(body)) {
				end = int64(len(body))
			}
			chunk := body[off:end]
			d := storeFragment(chunk)
			refs = append(refs, FragmentRef{Digest: d, Start: off, Length: int64(len(chunk))})
		}
		res.Fragments = refs

		deps := dedupSortedStrings(req.DependsOn)
		for _, dep := range deps {
			res.DependsOn = append(res.DependsOn, cleanLogicalPath(dep))
		}
		resources = append(resources, res)
	}

	// 校验依赖指向存在的资源(本批或基础包)。
	for _, r := range resources {
		for _, dep := range r.DependsOn {
			if !seen[dep] && !basePaths[dep] {
				return nil, nil, fmt.Errorf("assetpack: resource %q depends on missing %q", r.Path, dep)
			}
		}
	}

	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
	sort.Strings(orderedDigests)

	m := &Manifest{
		Version:      FormatVersion,
		FragmentSize: fragSize,
		Resources:    resources,
	}
	if opts.Base != nil {
		m.BaseIndex = opts.Base.IndexDigest
	}

	// 清单身份对不含 indexDigest 字段的规范 JSON 计算, 保证发布体可自证。
	mfPublished, idDigest, err := encodeManifest(m)
	if err != nil {
		return nil, nil, err
	}
	m.IndexDigest = idDigest
	mfPublished, _, err = encodeManifest(m)
	if err != nil {
		return nil, nil, err
	}

	var buf bytes.Buffer
	w := &buf
	w.WriteString(fileMagic)
	putU32(w, FormatVersion)
	putU64(w, uint64(fragSize))
	putU64(w, uint64(len(orderedDigests)))
	putU64(w, uint64(len(mfPublished)))
	for _, d := range orderedDigests {
		e := fragStore[d]
		w.WriteString(fragMagic)
		db, _ := hex.DecodeString(e.digest)
		w.WriteByte(byte(len(db)))
		w.Write(db)
		putU64(w, uint64(len(e.data)))
		w.Write(e.data)
	}
	indexOffset := int64(buf.Len())
	w.Write(mfPublished)
	putU64(w, uint64(indexOffset))
	putU64(w, uint64(len(mfPublished)))
	pubDig, _ := hex.DecodeString(hashBytes(mfPublished))
	w.Write(pubDig)
	w.WriteString(endMagic)

	return buf.Bytes(), m, nil
}

func encodeManifest(m *Manifest) (published []byte, identity string, err error) {
	var raw map[string]json.RawMessage
	b, err := canonicalJSON(m)
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, "", err
	}
	delete(raw, "indexDigest")
	idBody, err := canonicalJSON(raw)
	if err != nil {
		return nil, "", err
	}
	identity = hashBytes(idBody)
	return b, identity, nil
}

func putU32(w io.Writer, v uint32) { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); w.Write(b[:]) }
func putU64(w io.Writer, v uint64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], v); w.Write(b[:]) }

func dedupSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := map[string]struct{}{}
	for _, s := range in {
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// cleanLogicalPath 归一化逻辑路径并拒绝越界成分。
func cleanLogicalPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimSpace(p)
	wasAbsolute := strings.HasPrefix(p, "/")
	p = strings.TrimPrefix(p, "./")
	for strings.HasPrefix(p, "/") {
		p = strings.TrimPrefix(p, "/")
	}
	parts := strings.Split(p, "/")
	kept := parts[:0]
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "" // 越界信号
		}
		kept = append(kept, part)
	}
	out := strings.Join(kept, "/")
	if wasAbsolute {
		return "" // 绝对路径不允许作为包内逻辑路径
	}
	return out
}

// WriteFileAtomic 在目标目录原子发布文件: 先写临时文件再 rename。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
