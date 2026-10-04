package assetpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// 更新预览条目状态。
const (
	ChangeAdd       = "add"       // 包内新增, 本地不存在
	ChangeReplace   = "replace"   // 本地与安装记录一致, 可直接替换
	ChangeConflict  = "conflict"  // 本地已改动, 与安装记录不一致
	ChangeRemove    = "remove"    // 包内删除, 仅存在于本地安装
	ChangeUnchanged = "unchanged" // 内容身份一致, 无需动作
)

// 更新决策。
const (
	DecidePack   = "pack"   // 采用包内版本
	DecideKeep   = "keep"   // 保留本地版本
	DecideRemove = "remove" // 接受包内删除
)

// 最终清单中每个资源的来源标注。
const (
	OriginInstalled = "installed" // 来自原安装, 本次未变
	OriginKept      = "kept"      // 本地保留(人工选择)
	OriginUpdated   = "updated"   // 来自此次更新
)

// ErrPreviewStale 表示采用前复核发现预览依据已变化, 需要重新决定。
var ErrPreviewStale = errors.New("assetpack: preview basis changed, re-decide required")

// ErrUndecided 表示存在尚未决策的条目。
var ErrUndecided = errors.New("assetpack: update plan has undecided entries")

// ErrUpdateInProgress 表示存在中断的更新, 需要先接续或撤回。
var ErrUpdateInProgress = errors.New("assetpack: interrupted update pending, resume or rollback first")

// PlanEntry 描述一个资源在更新中的状态与人工决策。
type PlanEntry struct {
	Path        string   `json:"path"`
	Change      string   `json:"change"`
	InstalledID string   `json:"installedId,omitempty"`
	LocalID     string   `json:"localId,omitempty"`
	PackID      string   `json:"packId,omitempty"`
	Decision    string   `json:"decision,omitempty"`
	Impacts     []string `json:"impacts,omitempty"`
	ImpactNote  string   `json:"impactNote,omitempty"`
}

// UpdatePlan 是一次更新预览, 也是采用时的依据。
type UpdatePlan struct {
	PackIndex string                `json:"packIndex"` // 包(链)索引身份
	BaseIndex string                `json:"baseIndex"` // 本地安装索引身份
	Entries   map[string]*PlanEntry `json:"entries"`
}

// installedMeta 是 .assetpack/manifest.json 的结构。
type installedMeta struct {
	IndexDigest string               `json:"indexDigest"`
	PackIndex   string               `json:"packIndex,omitempty"`
	Records     map[string]DepRecord `json:"records"`
}

