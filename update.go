package assetpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	SourceOriginal = "original"
	SourceLocal    = "local"
	SourceUpdate   = "update"
)

const (
	pendingDirName = "updates/pending"
	historyDirName = "updates/history"
)

var (
	ErrPreviewChanged = errors.New("assetpack: preview basis changed; decide again")
	ErrUpdatePending  = errors.New("assetpack: update already in progress")
)

type UpdateAction string

const (
	UpdateReplace UpdateAction = "replace"
	UpdateDelete  UpdateAction = "delete"
	UpdateKeep    UpdateAction = "keep"
)

type UpdateStatus string

const (
	PreviewDirect  UpdateStatus = "direct"
	PreviewLocal   UpdateStatus = "local-modified"
	PreviewRemoved UpdateStatus = "package-deleted"
	PreviewNew     UpdateStatus = "new"
	PreviewMissing UpdateStatus = "user-deleted"
)

type UpdateChoice string

const (
	ChoicePackage UpdateChoice = "package"
	ChoiceKeep    UpdateChoice = "keep"
)

type UpdateItem struct {
	Path          string       `json:"path"`
	Status        UpdateStatus `json:"status"`
	Kind          string       `json:"kind"`
	InstalledID   string       `json:"installedId,omitempty"`
	LocalID       string       `json:"localId,omitempty"`
	PackageID     string       `json:"packageId,omitempty"`
	DefaultChoice UpdateChoice `json:"defaultChoice"`
	Choice        UpdateChoice `json:"choice"`
	ReferencedBy  []string     `json:"referencedBy,omitempty"`
	Impact        string       `json:"impact"`
}

type UpdatePreview struct {
	InstalledIndex string                  `json:"installedIndex"`
	PackageIndex   string                  `json:"packageIndex"`
	Items          []UpdateItem            `json:"items"`
	Choices        map[string]UpdateChoice `json:"choices"`
}

type pendingOperation struct {
	Path       string       `json:"path"`
	Action     UpdateAction `json:"action"`
	Kind       string       `json:"kind"`
	BeforeID   string       `json:"beforeId,omitempty"`
	AfterID    string       `json:"afterId,omitempty"`
	AfterSize  int64        `json:"afterSize,omitempty"`
	Source     string       `json:"source"`
	BackupName string       `json:"backupName,omitempty"`
	Completed  bool         `json:"completed"`
}

type pendingState struct {
	InstalledIndex string             `json:"installedIndex"`
	PackageIndex   string             `json:"packageIndex"`
	Preview        UpdatePreview      `json:"preview"`
	Operations     []pendingOperation `json:"operations"`
	FinalManifest  []byte             `json:"finalManifest"`
	FinalWritten   bool               `json:"finalWritten"`
}

type updateReceipt struct {
	InstalledIndex string                  `json:"installedIndex"`
	PackageIndex   string                  `json:"packageIndex"`
	Choices        map[string]UpdateChoice `json:"choices"`
	Operations     []pendingOperation      `json:"operations"`
	Previous       []byte                  `json:"previousManifest"`
	Withdrawn      bool                    `json:"withdrawn,omitempty"`
}

func (r *Reader) TipManifest() Manifest { return *cloneManifest(r.tip) }

func loadInstalled(root *os.Root) (*installedManifest, map[string]DepRecord, error) {
	b, err := root.ReadFile(filepath.Join(metaDir, "manifest.json"))
	if err != nil {
		return nil, nil, err
	}
	var inst installedManifest
	if err := json.Unmarshal(b, &inst); err != nil {
		return nil, nil, fmt.Errorf("%w: installed manifest: %v", ErrCorrupt, err)
	}
	records := map[string]DepRecord{}
	for p, rec := range inst.Records {
		name, err := inRootName(p)
		if err != nil {
			return nil, nil, err
		}
		_ = name
		records[p] = rec
	}
	return &inst, records, nil
}

func localIdentity(root *os.Root, logical, name, kind string) (string, error) {
	if kind == "dir" {
		d, err := root.Open(name)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%w: inspect %q: %v", ErrPathTraversal, name, err)
		}
		defer d.Close()
		dirs, err := d.ReadDir(-1)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%w: inspect %q: %v", ErrPathTraversal, name, err)
		}
		children := make([]string, 0, len(dirs))
		for _, ent := range dirs {
			children = append(children, filepath.ToSlash(filepath.Join(logical, ent.Name())))
		}
		sort.Strings(children)
		body, _ := canonicalJSON(struct {
			Type     string   `json:"type"`
			Path     string   `json:"path"`
			Children []string `json:"children"`
		}{"dir", logical, children})
		return hashBytes(body), nil
	}
	b, err := root.ReadFile(name)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: read %q: %v", ErrPathTraversal, name, err)
	}
	return hashBytes(b), nil
}

