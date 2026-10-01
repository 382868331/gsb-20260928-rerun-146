package assetpack

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// checkLogicalPath 在写入前对包内逻辑路径做词法边界校验, 返回相对 root 的 slash 路径。
// 真正的落盘边界由发布阶段的 os.Root 句柄以内核语义保证, 不依赖字符串比较。
func checkLogicalPath(logical string) (string, error) {
	clean := cleanLogicalPath(logical)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: %q", ErrPathTraversal, logical)
	}
	if clean == metaDir || strings.HasPrefix(clean, metaDir+"/") {
		return "", fmt.Errorf("%w: reserved name %q", ErrPathTraversal, clean)
	}
	return clean, nil
}

// errSymlinkLoop 是符号链接解析成环时的系统错误。
var errSymlinkLoop = syscall.ELOOP

// rootPathError 把逃逸类系统错误统一包装为 ErrPathTraversal。
// os.Root 经 openat2(RESOLVE_BENEATH) 解析路径, 任何指向 root 外的符号链接
// (含操作中途被替换的链接) 都会以权限错误或 ELOOP 失败, 此处如实上抛。
func rootPathError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, errSymlinkLoop) ||
		strings.Contains(err.Error(), "path escapes") {
		return fmt.Errorf("%w: %s: %v", ErrPathTraversal, op, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ensureRealDir 确保 rel 是 root 句柄之下的真实目录。
// 对已存在条目, 通过父目录的目录项(DirEntry)判定类型而不解析符号链接目标:
// 这样即使链接指向 root 之外(或悬空), 也只会被当作符号链接拒绝, 不会触发
// 任何跟随行为。
func ensureRealDir(rr *os.Root, rel string) error {
	if err := rr.Mkdir(rel, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return rootPathError("mkdir "+rel, err)
		}
		return inspectExistingDir(rr, rel)
	}
	return nil
}

// inspectExistingDir 经目录项信息确认 rel 是真实目录(符号链接或普通文件一律拒绝)。
func inspectExistingDir(rr *os.Root, rel string) error {
	parent := filepath.ToSlash(filepath.Dir(rel))
	base := filepath.Base(rel)
	d, err := rr.Open(parent)
	if err != nil {
		return rootPathError("open dir "+parent, err)
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return rootPathError("read dir "+parent, err)
	}
	for _, e := range ents {
		if e.Name() != base {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q is a symbolic link, not a directory", ErrPathTraversal, rel)
		}
		if !e.IsDir() {
			return fmt.Errorf("assetpack: %q exists and is not a directory", rel)
		}
		return nil
	}
	// 条目瞬间消失: 重试创建即可。
	if err := rr.Mkdir(rel, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return rootPathError("mkdir "+rel, err)
	}
	return nil
}

// ensureDirsWithinRoot 沿 rel 逐级确保目录存在且均为 root 内真实目录。
func ensureDirsWithinRoot(rr *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	cur := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if err := ensureRealDir(rr, cur); err != nil {
			return err
		}
	}
	return nil
}

func stageDirName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".stage-" + hex.EncodeToString(b[:]), nil
}

