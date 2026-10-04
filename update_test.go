package assetpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildPkg(t *testing.T, reqs []AddRequest, base *Manifest) ([]byte, *Manifest) {
	t.Helper()
	opts := Options{FragmentSize: 64}
	if base != nil {
		opts.Base = base
	}
	pkg, m, err := Build(reqs, opts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return pkg, m
}

func installBase(t *testing.T, dir string) ([]byte, *Manifest) {
	t.Helper()
	pkg, m := buildPkg(t, []AddRequest{
		{Path: "a.txt", Data: []byte("aaa-v1"), DependsOn: []string{"b.txt", "gone.txt"}},
		{Path: "b.txt", Data: []byte("bbb-v1")},
		{Path: "gone.txt", Data: []byte("gone-v1")},
		{Path: "keep.txt", Data: []byte("keep-v1")},
	}, nil)
	r := openBytes(t, pkg)
	if _, err := Unpack(r, dir); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return pkg, m
}

func deltaReader(t *testing.T, basePkg []byte, baseM *Manifest) *Reader {
	t.Helper()
	pkg, _ := buildPkg(t, []AddRequest{
		{Path: "a.txt", Data: []byte("aaa-v2"), DependsOn: []string{"b.txt", "gone.txt"}},
		{Path: "b.txt", Data: []byte("bbb-v2")},
		{Path: "keep.txt", Data: []byte("keep-v1")},
		{Path: "new.txt", Data: []byte("new-v2")},
		{Path: "gone.txt", Remove: true},
	}, baseM)
	r, err := OpenChain([]ReaderAtSize{
		bytes.NewReader(basePkg),
		bytes.NewReader(pkg),
	})
	if err != nil {
		t.Fatalf("open chain: %v", err)
	}
	return r
}

func TestPreviewClassifiesChanges(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	// 本地改动 b.txt; a.txt 未动; gone.txt 将被包内删除。
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb-local"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	cases := map[string]string{
		"a.txt":    ChangeReplace,
		"b.txt":    ChangeConflict,
		"gone.txt": ChangeRemove,
		"keep.txt": ChangeUnchanged,
		"new.txt":  ChangeAdd,
	}
	for path, want := range cases {
		e := plan.Entries[path]
		if e == nil {
			t.Fatalf("missing entry %s", path)
		}
		if e.Change != want {
			t.Errorf("%s: change=%s want %s", path, e.Change, want)
		}
	}
	// 引用影响说明: a.txt 依赖 b.txt。
	b := plan.Entries["b.txt"]
	if len(b.Impacts) != 1 || b.Impacts[0] != "a.txt" || b.ImpactNote == "" {
		t.Errorf("impact not explained: %+v", b)
	}
	if plan.Entries["gone.txt"].ImpactNote == "" {
		t.Error("removal impact not explained")
	}
}

func readMeta(t *testing.T, dir string) *installedMeta {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, metaDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta installedMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	return &meta
}