func PreviewUpdate(r *Reader, rootDir string) (*UpdatePreview, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if _, err := root.Stat(filepath.Join(metaDir, pendingDirName)); err == nil {
		return nil, ErrUpdatePending
	}
	inst, installed, err := loadInstalled(root)
	if err != nil {
		return nil, err
	}
	tip := r.TipManifest()
	if tip.BaseIndex == "" {
		return nil, fmt.Errorf("assetpack: update requires an incremental package")
	}
	if inst.IndexDigest != tip.BaseIndex {
		return nil, fmt.Errorf("%w: installed index %s does not match package base %s", ErrCorrupt, inst.IndexDigest[:12], tip.BaseIndex[:12])
	}

	referrers := map[string][]string{}
	addRef := func(from string, deps []string) {
		for _, dep := range deps {
			n, err := inRootName(dep)
			if err != nil {
				return
			}
			_ = n
			referrers[dep] = append(referrers[dep], from)
		}
	}
	for _, rec := range r.merged.Records() {
		addRef(rec.Path, rec.DependsOn)
	}
	for p, rec := range installed {
		addRef(p, rec.DependsOn)
	}

	pv := &UpdatePreview{InstalledIndex: inst.IndexDigest, PackageIndex: tip.IndexDigest, Choices: map[string]UpdateChoice{}}
	for _, change := range tip.Records() {
		name, err := inRootName(change.Path)
		if err != nil {
			return nil, err
		}
		old, hadOld := installed[change.Path]
		if change.Type == "delete" {
			if !hadOld {
				return nil, fmt.Errorf("%w: delete %q absent from installed index", ErrCorrupt, change.Path)
			}
			localID, err := localIdentity(root, change.Path, name, old.Type)
			if err != nil {
				return nil, err
			}
			status := PreviewRemoved
			choice := ChoicePackage
			if localID != "" && localID != old.ID {
				status = PreviewLocal
				choice = ChoiceKeep
			}
			item := UpdateItem{Path: change.Path, Status: status, Kind: old.Type, InstalledID: old.ID, LocalID: localID, DefaultChoice: choice, Choice: choice, ReferencedBy: dedupSortedStrings(referrers[change.Path])}
			item.Impact = impactText(item, true)
			pv.Items = append(pv.Items, item)
			pv.Choices[item.Path] = item.Choice
			continue
		}

		localID, err := localIdentity(root, change.Path, name, change.Type)
		if err != nil {
			return nil, err
		}
		if hadOld && old.ID == change.ID && localID == change.ID {
			continue
		}
		status := PreviewDirect
		choice := ChoicePackage
		switch {
		case localID == "" && !hadOld:
			// 包内新增, 本地与原安装都不存在: 默认采用包内容。
			status = PreviewNew
		case localID == "":
			// 原安装有记录但本地被用户删除: 默认尊重删除, 保持不存在。
			status = PreviewMissing
			choice = ChoiceKeep
		case localID != change.ID:
			status = PreviewLocal
			if !hadOld || localID != old.ID {
				choice = ChoiceKeep
			}
		}
		item := UpdateItem{Path: change.Path, Status: status, Kind: change.Type, PackageID: change.ID, LocalID: localID, DefaultChoice: choice, Choice: choice, ReferencedBy: dedupSortedStrings(referrers[change.Path])}
		if hadOld {
			item.InstalledID = old.ID
		}
		item.Impact = impactText(item, false)
		pv.Items = append(pv.Items, item)
		pv.Choices[item.Path] = item.Choice
	}
	sort.Slice(pv.Items, func(i, j int) bool { return pv.Items[i].Path < pv.Items[j].Path })
	return pv, nil
}

func impactText(item UpdateItem, deleted bool) string {
	refs := len(item.ReferencedBy)
	if deleted {
		if item.Choice == ChoiceKeep {
			return fmt.Sprintf("保留本地版本；包内已删除，%d 个引用将继续按本地资源解析", refs)
		}
		return fmt.Sprintf("采用包内删除；仍引用该路径的 %d 个资源需要改引用，否则关系将缺失", refs)
	}
	switch item.Status {
	case PreviewNew:
		if item.Choice == ChoiceKeep {
			return "不采用包内新增；本地保持不存在，清单不会记录该资源"
		}
		return fmt.Sprintf("采用包内新增内容；%d 个引用将解析到此次更新身份", refs)
	case PreviewMissing:
		if item.Choice == ChoiceKeep {
			return "保留本地删除；该资源保持不存在，清单不再记录它"
		}
		return fmt.Sprintf("采用包内内容；恢复本地已删除的资源，%d 个引用将解析到此次更新身份", refs)
	}
	if item.Choice == ChoiceKeep {
		return fmt.Sprintf("保留本地内容；%d 个引用不会切换到本次包内身份", refs)
	}
	return fmt.Sprintf("采用包内内容；%d 个引用将解析到此次更新身份", refs)
}

func DecideUpdate(pv *UpdatePreview, choices map[string]UpdateChoice) error {
	by := map[string]*UpdateItem{}
	for i := range pv.Items {
		by[pv.Items[i].Path] = &pv.Items[i]
	}
	for path, choice := range choices {
		name, err := inRootName(path)
		if err != nil {
			return err
		}
		_ = name
		item, ok := by[path]
		if !ok {
			return fmt.Errorf("assetpack: unknown preview resource %q", path)
		}
		if choice != ChoicePackage && choice != ChoiceKeep {
			return fmt.Errorf("assetpack: invalid choice %q for %q", choice, path)
		}
		item.Choice = choice
		pv.Choices[path] = choice
		item.Impact = impactText(*item, item.Status == PreviewRemoved)
	}
	return nil
}
