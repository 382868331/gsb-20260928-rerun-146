package assetpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readInstalled(t *testing.T, dir string) installedManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, metaDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inst installedManifest
	if err := json.Unmarshal(b, &inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestUpdatePreviewApplyResumeWithdraw(t *testing.T) {
	basePkg, baseM, err := Build([]AddRequest{
		{Path: "changed.txt", Data: []byte("old-changed"), DependsOn: []string{"removed.txt"}},
		{Path: "same.txt", Data: []byte("same")},
		{Path: "removed.txt", Data: []byte("removed")},
	}, Options{FragmentSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	baseReader := readerFromPackage(t, basePkg)
	deltaPkg, _, err := Build([]AddRequest{
		{Path: "changed.txt", Data: []byte("new-package"), DependsOn: []string{"removed.txt"}},
		{Path: "same.txt", Data: []byte("same")},
		{Path: "removed.txt", Delete: true},
	}, Options{FragmentSize: 1024, Base: baseM})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := chain.Lookup("removed.txt"); ok {
		t.Fatal("deleted resource must be absent from merged chain")
	}

	dir := filepath.Join(t.TempDir(), "install")
	if _, err := Unpack(baseReader, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("local-work"), 0o644); err != nil {
		t.Fatal(err)
	}

	pv, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]UpdateItem{}
	for _, item := range pv.Items {
		byPath[item.Path] = item
		if item.Path == "changed.txt" {
			if item.Status != PreviewLocal || item.Choice != ChoiceKeep {
				t.Fatalf("local modification must default to keep: %+v", item)
			}
			if len(item.ReferencedBy) != 0 || item.Impact == "" {
				t.Fatalf("missing impact report: %+v", item)
			}
		}
		if item.Path == "removed.txt" {
			if item.Status != PreviewRemoved || item.Choice != ChoicePackage {
				t.Fatalf("package-deleted resource should allow deletion by default: %+v", item)
			}
		}
	}
	if _, ok := byPath["same.txt"]; ok {
		t.Fatal("unchanged resource should not appear in preview")
	}
	if err := DecideUpdate(pv, map[string]UpdateChoice{
		"changed.txt": ChoicePackage,
		"removed.txt": ChoicePackage,
		"../evil":     ChoicePackage,
	}); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("decision path boundary must be enforced, got %v", err)
	}
	if err := DecideUpdate(pv, map[string]UpdateChoice{
		"changed.txt": ChoicePackage,
		"removed.txt": ChoicePackage,
	}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("changed-after-preview"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareUpdate(chain, dir, pv); !errors.Is(err, ErrPreviewChanged) {
		t.Fatalf("changed preview basis must be rejected, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("local-work"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareUpdate(chain, dir, pv); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(dir, metaDir, pendingDirName, "state.json")
	sb, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var state pendingState
	if err := json.Unmarshal(sb, &state); err != nil {
		t.Fatal(err)
	}
	for i := range state.Operations {
		if state.Operations[i].Action == UpdateDelete {
			state.Operations[i].Completed = true
		}
	}
	restarted, _ := json.Marshal(&state)
	if err := WriteFileAtomic(stateFile, restarted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := ResumeUpdate(chain, dir); err != nil {
		t.Fatalf("resume complete pending update: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "changed.txt"))
	if err != nil || string(got) != "new-package" {
		t.Fatalf("package version not adopted: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "removed.txt")); !os.IsNotExist(err) {
		t.Fatalf("package removal not adopted, err=%v", err)
	}
	inst := readInstalled(t, dir)
	if inst.Records["changed.txt"].Source != SourceUpdate {
		t.Fatal("final record must identify update source")
	}
	if inst.Records["same.txt"].Source != SourceOriginal {
		t.Fatal("unchanged record must identify original source")
	}
	if _, ok := inst.Records["removed.txt"]; ok {
		t.Fatal("removed record must not remain")
	}

	if err := WithdrawUpdate(dir); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "changed.txt"))
	if err != nil || string(got) != "local-work" {
		t.Fatalf("local choice should be restored: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "removed.txt"))
	if err != nil || string(got) != "removed" {
		t.Fatalf("deleted package file should be restored: %q %v", got, err)
	}
	inst = readInstalled(t, dir)
	if inst.IndexDigest != baseM.IndexDigest {
		t.Fatal("withdraw restored wrong manifest")
	}

	pv2, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecideUpdate(pv2, map[string]UpdateChoice{"changed.txt": ChoicePackage}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdate(chain, dir, pv2); err != nil {
		t.Fatalf("reopen after withdraw: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "changed.txt"))
	if err != nil || string(got) != "new-package" {
		t.Fatalf("reopened update mismatch: %q %v", got, err)
	}
}

func TestUpdateKeepsLocalChoice(t *testing.T) {
	basePkg, baseM, _ := Build([]AddRequest{{Path: "a", Data: []byte("old")}}, Options{FragmentSize: 1024})
	deltaPkg, _, _ := Build([]AddRequest{{Path: "a", Data: []byte("new")}}, Options{FragmentSize: 1024, Base: baseM})
	chain, _ := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	dir := filepath.Join(t.TempDir(), "install")
	if _, err := Unpack(readerFromPackage(t, basePkg), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	pv, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Items[0].Choice != ChoiceKeep {
		t.Fatal("local edit must not be silently replaced")
	}
	if err := ApplyUpdate(chain, dir, pv); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a"))
	if string(got) != "mine" {
		t.Fatalf("local version not retained: %q", got)
	}
	if rec := readInstalled(t, dir).Records["a"]; rec.Source != SourceLocal || rec.ID != hashBytes([]byte("mine")) {
		t.Fatalf("local record wrong: %+v", rec)
	}
}
