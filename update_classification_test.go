package assetpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateNewAndUserDeletedClassification(t *testing.T) {
	basePkg, baseM, err := Build([]AddRequest{
		{Path: "ref.txt", Data: []byte("ref-old")},
		{Path: "victim.txt", Data: []byte("victim-old")},
		{Path: "gone.txt", Data: []byte("gone-old")},
	}, Options{FragmentSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	deltaPkg, deltaM, err := Build([]AddRequest{
		{Path: "ref.txt", Data: []byte("ref-new")},
		{Path: "victim.txt", Data: []byte("victim-new")},
		{Path: "gone.txt", Delete: true},
		{Path: "new.txt", Data: []byte("fresh"), DependsOn: []string{"ref.txt"}},
	}, Options{FragmentSize: 1024, Base: baseM})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "install")
	if _, err := Unpack(readerFromPackage(t, basePkg), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ref.txt"), []byte("ref-local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "victim.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	pv, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]UpdateItem{}
	for _, item := range pv.Items {
		byPath[item.Path] = item
	}
	if item := byPath["new.txt"]; item.Status != PreviewNew || item.Choice != ChoicePackage {
		t.Fatalf("brand-new resource must default to package adoption: %+v", item)
	}
	if item := byPath["victim.txt"]; item.Status != PreviewMissing || item.Choice != ChoiceKeep {
		t.Fatalf("user-deleted resource must default to keeping the deletion: %+v", item)
	}
	if item := byPath["gone.txt"]; item.Status != PreviewRemoved || item.Choice != ChoicePackage {
		t.Fatalf("package-deleted resource already absent locally: %+v", item)
	}
	if item := byPath["ref.txt"]; item.Status != PreviewLocal || item.Choice != ChoiceKeep {
		t.Fatalf("local modification must default to keep: %+v", item)
	}
	if refs := byPath["ref.txt"].ReferencedBy; len(refs) != 1 || refs[0] != "new.txt" {
		t.Fatalf("new resource must reference the locally kept one: %+v", refs)
	}

	// Basis change after preview must be rejected.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("surprise"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareUpdate(chain, dir, pv); !errors.Is(err, ErrPreviewChanged) {
		t.Fatalf("new file appearing after preview must be rejected, got %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}

	if err := ApplyUpdate(chain, dir, pv); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(got) != "fresh" {
		t.Fatalf("new package file must land on disk: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "victim.txt")); !os.IsNotExist(err) {
		t.Fatalf("user deletion must stay deleted, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("package deletion must stay deleted, err=%v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "ref.txt"))
	if err != nil || string(got) != "ref-local" {
		t.Fatalf("local content must be kept: %q %v", got, err)
	}
	inst := readInstalled(t, dir)
	rec, ok := inst.Records["new.txt"]
	if !ok || rec.Source != SourceUpdate || rec.ID != hashBytes([]byte("fresh")) || rec.Size != int64(len("fresh")) {
		t.Fatalf("new resource record wrong: %+v", rec)
	}
	if rec, ok := inst.Records["victim.txt"]; ok {
		t.Fatalf("user-deleted resource must not be recorded as kept: %+v", rec)
	}
	if _, ok := inst.Records["gone.txt"]; ok {
		t.Fatal("package-deleted resource must not remain in manifest")
	}
	rec = inst.Records["ref.txt"]
	if rec.Source != SourceLocal || rec.ID != hashBytes([]byte("ref-local")) || rec.Size != int64(len("ref-local")) {
		t.Fatalf("local record wrong: %+v", rec)
	}

	if err := WithdrawUpdate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("withdraw must remove the adopted new file, err=%v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "ref.txt"))
	if err != nil || string(got) != "ref-local" {
		t.Fatalf("withdraw must keep local content: %q %v", got, err)
	}
	inst = readInstalled(t, dir)
	if inst.IndexDigest != baseM.IndexDigest {
		t.Fatal("withdraw must restore the pre-update manifest")
	}
	if _, ok := inst.Records["victim.txt"]; !ok {
		t.Fatal("withdraw must restore the pre-update record for the user-deleted file")
	}
	receiptPath := filepath.Join(dir, metaDir, historyDirName, deltaM.IndexDigest[:12]+".json")
	rb, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt updateReceipt
	if err := json.Unmarshal(rb, &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Withdrawn || receipt.Choices["victim.txt"] != ChoiceKeep || receipt.Choices["new.txt"] != ChoicePackage {
		t.Fatalf("human decisions must stay recorded after withdraw: %+v", receipt)
	}

	// A fresh preview after withdraw must classify everything again.
	pv2, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, item := range pv2.Items {
		if item.Path == "new.txt" {
			seen = true
			if item.Status != PreviewNew || item.Choice != ChoicePackage {
				t.Fatalf("new resource must re-enter preview naturally: %+v", item)
			}
		}
	}
	if !seen {
		t.Fatal("new resource missing from preview after withdraw")
	}
}

func TestUpdateExplicitChoicesForNewAndDeleted(t *testing.T) {
	basePkg, baseM, err := Build([]AddRequest{
		{Path: "victim.txt", Data: []byte("victim-old")},
	}, Options{FragmentSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	deltaPkg, _, err := Build([]AddRequest{
		{Path: "victim.txt", Data: []byte("victim-new")},
		{Path: "new.txt", Data: []byte("fresh")},
	}, Options{FragmentSize: 1024, Base: baseM})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "install")
	if _, err := Unpack(readerFromPackage(t, basePkg), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "victim.txt")); err != nil {
		t.Fatal(err)
	}
	pv, err := PreviewUpdate(chain, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Adopt the package for the user-deleted file; reject the new file.
	if err := DecideUpdate(pv, map[string]UpdateChoice{
		"victim.txt": ChoicePackage,
		"new.txt":    ChoiceKeep,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdate(chain, dir, pv); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "victim.txt"))
	if err != nil || string(got) != "victim-new" {
		t.Fatalf("package content must restore the user-deleted file: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected new file must stay absent, err=%v", err)
	}
	inst := readInstalled(t, dir)
	if rec := inst.Records["victim.txt"]; rec.Source != SourceUpdate || rec.ID != hashBytes([]byte("victim-new")) {
		t.Fatalf("restored record wrong: %+v", rec)
	}
	if rec, ok := inst.Records["new.txt"]; ok {
		t.Fatalf("rejected new file must not be recorded with an empty identity: %+v", rec)
	}

	if err := WithdrawUpdate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "victim.txt")); !os.IsNotExist(err) {
		t.Fatalf("withdraw must restore the pre-update deletion, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("withdraw must keep the rejected new file absent, err=%v", err)
	}
	if inst := readInstalled(t, dir); inst.IndexDigest != baseM.IndexDigest {
		t.Fatal("withdraw must restore the pre-update manifest")
	}
}