func TestApplyDecisionsAndOrigins(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb-local"), 0o644)
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	// 未决定时拒绝采用。
	if err := ApplyUpdate(r, dir, plan); !errors.Is(err, ErrUndecided) {
		t.Fatalf("want ErrUndecided, got %v", err)
	}
	if err := plan.Decide("b.txt", DecideKeep); err != nil {
		t.Fatal(err)
	}
	if err := plan.Decide("gone.txt", DecideRemove); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdate(r, dir, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 内容检查: a 采用包内版本, b 保留本地, gone 被删除, new 新增。
	got := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	if got("a.txt") != "aaa-v2" || got("b.txt") != "bbb-local" || got("new.txt") != "new-v2" {
		t.Error("content mismatch after apply")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Error("gone.txt should be removed")
	}
	// 最终清单来源标注。
	meta := readMeta(t, dir)
	origins := map[string]string{}
	for p, rec := range meta.Records {
		origins[p] = rec.Origin
	}
	want := map[string]string{
		"a.txt":    OriginUpdated,
		"b.txt":    OriginKept,
		"keep.txt": OriginInstalled,
		"new.txt":  OriginUpdated,
	}
	for p, w := range want {
		if origins[p] != w {
			t.Errorf("%s origin=%s want %s", p, origins[p], w)
		}
	}
	if _, ok := meta.Records["gone.txt"]; ok {
		t.Error("gone.txt should be dropped from index")
	}
	// 人工选择记录已保留。
	if _, err := os.Stat(filepath.Join(dir, metaDir, choicesName)); err != nil {
		t.Error("choices record missing")
	}
	// b.txt 的本地身份被记录。
	if meta.Records["b.txt"].ID != hashBytes([]byte("bbb-local")) {
		t.Error("kept local identity not recorded")
	}
}

func TestApplyRechecksBasis(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb-local"), 0o644)
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	plan.Decide("b.txt", DecidePack)
	plan.Decide("gone.txt", DecideRemove)
	// 预览之后本地又发生变化。
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb-local-2"), 0o644)
	if err := ApplyUpdate(r, dir, plan); !errors.Is(err, ErrPreviewStale) {
		t.Fatalf("want ErrPreviewStale, got %v", err)
	}
}

func TestInterruptedApplyResumes(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	plan.Decide("gone.txt", DecideKeep)
	// 在文件写完后、索引提交前中断。
	crashed := false
	applyTestHook = func(stage string) error {
		if stage == "manifest" && !crashed {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	defer func() { applyTestHook = nil }()
	if err := ApplyUpdate(r, dir, plan); err == nil {
		t.Fatal("expected crash error")
	}
	// 索引不得指向未完整写入的状态: 仍是原安装索引。
	meta := readMeta(t, dir)
	if meta.IndexDigest != plan.BaseIndex {
		t.Error("index changed before commit point")
	}
	if _, ok := meta.Records["gone.txt"]; !ok {
		t.Error("old index should still record gone.txt")
	}
	// 进行中的更新未接续前不允许新的采用。
	if err := ApplyUpdate(r, dir, plan); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("want ErrUpdateInProgress, got %v", err)
	}
	// 重开接续。
	if err := ResumeUpdate(r, dir); err != nil {
		t.Fatalf("resume: %v", err)
	}
	meta = readMeta(t, dir)
	if meta.Records["a.txt"].Origin != OriginUpdated {
		t.Error("resume did not commit new index")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "aaa-v2" {
		t.Error("resume content mismatch")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "gone.txt")); string(got) != "gone-v1" {
		t.Error("kept removal target lost")
	}
}

func TestRollbackRestoresPreviousState(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb-local"), 0o644)
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	plan.Decide("b.txt", DecidePack) // 覆盖本地改动
	plan.Decide("gone.txt", DecideRemove)
	if err := ApplyUpdate(r, dir, plan); err != nil {
		t.Fatal(err)
	}
	if err := RollbackUpdate(dir); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	if got("b.txt") != "bbb-local" || got("gone.txt") != "gone-v1" {
		t.Error("rollback did not restore local/previous content")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Error("added file should be deleted by rollback")
	}
	meta := readMeta(t, dir)
	if meta.IndexDigest != plan.BaseIndex {
		t.Error("rollback did not restore previous index")
	}
	// 人工选择记录在撤回后仍保留。
	if _, err := os.Stat(filepath.Join(dir, metaDir, choicesName)); err != nil {
		t.Error("choices record lost after rollback")
	}
	// 撤回只能进行一次。
	if err := RollbackUpdate(dir); err == nil {
		t.Error("second rollback should fail")
	}
}

func TestUpdatePathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	r := deltaReader(t, basePkg, baseM)
	// 篡改安装索引, 塞入越界路径。
	meta := readMeta(t, dir)
	meta.Records["../evil"] = DepRecord{ID: "x", Type: "file"}
	b, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(dir, metaDir, "manifest.json"), b, 0o644)
	if _, err := PreviewUpdate(r, dir); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("preview: want ErrPathTraversal, got %v", err)
	}
}

func TestPreviewPlanRoundTripJSON(t *testing.T) {
	dir := t.TempDir()
	basePkg, baseM := installBase(t, dir)
	r := deltaReader(t, basePkg, baseM)
	plan, err := PreviewUpdate(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var back UpdatePlan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.PackIndex != plan.PackIndex || len(back.Entries) != len(plan.Entries) {
		t.Error("plan JSON round trip mismatch")
	}
	if !strings.Contains(string(b), "packIndex") {
		t.Error("plan JSON missing packIndex")
	}
	_ = bytes.MinRead
}
