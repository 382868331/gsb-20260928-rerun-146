package assetpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// fragLoc 记录某个片段在底层文件中的物理位置。
type fragLoc struct {
	offset int64
	length int64
}

// Reader 支持按需读取一个包(或增量包链)。片段在首次需要时才读取并核对摘要。
type Reader struct {
	ra        io.ReaderAt
	size      int64
	merged    *Manifest
	fragIndex map[string]fragLoc
	base      *Reader
}

// ReaderAtSize 组合 ReaderAt 与长度信息。
type ReaderAtSize interface {
	io.ReaderAt
	Size() int64
}

// OpenReaderAt 打开单个(非增量)包。
func OpenReaderAt(ra io.ReaderAt, size int64) (*Reader, error) {
	r := &Reader{ra: ra, size: size}
	if err := r.parse(); err != nil {
		return nil, err
	}
	if r.merged.BaseIndex != "" {
		return nil, fmt.Errorf("%w: incremental package requires base via OpenChain", ErrCorrupt)
	}
	return r, nil
}

// OpenChain 打开增量包链: pkgs[0] 为基础包, 其后依次为增量包。
func OpenChain(pkgs []ReaderAtSize) (*Reader, error) {
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("assetpack: empty package chain")
	}
	chain := make([]*Reader, len(pkgs))
	for i, p := range pkgs {
		r := &Reader{ra: p, size: p.Size()}
		if err := r.parse(); err != nil {
			return nil, err
		}
		chain[i] = r
	}
	for i := len(chain) - 1; i > 0; i-- {
		chain[i].base = chain[i-1]
	}
	if chain[0].merged.BaseIndex != "" {
		return nil, fmt.Errorf("%w: first package in chain is incremental", ErrCorrupt)
	}
	for i := 1; i < len(chain); i++ {
		prev := chain[i-1].merged
		cur := chain[i].merged
		if cur.BaseIndex == "" {
			return nil, fmt.Errorf("%w: package %d not marked incremental", ErrCorrupt, i)
		}
		if cur.BaseIndex != prev.IndexDigest {
			return nil, fmt.Errorf("%w: chain base index mismatch at %d", ErrCorrupt, i)
		}
		chain[i].merged = mergeManifests(prev, cur)
	}
	return chain[len(chain)-1], nil
}

