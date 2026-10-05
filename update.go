package assetpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SourceOriginal = "original"
	SourceLocal    = "local"
	SourceUpdate   = "update"
	SourceMissing  = "missing"
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
	PreviewDirect      UpdateStatus = "direct"
	PreviewAdded       UpdateStatus = "added"
	PreviewLocal       UpdateStatus = "local-modified"
	PreviewUserDeleted UpdateStatus = "user-deleted"
	PreviewRemoved     UpdateStatus = "package-deleted"
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
	PackageDelete bool         `json:"packageDelete,omitempty"`
	DefaultChoice UpdateChoice `json:"defaultChoice"`
	Choice        UpdateChoice `json:"choice"`
	ReferencedBy  []string     `json:"referencedBy,omitempty"`
	Impact        string       `json:"impact"`
}

// DepResolution 说明某个引用在采用当前选择后最终解析到哪一份内容。
type DepResolution struct {
	Path   string `json:"path"`
	Source string `json:"source"` // update | local | missing
	ID     string `json:"id,omitempty"`
}

// RehearsalItem 按引用关系列出单个资源的每个依赖的采用结果。
type RehearsalItem struct {
	Path string          `json:"path"`
	Deps []DepResolution `json:"deps"`
}

// DanglingRef 是仍指向包内已删除资源的引用; ChoicePath 给出处理入口(对该路径做选择)。
type DanglingRef struct {
	Referrer   string `json:"referrer"`
	Target     string `json:"target"`
	ChoicePath string `json:"choicePath"`
	Handling   string `json:"handling"` // keep-local | accept-missing
}

type UpdatePreview struct {
	InstalledIndex string                  `json:"installedIndex"`
	PackageIndex   string                  `json:"packageIndex"`
	Items          []UpdateItem            `json:"items"`
	Choices        map[string]UpdateChoice `json:"choices"`
	Reasons        map[string]string       `json:"reasons,omitempty"`
	Depends        map[string][]string     `json:"depends,omitempty"`
	MergedIDs      map[string]string       `json:"mergedIds,omitempty"`
	Rehearsal      []RehearsalItem         `json:"rehearsal,omitempty"`
	Dangling       []DanglingRef           `json:"dangling,omitempty"`
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
	Decisions      []UpdateDecision        `json:"decisions,omitempty"`
	Operations     []pendingOperation      `json:"operations"`
	Previous       []byte                  `json:"previousManifest"`
	Withdrawn      bool                    `json:"withdrawn,omitempty"`
}

// UpdateDecision 记录一次人工选择及其理由, 随更新回执持久保存。
type UpdateDecision struct {
	Path   string       `json:"path"`
	Choice UpdateChoice `json:"choice"`
	Reason string       `json:"reason,omitempty"`
}