func readInstalledMeta(root *os.Root) (*installedMeta, error) {
	f, err := root.Open(filepath.Join(metaDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read installed index: %w", err)
	}
	defer f.Close()
	var meta installedMeta
	if err := json.NewDecoder(f).Decode(&meta); err != nil {
		return nil, fmt.Errorf("parse installed index: %w", err)
	}
	if meta.Records == nil {
		meta.Records = map[string]DepRecord{}
	}
	return &meta, nil
}

// localFileID 在根目录约束内计算本地文件内容身份。
func localFileID(root *os.Root, logical, resType string) (id string, exists bool, err error) {
	name, err := inRootName(logical)
	if err != nil {
		return "", false, err
	}
	info, err := root.Stat(name)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: resolve %q: %v", ErrPathTraversal, logical, err)
	}
	if resType == "dir" {
		return "", info.IsDir(), nil
	}
	if !info.Mode().IsRegular() {
		return "", true, nil
	}
	f, err := root.Open(name)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// PreviewUpdate 对比包(链)清单与本地安装, 产出更新预览。
// 所有本地访问都约束在 root 内, 越界路径在预览阶段即被拒绝。
func PreviewUpdate(r *Reader, rootDir string) (*UpdatePlan, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return previewWithRoot(r, root)
}

func previewWithRoot(r *Reader, root *os.Root) (*UpdatePlan, error) {
	meta, err := readInstalledMeta(root)
	if err != nil {
		return nil, err
	}
	m := r.Manifest()
	if _, err := validateResources(m.Records()); err != nil {
		return nil, err
	}
	plan := &UpdatePlan{
		PackIndex: m.IndexDigest,
		BaseIndex: meta.IndexDigest,
		Entries:   map[string]*PlanEntry{},
	}
	packPaths := map[string]bool{}
	for _, res := range m.Records() {
		packPaths[res.Path] = true
		entry := &PlanEntry{Path: res.Path, PackID: res.ID}
		rec, installed := meta.Records[res.Path]
		if installed {
			entry.InstalledID = rec.ID
		}
		localID, exists, err := localFileID(root, res.Path, res.Type)
		if err != nil {
			return nil, err
		}
		entry.LocalID = localID
		switch {
		case !installed && !exists:
			entry.Change = ChangeAdd
			entry.Decision = DecidePack
		case !installed && exists:
			// 索引未记录但磁盘已有内容: 视为本地已改动, 不许静默覆盖。
			entry.Change = ChangeConflict
		case installed && res.ID == rec.ID && (res.Type == "dir" || !exists || localID == rec.ID):
			entry.Change = ChangeUnchanged
		case installed && exists && localID == rec.ID:
			entry.Change = ChangeReplace
			entry.Decision = DecidePack
		default:
			// 本地内容偏离安装记录(或已丢失), 必须人工决定。
			entry.Change = ChangeConflict
		}
		plan.Entries[res.Path] = entry
	}
	for path, rec := range meta.Records {
		if packPaths[path] {
			continue
		}
		entry := &PlanEntry{Path: path, Change: ChangeRemove, InstalledID: rec.ID}
		localID, _, err := localFileID(root, path, rec.Type)
		if err != nil {
			return nil, err
		}
		entry.LocalID = localID
		plan.Entries[path] = entry
	}
	plan.fillImpacts(&m, meta)
	return plan, nil
}

// fillImpacts 标注每个条目被哪些资源引用, 以及决策对引用方的影响。
func (p *UpdatePlan) fillImpacts(m *Manifest, meta *installedMeta) {
	dependents := map[string][]string{}
	addDeps := func(from string, deps []string) {
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], from)
		}
	}
	for _, res := range m.Records() {
		addDeps(res.Path, res.DependsOn)
	}
	for path, rec := range meta.Records {
		addDeps(path, rec.DependsOn)
	}
	for path, entry := range p.Entries {
		deps := dedupSortedStrings(dependents[path])
		if len(deps) == 0 {
			continue
		}
		entry.Impacts = deps
		switch entry.Change {
		case ChangeConflict:
			entry.ImpactNote = fmt.Sprintf("保留本地版本时, %v 的引用将解析到本地内容身份而非包内声明的身份; 采用包内版本则引用方与包内声明一致。", deps)
		case ChangeRemove:
			entry.ImpactNote = fmt.Sprintf("接受删除后, %v 的引用将悬空; 保留本地版本可维持这些引用可用。", deps)
		}
	}
}