func (r *Reader) readAt(off int64, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > r.size {
		return nil, fmt.Errorf("%w: read out of bounds", ErrCorrupt)
	}
	b := make([]byte, n)
	if _, err := r.ra.ReadAt(b, off); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

func (r *Reader) parse() error {
	if r.size < int64(len(fileMagic))+trailerLen {
		return fmt.Errorf("%w: too small", ErrCorrupt)
	}
	hdr, err := r.readAt(0, int64(len(fileMagic)))
	if err != nil {
		return err
	}
	if string(hdr) != fileMagic {
		return fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	tail, err := r.readAt(r.size-int64(trailerLen), int64(trailerLen))
	if err != nil {
		return err
	}
	if string(tail[trailerLen-8:]) != endMagic {
		return fmt.Errorf("%w: bad end magic", ErrCorrupt)
	}
	indexOffset := int64(binary.BigEndian.Uint64(tail[0:8]))
	manifestLen := int64(binary.BigEndian.Uint64(tail[8:16]))
	manifestDigest := tail[16:48]
	if indexOffset < 0 || manifestLen < 0 || indexOffset+manifestLen > r.size-int64(trailerLen) {
		return fmt.Errorf("%w: manifest out of bounds", ErrCorrupt)
	}
	mf, err := r.readAt(indexOffset, manifestLen)
	if err != nil {
		return err
	}
	h := sha256.Sum256(mf)
	if !bytes.Equal(h[:], manifestDigest) {
		return fmt.Errorf("%w: manifest digest mismatch", ErrBadDigest)
	}
	var m Manifest
	if err := json.Unmarshal(mf, &m); err != nil {
		return fmt.Errorf("%w: manifest unmarshal: %v", ErrCorrupt, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(mf, &raw); err != nil {
		return err
	}
	delete(raw, "indexDigest")
	cannon, _ := canonicalJSON(raw)
	if hashBytes(cannon) != m.IndexDigest {
		return fmt.Errorf("%w: index identity mismatch", ErrBadDigest)
	}

	pos := int64(len(fileMagic))
	b, err := r.readAt(pos, 4+8+8+8)
	if err != nil {
		return err
	}
	version := binary.BigEndian.Uint32(b[0:4])
	if version != FormatVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrCorrupt, version)
	}
	fragCount := binary.BigEndian.Uint64(b[12:20])
	pos += int64(len(b))

	idx := make(map[string]fragLoc, fragCount)
	for i := uint64(0); i < fragCount; i++ {
		rec, err := r.readAt(pos, 4)
		if err != nil {
			return err
		}
		if string(rec) != fragMagic {
			return fmt.Errorf("%w: bad fragment magic", ErrCorrupt)
		}
		pos += 4
		dlenB, err := r.readAt(pos, 1)
		if err != nil {
			return err
		}
		pos++
		dlen := int64(dlenB[0])
		digB, err := r.readAt(pos, dlen)
		if err != nil {
			return err
		}
		pos += dlen
		lenB, err := r.readAt(pos, 8)
		if err != nil {
			return err
		}
		pos += 8
		dataLen := int64(binary.BigEndian.Uint64(lenB))
		dig := hex.EncodeToString(digB)
		idx[dig] = fragLoc{offset: pos, length: dataLen}
		pos += dataLen
	}
	if pos != indexOffset {
		return fmt.Errorf("%w: fragment section does not align with index", ErrCorrupt)
	}

	r.merged = &m
	r.fragIndex = idx
	return nil
}

// Manifest 返回(已合并包链后的)完整清单副本。
func (r *Reader) Manifest() Manifest { return *r.merged }

// Lookup 按逻辑路径查找资源元数据。
func (r *Reader) Lookup(path string) (Resource, bool) {
	path = cleanLogicalPath(path)
	for _, res := range r.merged.Records() {
		if res.Path == path {
			return res, true
		}
	}
	return Resource{}, false
}

func (r *Reader) loadFragment(digest string) ([]byte, error) {
	if loc, ok := r.fragIndex[digest]; ok {
		data, err := r.readAt(loc.offset, loc.length)
		if err != nil {
			return nil, err
		}
		if hashBytes(data) != digest {
			return nil, fmt.Errorf("%w: corrupt fragment %s", ErrBadDigest, digest[:12])
		}
		return data, nil
	}
	if r.base != nil {
		return r.base.loadFragment(digest)
	}
	return nil, fmt.Errorf("%w: missing fragment %s", ErrCorrupt, digest[:12])
}

// ReadResource 按需组装资源内容, 逐片段核对大小与摘要, 再核对整体身份。
func (r *Reader) ReadResource(path string) ([]byte, error) {
	res, ok := r.Lookup(path)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	var out bytes.Buffer
	for _, fr := range res.Fragments {
		data, err := r.loadFragment(fr.Digest)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != fr.Length {
			return nil, fmt.Errorf("%w: fragment size for %q", ErrBadDigest, path)
		}
		if int64(out.Len()) != fr.Start {
			return nil, fmt.Errorf("%w: non-contiguous fragment in %q", ErrCorrupt, path)
		}
		out.Write(data)
	}
	if int64(out.Len()) != res.Size {
		return nil, fmt.Errorf("%w: assembled size for %q", ErrBadDigest, path)
	}
	if hashBytes(out.Bytes()) != res.ID {
		return nil, fmt.Errorf("%w: content identity for %q", ErrBadDigest, path)
	}
	return out.Bytes(), nil
}

// VerifyAll 核对包链中所有资源的片段、大小与摘要。
func (r *Reader) VerifyAll() error {
	for _, res := range r.merged.Records() {
		if _, err := r.ReadResource(res.Path); err != nil {
			return err
		}
	}
	return nil
}

// Records 返回清单中的资源(已排序)。
func (m *Manifest) Records() []Resource {
	out := append([]Resource(nil), m.Resources...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// mergeManifests 以 cur 覆盖 base 中同路径资源, 产出新版本完整清单。
func mergeManifests(base, cur *Manifest) *Manifest {
	byPath := map[string]Resource{}
	var order []string
	for _, res := range base.Resources {
		if _, ok := byPath[res.Path]; !ok {
			order = append(order, res.Path)
		}
		byPath[res.Path] = res
	}
	for _, res := range cur.Resources {
		if res.Type == "removed" {
			delete(byPath, res.Path)
			continue
		}
		if _, ok := byPath[res.Path]; !ok {
			order = append(order, res.Path)
		}
		byPath[res.Path] = res
	}
	sort.Strings(order)
	out := &Manifest{
		Version:      cur.Version,
		FragmentSize: cur.FragmentSize,
		BaseIndex:    base.IndexDigest,
		IndexDigest:  cur.IndexDigest,
	}
	for _, p := range order {
		if res, ok := byPath[p]; ok {
			out.Resources = append(out.Resources, res)
		}
	}
	return out
}