// UpdateHistoryEntry 是已完成(含已撤回)更新的可查阅决定记录。
type UpdateHistoryEntry struct {
	Receipt        string           `json:"receipt"`
	InstalledIndex string           `json:"installedIndex"`
	PackageIndex   string           `json:"packageIndex"`
	Withdrawn      bool             `json:"withdrawn"`
	Decisions      []UpdateDecision `json:"decisions,omitempty"`
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

	depends := map[string][]string{}
	mergedIDs := map[string]string{}
	for _, rec := range r.merged.Records() {
		mergedIDs[rec.Path] = rec.ID
		if len(rec.DependsOn) > 0 {
			depends[rec.Path] = dedupSortedStrings(rec.DependsOn)
		}
	}
	for p, rec := range installed {
		if _, ok := depends[p]; !ok && len(rec.DependsOn) > 0 {
			depends[p] = dedupSortedStrings(rec.DependsOn)
		}
	}

	pv := &UpdatePreview{InstalledIndex: inst.IndexDigest, PackageIndex: tip.IndexDigest, Choices: map[string]UpdateChoice{}}
	if len(depends) > 0 {
		pv.Depends = depends
	}
	if len(mergedIDs) > 0 {
		pv.MergedIDs = mergedIDs
	}
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
			item := UpdateItem{Path: change.Path, Status: status, Kind: old.Type, InstalledID: old.ID, LocalID: localID, PackageDelete: true, DefaultChoice: choice, Choice: choice, ReferencedBy: dedupSortedStrings(referrers[change.Path])}
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
		var status UpdateStatus
		choice := ChoicePackage
		switch {
		case !hadOld && (localID == "" || localID == change.ID):
			status = PreviewAdded
		case hadOld && localID == "":
			status = PreviewUserDeleted
		case localID == change.ID || (hadOld && localID == old.ID):
			status = PreviewDirect
		default:
			status = PreviewLocal
			choice = ChoiceKeep
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
	pv.refreshDerived()
	return pv, nil
}

// resolveDep 计算依赖路径在当前选择下的最终内容来源。
func (pv *UpdatePreview) resolveDep(dep string) DepResolution {
	for i := range pv.Items {
		item := &pv.Items[i]
		if item.Path != dep {
			continue
		}
		if item.Choice == ChoiceKeep {
			return DepResolution{Path: dep, Source: SourceLocal, ID: item.LocalID}
		}
		if item.PackageDelete {
			return DepResolution{Path: dep, Source: SourceMissing}
		}
		return DepResolution{Path: dep, Source: SourceUpdate, ID: item.PackageID}
	}
	if id, ok := pv.MergedIDs[dep]; ok {
		return DepResolution{Path: dep, Source: SourceUpdate, ID: id}
	}
	return DepResolution{Path: dep, Source: SourceMissing}
}

// refreshDerived 依据当前选择重算预演清单与悬空引用处理入口。
func (pv *UpdatePreview) refreshDerived() {
	paths := make([]string, 0, len(pv.Depends))
	for p := range pv.Depends {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	pv.Rehearsal = nil
	pv.Dangling = nil
	for _, p := range paths {
		deps := pv.Depends[p]
		entry := RehearsalItem{Path: p, Deps: make([]DepResolution, 0, len(deps))}
		for _, dep := range deps {
			entry.Deps = append(entry.Deps, pv.resolveDep(dep))
		}
		pv.Rehearsal = append(pv.Rehearsal, entry)
		for _, dep := range deps {
			for i := range pv.Items {
				item := &pv.Items[i]
				if item.Path != dep || !item.PackageDelete {
					continue
				}
				handling := "accept-missing"
				if item.Choice == ChoiceKeep {
					handling = "keep-local"
				}
				pv.Dangling = append(pv.Dangling, DanglingRef{Referrer: p, Target: dep, ChoicePath: dep, Handling: handling})
			}
		}
	}
}

func impactText(item UpdateItem, deleted bool) string {
	refs := len(item.ReferencedBy)
	switch {
	case item.Choice == ChoiceKeep && deleted:
		return fmt.Sprintf("保留本地版本；包内已删除，%d 个引用将继续按本地资源解析", refs)
	case item.Choice == ChoicePackage && deleted:
		return fmt.Sprintf("采用包内删除；仍引用该路径的 %d 个资源需要改引用，否则关系将缺失", refs)
	case item.Choice == ChoiceKeep:
		return fmt.Sprintf("保留本地内容；%d 个引用不会切换到本次包内身份", refs)
	default:
		return fmt.Sprintf("采用包内内容；%d 个引用将解析到此次更新身份", refs)
	}
}

func DecideUpdate(pv *UpdatePreview, choices map[string]UpdateChoice) error {
	return DecideUpdateWithReasons(pv, choices, nil)
}

// DecideUpdateWithReasons 应用人工选择并保存每个选择的理由; 选择变化后重算预演清单。
func DecideUpdateWithReasons(pv *UpdatePreview, choices map[string]UpdateChoice, reasons map[string]string) error {
	by := map[string]*UpdateItem{}
	for i := range pv.Items {
		by[pv.Items[i].Path] = &pv.Items[i]
	}
	decided := false
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
		if choice == ChoiceKeep && item.LocalID == "" {
			return fmt.Errorf("assetpack: cannot keep %q: no local content exists", path)
		}
		item.Choice = choice
		pv.Choices[path] = choice
		item.Impact = impactText(*item, item.Status == PreviewRemoved)
		decided = true
	}
	for path, reason := range reasons {
		if _, ok := by[path]; !ok {
			return fmt.Errorf("assetpack: reason for unknown preview resource %q", path)
		}
		if reason == "" {
			continue
		}
		if pv.Reasons == nil {
			pv.Reasons = map[string]string{}
		}
		pv.Reasons[path] = reason
	}
	if decided || len(pv.Reasons) > 0 {
		pv.refreshDerived()
	}
	return nil
}

// RecheckNeeded 表示预演后部分资源的本地内容发生变化, 只有这些选择需要重新核对。
type RecheckNeeded struct {
	Paths []string
}

func (e *RecheckNeeded) Error() string {
	return fmt.Sprintf("%v: re-decide affected choice(s): %s", ErrPreviewChanged, strings.Join(e.Paths, ", "))
}

func (e *RecheckNeeded) Unwrap() error { return ErrPreviewChanged }

// RevalidatePreview 在预演依据变化后只重新核对受影响的项: 刷新其本地身份与默认选择,
// 清除这些项的人工选择与理由, 其余选择保持不变。返回需要重新决定的路径。
func RevalidatePreview(pv *UpdatePreview, rootDir string) ([]string, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var changed []string
	for i := range pv.Items {
		item := &pv.Items[i]
		name, err := inRootName(item.Path)
		if err != nil {
			return nil, err
		}
		got, err := localIdentity(root, item.Path, name, item.Kind)
		if err != nil {
			return nil, err
		}
		if got == item.LocalID {
			continue
		}
		item.LocalID = got
		hadOld := item.InstalledID != ""
		choice := ChoicePackage
		switch {
		case item.PackageDelete:
			if got != "" && got != item.InstalledID {
				item.Status = PreviewLocal
				choice = ChoiceKeep
			} else {
				item.Status = PreviewRemoved
			}
		case !hadOld && (got == "" || got == item.PackageID):
			item.Status = PreviewAdded
		case hadOld && got == "":
			item.Status = PreviewUserDeleted
		case got == item.PackageID || (hadOld && got == item.InstalledID):
			item.Status = PreviewDirect
		default:
			item.Status = PreviewLocal
			choice = ChoiceKeep
		}
		item.DefaultChoice = choice
		item.Choice = choice
		item.Impact = impactText(*item, item.PackageDelete)
		pv.Choices[item.Path] = choice
		if pv.Reasons != nil {
			delete(pv.Reasons, item.Path)
		}
		changed = append(changed, item.Path)
	}
	if len(changed) > 0 {
		pv.refreshDerived()
	}
	return changed, nil
}
