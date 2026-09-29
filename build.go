package assetpack

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// BuildPackage creates a complete package and returns its published manifest.
func BuildPackage(output string, inputs []InputResource) (Manifest, error) {
	return BuildDelta(output, nil, 1, inputs)
}

// BuildDelta creates a package relative to base.
func BuildDelta(output string, base *Manifest, version int, inputs []InputResource) (Manifest, error) {
	m, chunkData, err := buildManifest(base, version, inputs)
	if err != nil {
		return Manifest{}, err
	}
	if err := writePackage(output, &m, chunkData); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

type aggregatedResource struct {
	id       string
	paths    []string
	size     int64
	chunks   []string
	indices  map[int]bool
	children map[string]string
}

func buildManifest(base *Manifest, version int, inputs []InputResource) (Manifest, map[string][]byte, error) {
	if version < 1 {
		return Manifest{}, nil, fmt.Errorf("version must be positive")
	}
	if len(inputs) == 0 {
		return Manifest{}, nil, fmt.Errorf("cannot publish an empty package")
	}

	byContent := make(map[string]*aggregatedResource)
	inputIDs := make([]string, len(inputs))
	allChunks := make(map[string][]byte)
	pathOwners := make(map[string]string)

	for i, in := range inputs {
		if len(in.Paths) == 0 {
			return Manifest{}, nil, fmt.Errorf("resource %d has no path", i)
		}
		rid := ContentID(in.Data)
		inputIDs[i] = rid
		agg, ok := byContent[rid]
		if !ok {
			agg = &aggregatedResource{
				id:       rid,
				size:     int64(len(in.Data)),
				indices:  make(map[int]bool),
				children: make(map[string]string),
			}
			var chunkIDs []string
			for off := 0; off < len(in.Data); off += chunkSize {
				end := off + chunkSize
				if end > len(in.Data) {
					end = len(in.Data)
				}
				part := in.Data[off:end]
				cid := rawDigest(part)
				chunkIDs = append(chunkIDs, cid)
				allChunks[cid] = append([]byte(nil), part...)
			}
			agg.chunks = chunkIDs
			byContent[rid] = agg
		}
		agg.indices[i] = true
		for _, p := range in.Paths {
			if err := ValidateLogicalPath(p); err != nil {
				return Manifest{}, nil, err
			}
			if owner, exists := pathOwners[p]; exists && owner != rid {
				return Manifest{}, nil, fmt.Errorf("path %q assigned to multiple resources", p)
			}
			pathOwners[p] = rid
			agg.paths = appendUnique(agg.paths, p)
		}
	}

	for i, in := range inputs {
		agg := byContent[inputIDs[i]]
		for _, dep := range in.Deps {
			if dep < 0 || dep >= len(inputs) {
				return Manifest{}, nil, fmt.Errorf("resource %d has invalid dependency index %d", i, dep)
			}
			agg.indices[i] = true
		}
		for _, child := range in.Children {
			if child.Ref < 0 || child.Ref >= len(inputs) {
				return Manifest{}, nil, fmt.Errorf("resource %d has invalid nested index", i)
			}
			if child.Name == "" {
				return Manifest{}, nil, fmt.Errorf("resource %d has unnamed nested resource", i)
			}
			if err := ValidateLogicalPath(child.Name); err != nil {
				return Manifest{}, nil, err
			}
			target := inputIDs[child.Ref]
			if existing, exists := agg.children[child.Name]; exists && existing != target {
				return Manifest{}, nil, fmt.Errorf("resource %d has conflicting nested resource named %q", i, child.Name)
			}
			agg.children[child.Name] = target
		}
	}

	resources := make([]Resource, 0, len(byContent))
	for _, agg := range byContent {
		deps := make(map[string]bool)
		for idx := range agg.indices {
			for _, dep := range inputs[idx].Deps {
				deps[inputIDs[dep]] = true
			}
		}
		r := Resource{
			ID:     agg.id,
			Paths:  append([]string(nil), agg.paths...),
			Size:   agg.size,
			Chunks: append([]string(nil), agg.chunks...),
		}
		for dep := range deps {
			r.Deps = append(r.Deps, dep)
		}
		for name, id := range agg.children {
			r.Children = append(r.Children, ChildRef{Name: name, ID: id})
		}
		sort.Strings(r.Paths)
		sort.Strings(r.Deps)
		sort.Slice(r.Children, func(a, b int) bool { return r.Children[a].Name < r.Children[b].Name })
		resources = append(resources, r)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].ID < resources[j].ID })
	pathList := make([]string, 0, len(pathOwners))
	for name := range pathOwners {
		pathList = append(pathList, name)
	}
	if err := ValidatePathTree(pathList); err != nil {
		return Manifest{}, nil, err
	}

	baseChunks := map[string]bool{}
	var baseID string
	if base != nil {
		baseID = base.PackageID
		for _, c := range base.Chunks {
			baseChunks[c.Digest] = true
		}
	}

	chunkIDs := make([]string, 0, len(allChunks))
	includedChunkData := make(map[string][]byte)
	for cid := range allChunks {
		if !baseChunks[cid] {
			chunkIDs = append(chunkIDs, cid)
			includedChunkData[cid] = allChunks[cid]
		}
	}
	sort.Strings(chunkIDs)
	chunks := make([]ChunkInfo, 0, len(chunkIDs))
	offset := int64(len(headerPrefix))
	for _, cid := range chunkIDs {
		data := allChunks[cid]
		chunks = append(chunks, ChunkInfo{Digest: cid, Offset: offset, Size: int64(len(data))})
		offset += int64(len(data))
	}

	m := Manifest{
		Version:   version,
		Chunks:    chunks,
		Resources: resources,
		BaseID:    baseID,
	}
	id, err := manifestID(&m)
	if err != nil {
		return Manifest{}, nil, err
	}
	m.PackageID = id
	return m, includedChunkData, nil
}

func writePackage(output string, m *Manifest, chunkData map[string][]byte) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	compressed, err := encodeIndex(raw)
	if err != nil {
		return err
	}

	dir := filepath.Dir(output)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".assetpack-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	w := newPackageWriter(tmp)
	if err := w.write(headerPrefix); err != nil {
		return err
	}
	for _, c := range m.Chunks {
		if _, ok := chunkData[c.Digest]; !ok {
			return fmt.Errorf("internal packer error: missing data for chunk %s", c.Digest)
		}
		if err := w.write(chunkData[c.Digest]); err != nil {
			return err
		}
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(compressed)))
	if err := w.write(compressed); err != nil {
		return err
	}
	if err := w.write(trailerMagic); err != nil {
		return err
	}
	if err := w.write(length[:]); err != nil {
		return err
	}
	if err := w.write([]byte(rawDigest(compressed))); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, output); err != nil {
		return err
	}
	committed = true
	return nil
}

type packageWriter struct {
	w   io.Writer
	err error
}

func newPackageWriter(w io.Writer) *packageWriter { return &packageWriter{w: w} }

func (pw *packageWriter) write(p []byte) error {
	if pw.err != nil {
		return pw.err
	}
	_, pw.err = pw.w.Write(p)
	return pw.err
}

func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
