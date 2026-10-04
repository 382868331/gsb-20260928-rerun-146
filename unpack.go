package assetpack

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// metaDir 是解包后保存资源身份与引用关系的元数据目录(逻辑路径禁止占用)。
const metaDir = ".assetpack"

// DepRecord 记录单个素材的内容身份与它引用的其它素材。
type DepRecord struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Size      int64    `json:"size"`
	DependsOn []string `json:"dependsOn"`
	Source    string   `json:"source,omitempty"`
}

type installedManifest struct {
	IndexDigest string               `json:"indexDigest"`
	Records     map[string]DepRecord `json:"records"`
}

// UnpackResult 汇总解包结果。
type UnpackResult struct {
	Files       int
	Directories int
	Records     map[string]DepRecord
}

// safeJoin 把逻辑路径约束在根目录内, 拒绝任何越界。
func safeJoin(root, logical string) (string, error) {
	clean := cleanLogicalPath(logical)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: %q", ErrPathTraversal, logical)
	}
	if strings.HasPrefix(clean, metaDir+"/") || clean == metaDir {
		return "", fmt.Errorf("%w: reserved name %q", ErrPathTraversal, clean)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q escapes root", ErrPathTraversal, logical)
	}
	return full, nil
}

func inRootName(logical string) (string, error) {
	clean := cleanLogicalPath(logical)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: %q", ErrPathTraversal, logical)
	}
	if clean == metaDir || strings.HasPrefix(clean, metaDir+"/") {
		return "", fmt.Errorf("%w: reserved name %q", ErrPathTraversal, clean)
	}
	return filepath.FromSlash(clean), nil
}

func validateResources(records []Resource) ([]Resource, error) {
	out := make([]Resource, 0, len(records))
	for _, res := range records {
		if res.Type == "delete" {
			if _, err := inRootName(res.Path); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, res)
	}
	sortResources(out)
	seen := make(map[string]bool, len(out))
	files := make(map[string]bool, len(out))
	dirs := map[string]bool{"": true}
	for _, res := range out {
		name, err := inRootName(res.Path)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: duplicate resource path %q", ErrPathTraversal, res.Path)
		}
		seen[name] = true

		switch res.Type {
		case "dir":
			if files[name] {
				return nil, fmt.Errorf("%w: resource %q conflicts with a file", ErrPathTraversal, res.Path)
			}
			dirs[name] = true
		case "file":
			if dirs[name] {
				return nil, fmt.Errorf("%w: resource %q conflicts with a directory", ErrPathTraversal, res.Path)
			}
			parent := filepath.Dir(name)
			if files[parent] {
				return nil, fmt.Errorf("%w: resource %q conflicts with another resource", ErrPathTraversal, res.Path)
			}
			for part := parent; part != "" && part != "."; part = filepath.Dir(part) {
				if files[part] {
					return nil, fmt.Errorf("%w: resource %q is nested inside file %q", ErrPathTraversal, res.Path, filepath.ToSlash(part))
				}
			}
			files[name] = true
		default:
			return nil, fmt.Errorf("assetpack: unsupported resource type %q for %q", res.Type, res.Path)
		}
	}
	return out, nil
}

func sortResources(resources []Resource) {
	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
}

func preflightRoot(root *os.Root, resources []Resource) error {
	for _, res := range resources {
		name, err := inRootName(res.Path)
		if err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: resolve %q: %v", ErrPathTraversal, res.Path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if _, err := root.Stat(name); err != nil {
				return fmt.Errorf("%w: symlink %q escapes root: %v", ErrPathTraversal, res.Path, err)
			}
		}
	}
	return nil
}

func writeFileInRoot(root *os.Root, name string, data []byte, perm os.FileMode) (err error) {
	parent := filepath.Dir(name)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("%w: create parent for %q: %v", ErrPathTraversal, name, err)
		}
	}

	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmpName := filepath.Join(parent, ".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create temporary file for %q: %v", ErrPathTraversal, name, err)
	}
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			f.Close()
		}
		root.Remove(tmpName)
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		tmpClosed = true
		return err
	}
	tmpClosed = true
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("%w: publish %q: %v", ErrPathTraversal, name, err)
	}
	return nil
}

// Unpack 先在内存/临时区核对全部大小与摘要, 通过后再原子发布到 root。
// 所有素材先落盘, 最后统一写出引用关系, 因此循环依赖可被完整恢复。
func Unpack(r *Reader, root string) (*UnpackResult, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	targetRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer targetRoot.Close()
	m := r.Manifest()

	// 阶段一: 读取并核对所有资源(大小、片段摘要、整体身份)。
	type staged struct {
		res  Resource
		data []byte
	}
	stagedList := make([]staged, 0, len(m.Resources))
	records := make(map[string]DepRecord, len(m.Resources))
	for _, res := range m.Records() {
		data, err := r.ReadResource(res.Path)
		if err != nil {
			return nil, fmt.Errorf("verify %q: %w", res.Path, err)
		}
		stagedList = append(stagedList, staged{res: res, data: data})
		records[res.Path] = DepRecord{
			ID:        res.ID,
			Type:      res.Type,
			Size:      res.Size,
			DependsOn: append([]string(nil), res.DependsOn...),
			Source:    SourceOriginal,
		}
	}

	// 阶段二: 路径、资源层级冲突和真实落盘位置全部在写入前一次性拒绝。
	resources := make([]Resource, len(stagedList))
	for i, st := range stagedList {
		resources[i] = st.res
	}
	resources, err = validateResources(resources)
	if err != nil {
		return nil, err
	}
	if err := preflightRoot(targetRoot, resources); err != nil {
		return nil, err
	}
	stagedByPath := make(map[string]staged, len(stagedList))
	for _, st := range stagedList {
		name, _ := inRootName(st.res.Path)
		stagedByPath[name] = st
	}

	// 阶段三: 发布素材(目录建目录, 文件原子写)。先全部就位再写关系索引。
	res := &UnpackResult{Records: records}
	for _, resource := range resources {
		name, _ := inRootName(resource.Path)
		st := stagedByPath[name]
		if st.res.Type == "dir" {
			if err := targetRoot.MkdirAll(name, 0o755); err != nil {
				return nil, fmt.Errorf("%w: create directory %q: %v", ErrPathTraversal, st.res.Path, err)
			}
			res.Directories++
			continue
		}
		if err := writeFileInRoot(targetRoot, name, st.data, 0o644); err != nil {
			return nil, err
		}
		res.Files++
	}

	// 阶段四: 原子发布索引(身份 + 引用关系, 含循环依赖)。包内容未全部核对前不写它。
	mb, err := json.MarshalIndent(installedManifest{IndexDigest: m.IndexDigest, Records: records}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := targetRoot.MkdirAll(metaDir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: create metadata directory: %v", ErrPathTraversal, err)
	}
	indexName := filepath.Join(metaDir, "manifest.json")
	if err := writeFileInRoot(targetRoot, indexName, mb, 0o644); err != nil {
		return nil, err
	}
	return res, nil
}
