package assetpack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// Unpack 先在内存/临时区核对全部大小与摘要, 通过后再原子发布到 root。
// 所有素材先落盘, 最后统一写出引用关系, 因此循环依赖可被完整恢复。
func Unpack(r *Reader, root string) (*UnpackResult, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
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
		}
	}

	// 阶段二: 路径越界全部在写入前一次性拒绝。
	targets := make([]string, len(stagedList))
	for i, st := range stagedList {
		full, err := safeJoin(root, st.res.Path)
		if err != nil {
			return nil, err
		}
		targets[i] = full
	}

	// 阶段三: 发布素材(目录建目录, 文件原子写)。先全部就位再写关系索引。
	res := &UnpackResult{Records: records}
	for i, st := range stagedList {
		if st.res.Type == "dir" {
			if err := os.MkdirAll(targets[i], 0o755); err != nil {
				return nil, err
			}
			res.Directories++
			continue
		}
		if err := WriteFileAtomic(targets[i], st.data, 0o644); err != nil {
			return nil, err
		}
		res.Files++
	}

	// 阶段四: 原子发布索引(身份 + 引用关系, 含循环依赖)。包内容未全部核对前不写它。
	meta := struct {
		IndexDigest string               `json:"indexDigest"`
		Records     map[string]DepRecord `json:"records"`
	}{m.IndexDigest, records}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(filepath.Join(root, metaDir, "manifest.json"), mb, 0o644); err != nil {
		return nil, err
	}
	return res, nil
}
