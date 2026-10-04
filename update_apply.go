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

func statePath() string       { return filepath.Join(metaDir, pendingDirName, "state.json") }
func payloadDir() string      { return filepath.Join(metaDir, pendingDirName, "payloads") }
func backupDir() string       { return filepath.Join(metaDir, pendingDirName, "backups") }
func historyIndexDir() string { return filepath.Join(metaDir, historyDirName) }

func writeJSONInRoot(root *os.Root, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileInRoot(root, name, b, 0o644)
}

func readJSONInRoot(root *os.Root, name string, v any) error {
	b, err := root.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCorrupt, name, err)
	}
	return nil
}

func readExistingBytes(root *os.Root, name string) ([]byte, bool, error) {
	b, err := root.ReadFile(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read %q: %v", ErrPathTraversal, name, err)
	}
	return b, true, nil
}

func safeHexToken(value string, n int) (string, error) {
	if len(value) != n {
		return "", fmt.Errorf("%w: invalid digest length", ErrCorrupt)
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("%w: invalid digest token", ErrCorrupt)
		}
	}
	return value, nil
}

func safeReceiptName(name string) (string, error) {
	base := filepath.Base(name)
	if base != name || strings.ContainsAny(base, `/\`) || filepath.Ext(base) != ".json" {
		return "", fmt.Errorf("%w: invalid update receipt name %q", ErrPathTraversal, name)
	}
	token := strings.TrimSuffix(base, filepath.Ext(base))
	if _, err := safeHexToken(token, 12); err != nil {
		return "", err
	}
	return base, nil
}

func localSize(root *os.Root, name, kind string) (int64, error) {
	if kind == "dir" {
		d, err := root.Open(name)
		if err != nil {
			return 0, err
		}
		defer d.Close()
		info, err := d.Stat()
		return info.Size(), err
	}
	b, _, err := readExistingBytes(root, name)
	if err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

func PrepareUpdate(r *Reader, rootDir string, pv *UpdatePreview) error {
	if pv == nil {
		return fmt.Errorf("assetpack: nil update preview")
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	inst, installed, err := loadInstalled(root)
	if err != nil {
		return err
	}
	tip := r.TipManifest()
	if pv.InstalledIndex != inst.IndexDigest || pv.PackageIndex != tip.IndexDigest {
		return fmt.Errorf("%w: preview identity mismatch", ErrPreviewChanged)
	}
	if _, err := root.Stat(filepath.Join(metaDir, pendingDirName)); err == nil {
		return ErrUpdatePending
	}
	pendingRoot := filepath.Join(metaDir, pendingDirName)
	committed := false
	defer func() {
		if !committed {
			_ = root.RemoveAll(pendingRoot)
		}
	}()

	for _, item := range pv.Items {
		name, err := inRootName(item.Path)
		if err != nil {
			return err
		}
		got, err := localIdentity(root, item.Path, name, item.Kind)
		if err != nil {
			return err
		}
		if got != item.LocalID {
			return fmt.Errorf("%w: %q", ErrPreviewChanged, item.Path)
		}
		if item.Choice == ChoiceKeep && item.LocalID == "" {
			return fmt.Errorf("%w: cannot keep %q: no local content exists", ErrCorrupt, item.Path)
		}
	}

	mergedByPath := map[string]Resource{}
	for _, res := range r.merged.Records() {
		mergedByPath[res.Path] = res
	}
	tipByPath := map[string]Resource{}
	for _, res := range tip.Records() {
		tipByPath[res.Path] = res
	}
	ops := make([]pendingOperation, 0, len(pv.Items))
	for _, item := range pv.Items {
		name, err := inRootName(item.Path)
		if err != nil {
			return err
		}
		tipRes, isDelete := tipByPath[item.Path]
		op := pendingOperation{Path: item.Path, Kind: item.Kind, BeforeID: item.LocalID}
		if isDelete && tipRes.Type == "delete" {
			if item.Choice == ChoiceKeep {
				op.Action = UpdateKeep
				op.AfterID = item.LocalID
				if size, err := localSize(root, name, item.Kind); err != nil {
					return err
				} else {
					op.AfterSize = size
				}
				op.Source = SourceLocal
			} else {
				op.Action = UpdateDelete
				op.Source = SourceUpdate
				if item.LocalID != "" {
					data, ok, err := readExistingBytes(root, name)
					if err != nil {
						return err
					}
					if item.Kind == "file" && ok {
						token, err := safeHexToken(hashBytes(data), 64)
						if err != nil {
							return err
						}
						op.BackupName = token
						if err := writeFileInRoot(root, filepath.Join(backupDir(), token), data, 0o600); err != nil {
							return err
						}
					}
				}
			}
			ops = append(ops, op)
			continue
		}
		pkgRes := mergedByPath[item.Path]
		data, err := r.ReadResource(item.Path)
		if err != nil {
			return err
		}
		if hashBytes(data) != pkgRes.ID {
			return ErrBadDigest
		}
		if item.Choice == ChoiceKeep {
			op.Action = UpdateKeep
			op.AfterID = item.LocalID
			if size, err := localSize(root, name, item.Kind); err != nil {
				return err
			} else {
				op.AfterSize = size
			}
			op.Source = SourceLocal
		} else {
			if old, ok := installed[item.Path]; ok && item.LocalID != "" && item.Kind == "file" {
				oldData, existed, err := readExistingBytes(root, name)
				if err != nil {
					return err
				}
				if existed && hashBytes(oldData) == item.LocalID {
					_ = old
					token, err := safeHexToken(hashBytes(oldData), 64)
					if err != nil {
						return err
					}
					op.BackupName = token
					if err := writeFileInRoot(root, filepath.Join(backupDir(), token), oldData, 0o600); err != nil {
						return err
					}
				}
			}
			op.Action = UpdateReplace
			op.AfterID = pkgRes.ID
			op.Source = SourceUpdate
			if err := writeFileInRoot(root, filepath.Join(payloadDir(), pkgRes.ID), data, 0o600); err != nil {
				return err
			}
		}
		ops = append(ops, op)
	}

	final, err := buildFinalManifest(r, inst, installed, pv, ops)
	if err != nil {
		return err
	}
	state := pendingState{InstalledIndex: inst.IndexDigest, PackageIndex: tip.IndexDigest, Preview: *pv, Operations: ops, FinalManifest: final}
	if err := writeJSONInRoot(root, statePath(), &state); err != nil {
		return err
	}
	committed = true
	return nil
}

func buildFinalManifest(r *Reader, inst *installedManifest, installed map[string]DepRecord, pv *UpdatePreview, ops []pendingOperation) ([]byte, error) {
	records := map[string]DepRecord{}
	for _, res := range r.merged.Records() {
		records[res.Path] = DepRecord{ID: res.ID, Type: res.Type, Size: res.Size, DependsOn: append([]string(nil), res.DependsOn...), Source: SourceOriginal}
	}
	for _, op := range ops {
		tipRes, _ := r.Lookup(op.Path)
		switch op.Action {
		case UpdateKeep:
			if old, ok := installed[op.Path]; ok {
				old.Source = SourceLocal
				old.ID = op.AfterID
				old.Size = op.AfterSize
				records[op.Path] = old
			} else {
				records[op.Path] = DepRecord{ID: op.AfterID, Type: op.Kind, Size: op.AfterSize, DependsOn: append([]string(nil), tipRes.DependsOn...), Source: SourceLocal}
			}
		case UpdateReplace:
			records[op.Path] = DepRecord{ID: tipRes.ID, Type: tipRes.Type, Size: tipRes.Size, DependsOn: append([]string(nil), tipRes.DependsOn...), Source: SourceUpdate}
		case UpdateDelete:
			delete(records, op.Path)
		}
	}
	b, err := json.MarshalIndent(installedManifest{IndexDigest: pv.PackageIndex, Records: records}, "", "  ")
	if err != nil {
		return nil, err
	}
	return b, nil
}

func ApplyPrepared(rootDir string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var state pendingState
	if err := readJSONInRoot(root, statePath(), &state); err != nil {
		return err
	}
	var finalInstalled installedManifest
	if err := json.Unmarshal(state.FinalManifest, &finalInstalled); err != nil {
		return fmt.Errorf("%w: staged final manifest: %v", ErrCorrupt, err)
	}
	packageToken, err := safeHexToken(state.PackageIndex, 64)
	if err != nil {
		return err
	}
	if finalInstalled.IndexDigest != packageToken {
		return fmt.Errorf("%w: staged final manifest identity", ErrCorrupt)
	}
	for _, op := range state.Operations {
		if op.Completed {
			continue
		}
		name, err := inRootName(op.Path)
		if err != nil {
			return err
		}
		switch op.Action {
		case UpdateReplace:
			afterToken, err := safeHexToken(op.AfterID, 64)
			if err != nil {
				return err
			}
			data, err := root.ReadFile(filepath.Join(payloadDir(), afterToken))
			if err != nil {
				return fmt.Errorf("%w: staged payload %q: %v", ErrCorrupt, op.Path, err)
			}
			if hashBytes(data) != afterToken {
				return fmt.Errorf("%w: staged payload %q", ErrBadDigest, op.Path)
			}
			if op.Kind == "dir" {
				if err := root.MkdirAll(name, 0o755); err != nil {
					return fmt.Errorf("%w: %v", ErrPathTraversal, err)
				}
			} else if err := writeFileInRoot(root, name, data, 0o644); err != nil {
				return err
			}
		case UpdateDelete:
			if op.Kind == "dir" {
				if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("%w: delete %q: %v", ErrPathTraversal, op.Path, err)
				}
			} else if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("%w: delete %q: %v", ErrPathTraversal, op.Path, err)
			}
		case UpdateKeep:
		default:
			return fmt.Errorf("%w: unknown operation for %q", ErrCorrupt, op.Path)
		}
		op.Completed = true
		for i := range state.Operations {
			if state.Operations[i].Path == op.Path {
				state.Operations[i] = op
			}
		}
		if err := writeJSONInRoot(root, statePath(), &state); err != nil {
			return err
		}
	}
	previous, err := root.ReadFile(filepath.Join(metaDir, "manifest.json"))
	if err != nil {
		return err
	}
	receipt := updateReceipt{InstalledIndex: state.InstalledIndex, PackageIndex: packageToken, Choices: state.Preview.Choices, Operations: state.Operations, Previous: previous}
	historyName := filepath.Join(historyIndexDir(), packageToken[:12]+".json")
	if err := writeJSONInRoot(root, historyName, &receipt); err != nil {
		return err
	}
	if err := writeFileInRoot(root, filepath.Join(metaDir, "manifest.json"), state.FinalManifest, 0o644); err != nil {
		return err
	}
	state.FinalWritten = true
	_ = writeJSONInRoot(root, statePath(), &state)
	historyDataName := filepath.Join(metaDir, historyDirName, packageToken[:12])
	if _, err := root.Stat(historyDataName); err == nil {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return err
		}
		archived := historyDataName + "-withdrawn-" + hex.EncodeToString(suffix[:])
		if err := root.Rename(historyDataName, archived); err != nil {
			return err
		}
	}
	return root.Rename(filepath.Join(metaDir, pendingDirName), historyDataName)
}

func ApplyUpdate(r *Reader, rootDir string, pv *UpdatePreview) error {
	if err := PrepareUpdate(r, rootDir, pv); err != nil {
		return err
	}
	return ApplyPrepared(rootDir)
}

func ResumeUpdate(r *Reader, rootDir string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var state pendingState
	if err := readJSONInRoot(root, statePath(), &state); err != nil {
		return err
	}
	tip := r.TipManifest()
	if tip.IndexDigest != state.PackageIndex {
		return fmt.Errorf("%w: resumed package identity mismatch", ErrCorrupt)
	}
	if err := r.VerifyAll(); err != nil {
		return err
	}
	return ApplyPrepared(rootDir)
}

func WithdrawUpdate(rootDir string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	historyRoot, err := root.Open(historyIndexDir())
	if err != nil {
		return err
	}
	defer historyRoot.Close()
	entries, err := historyRoot.ReadDir(-1)
	if err != nil {
		return err
	}
	receiptNames := make([]string, 0)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		base, err := safeReceiptName(ent.Name())
		if err != nil {
			return err
		}
		receiptNames = append(receiptNames, base)
	}
	sort.Strings(receiptNames)
	var receiptName string
	for i := len(receiptNames) - 1; i >= 0; i-- {
		candidate := filepath.Join(historyIndexDir(), receiptNames[i])
		var candidateReceipt updateReceipt
		if err := readJSONInRoot(root, candidate, &candidateReceipt); err != nil {
			return err
		}
		if !candidateReceipt.Withdrawn {
			receiptName = candidate
			break
		}
	}
	if receiptName == "" {
		return fmt.Errorf("%w: no completed update to withdraw", ErrNotFound)
	}
	var receipt updateReceipt
	if err := readJSONInRoot(root, receiptName, &receipt); err != nil {
		return err
	}
	packageToken, err := safeHexToken(receipt.PackageIndex, 64)
	if err != nil {
		return err
	}
	for i := len(receipt.Operations) - 1; i >= 0; i-- {
		op := receipt.Operations[i]
		name, err := inRootName(op.Path)
		if err != nil {
			return err
		}
		switch op.Action {
		case UpdateReplace, UpdateDelete:
			if op.Kind == "file" {
				current, existed, err := readExistingBytes(root, name)
				if err != nil {
					return err
				}
				if existed && hashBytes(current) != op.AfterID {
					return fmt.Errorf("%w: %q changed after update; refuse silent withdrawal overwrite", ErrPreviewChanged, op.Path)
				}
			}
			if op.Kind == "dir" && op.BackupName == "" {
				if err := root.MkdirAll(name, 0o755); err != nil {
					return fmt.Errorf("%w: %v", ErrPathTraversal, err)
				}
				continue
			}
			if op.BackupName == "" {
				if err := root.RemoveAll(name); err != nil {
					return fmt.Errorf("%w: %v", ErrPathTraversal, err)
				}
				continue
			}
			backupToken, err := safeHexToken(op.BackupName, 64)
			if err != nil {
				return err
			}
			b, err := root.ReadFile(filepath.Join(historyIndexDir(), packageToken[:12], "backups", backupToken))
			if err != nil {
				return err
			}
			if hashBytes(b) != op.BeforeID {
				return ErrBadDigest
			}
			if err := writeFileInRoot(root, name, b, 0o644); err != nil {
				return err
			}
		case UpdateKeep:
		}
	}
	if err := writeFileInRoot(root, filepath.Join(metaDir, "manifest.json"), receipt.Previous, 0o644); err != nil {
		return err
	}
	receipt.Withdrawn = true
	return writeJSONInRoot(root, receiptName, &receipt)
}
