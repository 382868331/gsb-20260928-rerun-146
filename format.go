package assetpack

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
)

const (
	formatMagic  = "DAP1"
	indexMagic   = "DAPIDX1"
	maxIndexSize = 256 * 1024 * 1024
	hexDgstLen   = 64
)

var (
	headerPrefix = []byte(formatMagic + "\n")
	trailerMagic = []byte("\n" + indexMagic)
)

// Package is an opened, verified package.
type Package struct {
	file     *os.File
	manifest Manifest
}

// Open validates the package trailer and manifest without reading all chunks.
func Open(path string) (*Package, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	p := &Package{file: f}
	if err := p.loadIndex(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return p, nil
}

func (p *Package) Close() error { return p.file.Close() }

// Manifest returns a copy of the published index.
func (p *Package) Manifest() Manifest {
	m := p.manifest
	m.Chunks = append([]ChunkInfo(nil), p.manifest.Chunks...)
	m.Resources = append([]Resource(nil), p.manifest.Resources...)
	return m
}

func (p *Package) loadIndex() error {
	info, err := p.file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	fixed := len(trailerMagic) + 8 + hexDgstLen
	if size < int64(len(headerPrefix)+fixed) {
		return fmt.Errorf("invalid package: too small")
	}

	head := make([]byte, len(headerPrefix))
	if _, err := p.file.ReadAt(head, 0); err != nil {
		return err
	}
	if !bytes.Equal(head, headerPrefix) {
		return fmt.Errorf("invalid package: bad magic")
	}

	tail := make([]byte, fixed)
	if _, err := p.file.ReadAt(tail, size-int64(fixed)); err != nil {
		return err
	}
	if !bytes.HasPrefix(tail, trailerMagic) {
		return fmt.Errorf("invalid package: missing index marker")
	}
	indexLen := binary.BigEndian.Uint64(tail[len(trailerMagic) : len(trailerMagic)+8])
	indexDigest := string(tail[len(trailerMagic)+8:])
	if indexLen == 0 || indexLen > maxIndexSize {
		return fmt.Errorf("invalid package: index length out of range")
	}
	indexEnd := size - int64(fixed)
	indexStart := indexEnd - int64(indexLen)
	if indexStart < int64(len(headerPrefix)) {
		return fmt.Errorf("invalid package: index overlaps header")
	}

	compressed := make([]byte, indexLen)
	if _, err := p.file.ReadAt(compressed, indexStart); err != nil {
		return err
	}
	if rawDigest(compressed) != indexDigest {
		return fmt.Errorf("invalid package: index checksum mismatch")
	}
	raw, err := decodeIndex(compressed)
	if err != nil {
		return fmt.Errorf("invalid package: corrupt index: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("invalid package: malformed manifest: %w", err)
	}
	if err := p.validateManifest(&m); err != nil {
		return err
	}
	p.manifest = m
	return nil
}

func (p *Package) validateManifest(m *Manifest) error {
	if !validDigest(m.PackageID) {
		return fmt.Errorf("invalid package: bad package id")
	}
	if m.Version < 1 {
		return fmt.Errorf("invalid package: version must be positive")
	}
	if m.BaseID != "" && !validDigest(m.BaseID) {
		return fmt.Errorf("invalid package: bad base id")
	}

	info, err := p.file.Stat()
	if err != nil {
		return err
	}
	fixed := len(trailerMagic) + 8 + hexDgstLen
	dataEnd := info.Size()
	var indexLen uint64
	tail := make([]byte, fixed)
	if _, err := p.file.ReadAt(tail, info.Size()-int64(fixed)); err != nil {
		return err
	}
	indexLen = binary.BigEndian.Uint64(tail[len(trailerMagic) : len(trailerMagic)+8])
	dataEnd -= int64(fixed) + int64(indexLen)

	localChunks := make(map[string]ChunkInfo, len(m.Chunks))
	var prev string
	dataCursor := int64(len(headerPrefix))
	for i, c := range m.Chunks {
		if !validDigest(digestID(c.Digest)) || len(c.Digest) != hexDgstLen {
			return fmt.Errorf("invalid package: bad chunk digest")
		}
		if _, err := hex.DecodeString(c.Digest); err != nil {
			return fmt.Errorf("invalid package: bad chunk digest")
		}
		if i > 0 && c.Digest <= prev {
			return fmt.Errorf("invalid package: chunks are not ordered")
		}
		end := c.Offset + c.Size
		if c.Offset < int64(len(headerPrefix)) || c.Size < 0 || end > dataEnd {
			return fmt.Errorf("invalid package: illegal chunk location")
		}
		if c.Offset != dataCursor {
			return fmt.Errorf("invalid package: chunks overlap or leave a gap")
		}
		localChunks[c.Digest] = c
		prev = c.Digest
		dataCursor = end
	}

	seenResources := make(map[string]bool, len(m.Resources))
	seenPaths := make(map[string]bool)
	prev = ""
	for i, r := range m.Resources {
		if !validDigest(r.ID) {
			return fmt.Errorf("invalid package: bad resource id")
		}
		if i > 0 && r.ID <= prev {
			return fmt.Errorf("invalid package: resources are not ordered")
		}
		if seenResources[r.ID] {
			return fmt.Errorf("invalid package: duplicate resource %s", r.ID)
		}
		seenResources[r.ID] = true
		if len(r.Paths) == 0 {
			return fmt.Errorf("invalid package: resource %s has no path", r.ID)
		}
		var localSize int64
		for _, path := range r.Paths {
			if err := ValidateLogicalPath(path); err != nil {
				return fmt.Errorf("invalid package: %w", err)
			}
			if seenPaths[path] {
				return fmt.Errorf("invalid package: duplicate path %s", path)
			}
			seenPaths[path] = true
		}
		for _, cid := range r.Chunks {
			if len(cid) != hexDgstLen {
				return fmt.Errorf("invalid package: bad chunk reference")
			}
			if c, ok := localChunks[cid]; ok {
				localSize += c.Size
			} else if m.BaseID == "" {
				return fmt.Errorf("invalid package: missing chunk %s", cid)
			}
		}
		if localSize > r.Size {
			return fmt.Errorf("invalid package: resource %s size mismatch", r.ID)
		}
		prev = r.ID
	}

	for _, r := range m.Resources {
		for _, dep := range r.Deps {
			if !seenResources[dep] {
				return fmt.Errorf("invalid package: dangling dependency %s", dep)
			}
		}
		for _, child := range r.Children {
			if child.Name == "" || !seenResources[child.ID] {
				return fmt.Errorf("invalid package: dangling nested resource")
			}
			if err := ValidateLogicalPath(child.Name); err != nil {
				return fmt.Errorf("invalid package: invalid child name: %w", err)
			}
		}
	}
	pathList := make([]string, 0, len(seenPaths))
	for name := range seenPaths {
		pathList = append(pathList, name)
	}
	if err := ValidatePathTree(pathList); err != nil {
		return err
	}

	want, err := manifestID(m)
	if err != nil {
		return err
	}
	if want != m.PackageID {
		return fmt.Errorf("invalid package: manifest id mismatch")
	}
	return nil
}

// ReadResource verifies every fragment and whole-resource digest while reading.
func (p *Package) ReadResource(id string) (io.Reader, error) {
	r, err := p.findResource(id)
	if err != nil {
		return nil, err
	}
	readers := make([]io.Reader, 0, len(r.Chunks))
	for _, cid := range r.Chunks {
		c, err := p.lookupChunk(cid)
		if err != nil {
			return nil, err
		}
		readers = append(readers, &chunkVerifier{
			r:    io.NewSectionReader(p.file, c.Offset, c.Size),
			h:    sha256.New(),
			want: cid,
			size: c.Size,
		})
	}
	return &verifiedReader{readers: readers, want: r.ID[len("sha256:"):], size: r.Size}, nil
}

// ReadBytes reads and verifies one resource into memory.
func (p *Package) ReadBytes(id string) ([]byte, error) {
	r, err := p.ReadResource(id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// ReadPath resolves a logical path and verifies the selected resource.
func (p *Package) ReadPath(name string) ([]byte, error) {
	id, err := p.PathID(name)
	if err != nil {
		return nil, err
	}
	return p.ReadBytes(id)
}

// PathID returns the shared content identity behind a logical path.
func (p *Package) PathID(name string) (string, error) {
	for _, r := range p.manifest.Resources {
		for _, candidate := range r.Paths {
			if candidate == name {
				return r.ID, nil
			}
		}
	}
	return "", fmt.Errorf("unknown resource path %q", name)
}

func (p *Package) findResource(id string) (Resource, error) {
	lo, hi := 0, len(p.manifest.Resources)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case p.manifest.Resources[mid].ID < id:
			lo = mid + 1
		case p.manifest.Resources[mid].ID > id:
			hi = mid
		default:
			return p.manifest.Resources[mid], nil
		}
	}
	return Resource{}, fmt.Errorf("unknown resource %q", id)
}

func (p *Package) lookupChunk(digest string) (ChunkInfo, error) {
	if c, ok := chunkIndex(p.manifest.Chunks, digest); ok {
		return c, nil
	}
	return ChunkInfo{}, fmt.Errorf("chunk %s not found in this package (base package required)", digest)
}

type verifiedReader struct {
	readers []io.Reader
	h       hash.Hash
	want    string
	size    int64
	read    int64
}

type chunkVerifier struct {
	r    io.Reader
	h    hash.Hash
	want string
	size int64
	read int64
}

func (cv *chunkVerifier) Read(p []byte) (int, error) {
	n, err := cv.r.Read(p)
	if n > 0 {
		cv.h.Write(p[:n])
		cv.read += int64(n)
	}
	if err == io.EOF {
		if cv.read != cv.size || hex.EncodeToString(cv.h.Sum(nil)) != cv.want {
			return n, fmt.Errorf("corrupt chunk %s: size or digest mismatch", cv.want)
		}
	}
	return n, err
}

func (vr *verifiedReader) Read(p []byte) (int, error) {
	if len(vr.readers) == 0 {
		if vr.h == nil {
			vr.h = sha256.New()
		}
		if vr.read != vr.size {
			return 0, fmt.Errorf("corrupt resource: expected %d bytes, got %d", vr.size, vr.read)
		}
		if got := hex.EncodeToString(vr.h.Sum(nil)); got != vr.want {
			return 0, fmt.Errorf("corrupt resource: digest mismatch")
		}
		return 0, io.EOF
	}
	if vr.h == nil {
		vr.h = sha256.New()
	}
	n, err := vr.readers[0].Read(p)
	if n > 0 {
		vr.h.Write(p[:n])
		vr.read += int64(n)
	}
	if err == io.EOF {
		vr.readers = vr.readers[1:]
		err = nil
	}
	if err != nil {
		return n, err
	}
	if vr.read > vr.size {
		return n, fmt.Errorf("corrupt resource: exceeds declared size")
	}
	return n, nil
}

func chunkIndex(chunks []ChunkInfo, digest string) (ChunkInfo, bool) {
	lo, hi := 0, len(chunks)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case chunks[mid].Digest < digest:
			lo = mid + 1
		case chunks[mid].Digest > digest:
			hi = mid
		default:
			return chunks[mid], true
		}
	}
	return ChunkInfo{}, false
}

func encodeIndex(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeIndex(compressed []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	return io.ReadAll(r)
}