// Unpack 先在内存核对全部大小与摘要, 再把所有素材写入 root 内的独占暂存区,
// 最后经受限于 root 的目录句柄原子发布; 关系索引一定在最后落盘。
// 目标目录树中预置或中途替换的符号链接都无法把写入带出 root: 落盘边界检查
// 由内核按真实路径逐级执行, 而不是对路径字符串做比较。
func Unpack(r *Reader, root string) (*UnpackResult, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, err
	}
	rr, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, err
	}
	defer rr.Close()
	m := r.Manifest()

	// 阶段一: 读取并核对所有资源(大小、片段摘要、整体身份)。此阶段不触碰磁盘。
	type staged struct {
		res  Resource
		data []byte
		rel  string // 相对 root 的 slash 路径
	}
	stagedList := make([]staged, 0, len(m.Resources))
	records := make(map[string]DepRecord, len(m.Resources))
	for _, res := range m.Records() {
		data, err := r.ReadResource(res.Path)
		if err != nil {
			return nil, fmt.Errorf("verify %q: %w", res.Path, err)
		}
		rel, err := checkLogicalPath(res.Path)
		if err != nil {
			return nil, err
		}
		if _, dup := records[res.Path]; dup {
			return nil, fmt.Errorf("%w: duplicate resource path %q", ErrPathTraversal, res.Path)
		}
		stagedList = append(stagedList, staged{res: res, data: data, rel: rel})
		records[res.Path] = DepRecord{
			ID:        res.ID,
			Type:      res.Type,
			Size:      res.Size,
			DependsOn: append([]string(nil), res.DependsOn...),
		}
	}

	// 阶段二: 在 root 内建立独占暂存区(位于保留名 metaDir 下, 包内资源无法占用)。
	// 所有素材先落暂存区, 任一步失败都不会发布关系索引, 也不会触碰 root 之外。
	if err := ensureRealDir(rr, metaDir); err != nil {
		return nil, err
	}
	stageName, err := stageDirName()
	if err != nil {
		return nil, err
	}
	stageRel := metaDir + "/" + stageName
	if err := rr.Mkdir(stageRel, 0o755); err != nil {
		return nil, rootPathError("mkdir staging", err)
	}
	defer rr.RemoveAll(stageRel)

	// 阶段三: 素材写入暂存区(目录建目录, 文件经 root 句柄写)。
	res := &UnpackResult{Records: records}
	for _, st := range stagedList {
		stageTarget := stageRel + "/" + st.rel
		if err := ensureDirsWithinRoot(rr, filepath.ToSlash(filepath.Dir(stageTarget))); err != nil {
			return nil, err
		}
		if st.res.Type == "dir" {
			if err := ensureRealDir(rr, stageTarget); err != nil {
				return nil, err
			}
			res.Directories++
			continue
		}
		if err := rr.WriteFile(stageTarget, st.data, 0o644); err != nil {
			return nil, rootPathError("stage write "+st.rel, err)
		}
		res.Files++
	}

	// 关系索引也先生成到暂存区, 随素材一起在最后发布。
	meta := struct {
		IndexDigest string               `json:"indexDigest"`
		Records     map[string]DepRecord `json:"records"`
	}{m.IndexDigest, records}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := rr.WriteFile(stageRel+"/manifest.json", mb, 0o644); err != nil {
		return nil, rootPathError("stage manifest", err)
	}

	// 阶段四: 经 root 句柄发布。先目录后文件, 最后才发布关系索引。
	// ensureRealDir 拒绝任何符号链接目录; Rename 在同一 root 句柄内完成,
	// 内核逐级校验真实路径: 父级是逃逸链接即失败, 末级是链接则替换链接本身
	// (不跟随), 因此逐级目录变化或链接替换都无法把内容带出 root。
	for _, st := range stagedList {
		if st.res.Type != "dir" {
			continue
		}
		if err := publishDirs(rr, st.rel); err != nil {
			return nil, err
		}
	}
	for _, st := range stagedList {
		if st.res.Type == "dir" {
			continue
		}
		parent := filepath.ToSlash(filepath.Dir(st.rel))
		if parent != "." {
			if err := publishDirs(rr, parent); err != nil {
				return nil, err
			}
		}
		stageTarget := stageRel + "/" + st.rel
		if err := rr.Rename(stageTarget, st.rel); err != nil {
			return nil, rootPathError("publish "+st.rel, err)
		}
	}
	if err := rr.Rename(stageRel+"/manifest.json", metaDir+"/manifest.json"); err != nil {
		return nil, rootPathError("publish manifest", err)
	}
	return res, nil
}

func publishDirs(rr *os.Root, rel string) error {
	return ensureDirsWithinRoot(rr, rel)
}
