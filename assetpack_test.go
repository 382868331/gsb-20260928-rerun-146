package assetpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bigData(prefix byte, n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = prefix ^ byte(i*31)
	}
	return data
}

func TestPackSharedCyclesNestedAndRead(t *testing.T) {
	dir := t.TempDir()
	texture := bigData(1, chunkSize+123)
	shared := []byte("shared palette")
	inputs := []InputResource{
		{Paths: []string{"art/texture.bin"}, Data: texture},
		{Paths: []string{"art/a-palette.bin", "art/b-palette.bin"}, Data: shared, Deps: []int{2}},
		{Paths: []string{"scene/root.json"}, Data: []byte(`{"name":"root"}`), Deps: []int{1}, Children: []NestedInput{{Name: "texture", Ref: 0}, {Name: "palette", Ref: 1}}},
	}
	path := filepath.Join(dir, "v1.dap")
	m, err := BuildPackage(path, inputs)
	if err != nil {
		t.Fatal(err)
	}

	if len(m.Resources) != 3 {
		t.Fatalf("expected 3 distinct resources, got %d", len(m.Resources))
	}
	chunkCount := countChunksFromInputs(inputs)
	if len(m.Chunks) != chunkCount {
		t.Fatalf("expected %d deduplicated chunks, got %d", chunkCount, len(m.Chunks))
	}

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	pidA, err := p.PathID("art/a-palette.bin")
	if err != nil {
		t.Fatal(err)
	}
	pidB, err := p.PathID("art/b-palette.bin")
	if err != nil {
		t.Fatal(err)
	}
	if pidA != pidB {
		t.Fatal("shared paths must point at one content identity")
	}
	if got := p.Manifest(); got.PackageID != m.PackageID {
		t.Fatal("package id mismatch")
	}

	data, err := p.ReadPath("art/texture.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, texture) {
		t.Fatal("multi-chunk resource mismatch")
	}
	if _, err := p.ReadPath("scene/root.json"); err != nil {
		t.Fatal(err)
	}

	var root Resource
	for _, r := range p.Manifest().Resources {
		if len(r.Children) > 0 {
			root = r
		}
	}
	if len(root.Children) != 2 {
		t.Fatalf("nested references lost: %+v", root)
	}
	if err := assertCycleRecoverable(p); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Extract(ExtractOptions{Root: out}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(out, "art/texture.bin")); err != nil || !bytes.Equal(got, texture) {
		t.Fatal("extracted resource verification failed")
	}
}

func assertCycleRecoverable(p *Package) error {
	g := p.DependencyGraph()
	visited := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		for _, dep := range g[id] {
			walk(dep)
		}
	}
	for id := range g {
		walk(id)
	}
	root, err := p.PathID("scene/root.json")
	if err != nil {
		return err
	}
	palette, err := p.PathID("art/a-palette.bin")
	if err != nil {
		return err
	}
	if !contains(g[root], palette) || !contains(g[palette], root) {
		return bytes.ErrTooLarge
	}
	return nil
}

func TestDeterministicAndIncrementalIdentity(t *testing.T) {
	dir := t.TempDir()
	unchanged := bigData(7, chunkSize+10)
	inputsV1 := []InputResource{
		{Paths: []string{"shared.bin"}, Data: unchanged},
		{Paths: []string{"v1.bin"}, Data: []byte("version one")},
	}
	m1a, err := BuildPackage(filepath.Join(dir, "a.dap"), inputsV1)
	if err != nil {
		t.Fatal(err)
	}
	m1b, err := BuildPackage(filepath.Join(dir, "b.dap"), inputsV1)
	if err != nil {
		t.Fatal(err)
	}
	if m1a.PackageID != m1b.PackageID {
		t.Fatal("manifest must be stable for fixed input")
	}
	a, err := os.ReadFile(filepath.Join(dir, "a.dap"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "b.dap"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("package bytes must be stable for fixed input")
	}

	inputsV2 := []InputResource{
		{Paths: []string{"shared.bin"}, Data: unchanged},
		{Paths: []string{"v2.bin"}, Data: []byte("version two")},
	}
	m2, err := BuildDelta(filepath.Join(dir, "v2.dap"), &m1a, 2, inputsV2)
	if err != nil {
		t.Fatal(err)
	}
	if m2.BaseID != m1a.PackageID || m2.Version != 2 {
		t.Fatal("incremental base metadata missing")
	}
	sharedID := ContentID(unchanged)
	var sharedChunks []string
	for _, r := range m2.Resources {
		if r.ID == sharedID {
			sharedChunks = r.Chunks
		}
	}
	for _, cid := range sharedChunks {
		if _, ok := chunkIndex(m2.Chunks, cid); ok {
			t.Fatalf("unchanged chunk %s should be inherited, not republished", cid)
		}
	}

	p1, err := Open(filepath.Join(dir, "a.dap"))
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := Open(filepath.Join(dir, "v2.dap"))
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	chain, err := NewChain(p2, p1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := chain.ReadPath("shared.bin")
	if err != nil || !bytes.Equal(data, unchanged) {
		t.Fatalf("incremental shared read failed: %v", err)
	}
	if data, err := chain.ReadPath("v2.bin"); err != nil || string(data) != "version two" {
		t.Fatalf("incremental changed read failed: %v", err)
	}
}

func TestCorruptFragmentRejected(t *testing.T) {
	dir := t.TempDir()
	data := bigData(9, chunkSize+1)
	path := filepath.Join(dir, "p.dap")
	if _, err := BuildPackage(path, []InputResource{{Paths: []string{"x"}, Data: data}}); err != nil {
		t.Fatal(err)
	}
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := p.manifest.Chunks[0]
	p.Close()
	wf, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.WriteAt([]byte{0xff ^ data[0]}, first.Offset); err != nil {
		t.Fatal(err)
	}
	wf.Close()
	p, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ReadPath("x"); err == nil || !strings.Contains(err.Error(), "corrupt chunk") {
		t.Fatalf("expected corrupt chunk error, got %v", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	bad := []string{"../x", "a/../../x", "/abs/x", "a\\b", "a/./b", "a//b"}
	for _, name := range bad {
		if err := ValidateLogicalPath(name); err == nil {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "p.dap")
	_, err := BuildPackage(path, []InputResource{{Paths: bad[:1], Data: []byte("x")}})
	if err == nil {
		t.Fatal("packer accepted traversal path")
	}
}

func TestConflictingPathTreeRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := BuildPackage(filepath.Join(dir, "p.dap"), []InputResource{
		{Paths: []string{"a"}, Data: []byte("file")},
		{Paths: []string{"a/b"}, Data: []byte("child")},
	})
	if err == nil {
		t.Fatal("accepted conflicting file and directory path")
	}
}

func TestNoIndexBeforeCompletion(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "index.dap")
	if err := os.WriteFile(desired, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A failed validation must leave an existing target untouched.
	_, err := BuildPackage(desired, []InputResource{{Paths: []string{"../escape"}}})
	if err == nil {
		t.Fatal("expected build failure")
	}
	got, err := os.ReadFile(desired)
	if err != nil || string(got) != "old" {
		t.Fatal("target was published before completion")
	}
}

func countChunksFromInputs(inputs []InputResource) int {
	set := map[string]bool{}
	for _, in := range inputs {
		off := 0
		for off < len(in.Data) {
			end := off + chunkSize
			if end > len(in.Data) {
				end = len(in.Data)
			}
			sum := sha256.Sum256(in.Data[off:end])
			set[hex.EncodeToString(sum[:])] = true
			off = end
		}
	}
	return len(set)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