// SortedEntries 返回按路径排序的预览条目。
func (p *UpdatePlan) SortedEntries() []*PlanEntry {
	out := make([]*PlanEntry, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Decide 记录一个人工决策, 只允许与条目状态相容的取值。
func (p *UpdatePlan) Decide(path, decision string) error {
	entry, ok := p.Entries[path]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	switch entry.Change {
	case ChangeConflict, ChangeAdd, ChangeReplace:
		if decision != DecidePack && decision != DecideKeep {
			return fmt.Errorf("assetpack: entry %q allows pack|keep", path)
		}
	case ChangeRemove:
		if decision != DecideRemove && decision != DecideKeep {
			return fmt.Errorf("assetpack: entry %q allows remove|keep", path)
		}
	default:
		return fmt.Errorf("assetpack: entry %q needs no decision", path)
	}
	entry.Decision = decision
	return nil
}

func (p *UpdatePlan) validateDecisions() error {
	var missing []string
	for _, e := range p.SortedEntries() {
		switch e.Change {
		case ChangeConflict, ChangeRemove:
			if e.Decision == "" {
				missing = append(missing, e.Path)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", ErrUndecided, missing)
	}
	return nil
}

// recheckBasis 采用前复核: 本地内容相对预览时发生变化的条目必须重新决定。
func (p *UpdatePlan) recheckBasis(root *os.Root, m *Manifest, meta *installedMeta) error {
	types := map[string]string{}
	for _, res := range m.Records() {
		types[res.Path] = res.Type
	}
	for path, rec := range meta.Records {
		if _, ok := types[path]; !ok {
			types[path] = rec.Type
		}
	}
	var stale []string
	for _, e := range p.SortedEntries() {
		localID, _, err := localFileID(root, e.Path, types[e.Path])
		if err != nil {
			return err
		}
		if localID != e.LocalID {
			stale = append(stale, e.Path)
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("%w: %v", ErrPreviewStale, stale)
	}
	return nil
}

// journal 记录采用进度, 支持中断后重开接续; 提交点之前索引不会被替换。
type journal struct {
	State string      `json:"state"` // "applying"
	Plan  *UpdatePlan `json:"plan"`
}

// rollbackFile 记录单个路径的撤回方式。
type rollbackFile struct {
	Path   string `json:"path"`
	Action string `json:"action"`           // "restore" | "delete"
	Backup string `json:"backup,omitempty"` // metaDir 内的备份文件名
}

// rollbackData 保存撤回本次更新所需的全部信息。
type rollbackData struct {
	PreviousManifest json.RawMessage `json:"previousManifest"`
	Files            []rollbackFile  `json:"files"`
}

const (
	journalName  = "journal.json"
	rollbackName = "rollback.json"
	choicesName  = "choices.json"
	rollbackDir  = "rollback-data"
)

// applyTestHook 仅供测试模拟中断。
var applyTestHook func(stage string) error

func runHook(stage string) error {
	if applyTestHook != nil {
		return applyTestHook(stage)
	}
	return nil
}

func writeMetaFile(root *os.Root, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := root.MkdirAll(metaDir, 0o755); err != nil {
		return fmt.Errorf("%w: create metadata directory: %v", ErrPathTraversal, err)
	}
	return writeFileInRoot(root, filepath.Join(metaDir, name), b, 0o644)
}

func readMetaFile(root *os.Root, name string, v any) error {
	f, err := root.Open(filepath.Join(metaDir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

func removeMetaPath(root *os.Root, name string) {
	root.Remove(filepath.Join(metaDir, name))
}

// ApplyUpdate 采用更新: 复核预览依据, 记录人工选择, 原子提交新索引。
// 中断不会留下指向未完整写入内容的索引; 可用 ResumeUpdate 重开接续。
func ApplyUpdate(r *Reader, rootDir string, plan *UpdatePlan) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var pending journal
	if err := readMetaFile(root, journalName, &pending); err == nil && pending.State == "applying" {
		return ErrUpdateInProgress
	}
	meta, err := readInstalledMeta(root)
	if err != nil {
		return err
	}
	if err := plan.validateDecisions(); err != nil {
		return err
	}
	m := r.Manifest()
	resources, err := validateResources(m.Records())
	if err != nil {
		return err
	}
	if err := preflightRoot(root, resources); err != nil {
		return err
	}
	// 正式采用前再次核对预览依据。
	if err := plan.recheckBasis(root, &m, meta); err != nil {
		return err
	}
	if plan.PackIndex != m.IndexDigest {
		return fmt.Errorf("%w: plan pack %s != reader %s", ErrPreviewStale, plan.PackIndex[:12], m.IndexDigest[:12])
	}
	if err := writeMetaFile(root, journalName, &journal{State: "applying", Plan: plan}); err != nil {
		return err
	}
	if err := runHook("journal"); err != nil {
		return err
	}
	return applyStages(r, root, plan, meta, &m)
}

// ResumeUpdate 接续一次中断的采用。
func ResumeUpdate(r *Reader, rootDir string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var pending journal
	if err := readMetaFile(root, journalName, &pending); err != nil || pending.State != "applying" {
		return fmt.Errorf("assetpack: no interrupted update to resume")
	}
	meta, err := readInstalledMeta(root)
	if err != nil {
		return err
	}
	m := r.Manifest()
	if pending.Plan.PackIndex != m.IndexDigest {
		return fmt.Errorf("%w: pending plan pack differs from reader", ErrPreviewStale)
	}
	return applyStages(r, root, pending.Plan, meta, &m)
}

// applyStages 执行备份、写文件、提交索引; 可重入(中断后重跑安全)。
func applyStages(r *Reader, root *os.Root, plan *UpdatePlan, meta *installedMeta, m *Manifest) error {
	resByPath := map[string]Resource{}
	for _, res := range m.Records() {
		resByPath[res.Path] = res
	}
	// 阶段一: 备份将被覆盖/删除的本地内容(幂等, 已有备份则跳过)。
	var rb rollbackData
	if err := readMetaFile(root, rollbackName, &rb); err != nil {
		rb = rollbackData{}
		if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
			rb.PreviousManifest = b
		}
		seq := 0
		for _, e := range plan.SortedEntries() {
			name, err := inRootName(e.Path)
			if err != nil {
				return err
			}
			switch {
			case e.Decision == DecidePack && (e.Change == ChangeReplace || e.Change == ChangeConflict) && e.InstalledID != "":
				backup := fmt.Sprintf("%s/%06d", rollbackDir, seq)
				seq++
				if err := backupLocalFile(root, name, backup); err != nil {
					return err
				}
				rb.Files = append(rb.Files, rollbackFile{Path: e.Path, Action: "restore", Backup: backup})
			case e.Change == ChangeRemove && e.Decision == DecideRemove:
				backup := fmt.Sprintf("%s/%06d", rollbackDir, seq)
				seq++
				if err := backupLocalFile(root, name, backup); err != nil {
					return err
				}
				rb.Files = append(rb.Files, rollbackFile{Path: e.Path, Action: "restore", Backup: backup})
			case e.Decision == DecidePack && (e.Change == ChangeAdd || (e.Change == ChangeConflict && e.InstalledID == "")):
				rb.Files = append(rb.Files, rollbackFile{Path: e.Path, Action: "delete"})
			}
		}
		if err := writeMetaFile(root, rollbackName, &rb); err != nil {
			return err
		}
	}
	if err := runHook("backups"); err != nil {
		return err
	}
	// 阶段二: 按决策落盘(每个文件原子写)。
	for _, e := range plan.SortedEntries() {
		name, err := inRootName(e.Path)
		if err != nil {
			return err
		}
		switch {
		case e.Decision == DecidePack && e.Change != ChangeRemove && e.Change != ChangeUnchanged:
			res := resByPath[e.Path]
			if res.Type == "dir" {
				if err := root.MkdirAll(name, 0o755); err != nil {
					return fmt.Errorf("%w: create directory %q: %v", ErrPathTraversal, e.Path, err)
				}
				continue
			}
			data, err := r.ReadResource(e.Path)
			if err != nil {
				return fmt.Errorf("read %q: %w", e.Path, err)
			}
			if err := writeFileInRoot(root, name, data, 0o644); err != nil {
				return err
			}
		case e.Change == ChangeRemove && e.Decision == DecideRemove:
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %q: %w", e.Path, err)
			}
		}
	}
	if err := runHook("files"); err != nil {
		return err
	}
	// 阶段三: 组装带来源标注的新索引并原子提交(提交点)。
	newMeta, err := composeMeta(root, plan, meta, m)
	if err != nil {
		return err
	}
	if err := runHook("manifest"); err != nil {
		return err
	}
	if err := writeMetaFile(root, "manifest.json", newMeta); err != nil {
		return err
	}
	// 阶段四: 保留人工选择记录, 清除进行标记。
	choices := map[string]string{}
	for _, e := range plan.SortedEntries() {
		if e.Decision != "" {
			choices[e.Path] = e.Decision
		}
	}
	if err := writeMetaFile(root, choicesName, choices); err != nil {
		return err
	}
	removeMetaPath(root, journalName)
	return nil
}

// backupLocalFile 把本地文件复制进 metaDir 备份区(幂等)。
func backupLocalFile(root *os.Root, name, backup string) error {
	src, err := root.Open(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	if _, err := root.Stat(filepath.Join(metaDir, backup)); err == nil {
		return nil // 已有备份(重入)
	}
	if err := root.MkdirAll(filepath.Join(metaDir, rollbackDir), 0o755); err != nil {
		return err
	}
	return writeFileInRoot(root, filepath.Join(metaDir, backup), data, 0o600)
}

// composeMeta 组装新索引: 每个资源标注来自原安装、本地保留还是此次更新。
func composeMeta(root *os.Root, plan *UpdatePlan, meta *installedMeta, m *Manifest) (*installedMeta, error) {
	records := map[string]DepRecord{}
	for path, rec := range meta.Records {
		if rec.Origin == "" {
			rec.Origin = OriginInstalled
		}
		records[path] = rec
	}
	resByPath := map[string]Resource{}
	for _, res := range m.Records() {
		resByPath[res.Path] = res
	}
	for _, e := range plan.SortedEntries() {
		res, inPack := resByPath[e.Path]
		switch {
		case e.Change == ChangeRemove:
			if e.Decision == DecideRemove {
				delete(records, e.Path)
			} else {
				rec := records[e.Path]
				rec.Origin = OriginKept
				records[e.Path] = rec
			}
		case e.Decision == DecidePack && e.Change != ChangeUnchanged:
			records[e.Path] = DepRecord{
				ID:        res.ID,
				Type:      res.Type,
				Size:      res.Size,
				DependsOn: append([]string(nil), res.DependsOn...),
				Origin:    OriginUpdated,
			}
		case e.Change == ChangeConflict && e.Decision == DecideKeep:
			rec, ok := records[e.Path]
			if !ok {
				info, err := root.Stat(filepath.FromSlash(e.Path))
				if err != nil {
					return nil, err
				}
				rec = DepRecord{Type: res.Type, Size: info.Size()}
				if !inPack {
					rec.Type = "file"
				}
			}
			rec.ID = e.LocalID
			rec.Origin = OriginKept
			records[e.Path] = rec
		}
	}
	body, err := canonicalJSON(records)
	if err != nil {
		return nil, err
	}
	return &installedMeta{
		IndexDigest: hashBytes(body),
		PackIndex:   plan.PackIndex,
		Records:     records,
	}, nil
}

// RollbackUpdate 撤回最近一次更新, 恢复更新前索引与内容; 人工选择记录保留。
func RollbackUpdate(rootDir string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var rb rollbackData
	if err := readMetaFile(root, rollbackName, &rb); err != nil {
		return fmt.Errorf("assetpack: no update to roll back")
	}
	// 路径边界检查在撤回时同样有效。
	for _, f := range rb.Files {
		name, err := inRootName(f.Path)
		if err != nil {
			return err
		}
		switch f.Action {
		case "restore":
			data, err := func() ([]byte, error) {
				src, err := root.Open(filepath.Join(metaDir, filepath.FromSlash(f.Backup)))
				if err != nil {
					return nil, err
				}
				defer src.Close()
				return io.ReadAll(src)
			}()
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := writeFileInRoot(root, name, data, 0o644); err != nil {
				return err
			}
		case "delete":
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if len(rb.PreviousManifest) > 0 {
		if err := writeFileInRoot(root, filepath.Join(metaDir, "manifest.json"), rb.PreviousManifest, 0o644); err != nil {
			return err
		}
	}
	root.RemoveAll(filepath.Join(metaDir, rollbackDir))
	removeMetaPath(root, rollbackName)
	removeMetaPath(root, journalName)
	return nil
}
