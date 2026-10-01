package assetpack

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openBytes(t *testing.T, pkg []byte) *Reader {
	t.Helper()
	r, err := OpenReaderAt(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r
}

func physicalDigests(t *testing.T, r *Reader) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(r.fragIndex))
	for d := range r.fragIndex {
		out[d] = true
	}
	return out
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestRoundTripSharedAndCyclic(t *testing.T) {
	shared := bytes.Repeat([]byte("SHARED-BLOCK:"), 5000)
	reqs := []AddRequest{
		{Path: "scenes/a.json", Data: append(append([]byte(nil), shared...), 'A'), DependsOn: []string{"textures/t.png", "scenes/b.json"}},
		{Path: "scenes/b.json", Data: append(append([]byte(nil), shared...), 'B'), DependsOn: []string{"scenes/a.json"}},
		{Path: "textures/t.png", Data: shared},
		{Path: "scenes", IsDir: true, Children: []string{"scenes/a.json", "scenes/b.json"}},
	}
	pkg, m, err := Build(reqs, Options{FragmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	r := openBytes(t, pkg)
	if err := r.VerifyAll(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	refCount := map[string]int{}
	totalRefs := 0
	for _, res := range m.Resources {
		for _, f := range res.Fragments {
			refCount[f.Digest]++
			totalRefs++
		}
	}
	sharedCount := 0
	for _, c := range refCount {
		if c > 1 {
			sharedCount++
		}
	}
	if sharedCount == 0 || len(refCount) >= totalRefs {
		t.Fatalf("deduplication not evident: unique=%d refs=%d", len(refCount), totalRefs)
	}
	a, _ := r.Lookup("scenes/a.json")
	b, _ := r.Lookup("scenes/b.json")
	if !containsStr(a.DependsOn, "scenes/b.json") || !containsStr(b.DependsOn, "scenes/a.json") {
		t.Fatalf("cyclic dependency lost: %v %v", a.DependsOn, b.DependsOn)
	}
	dir := t.TempDir()
	res, err := Unpack(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Directories != 1 {
		t.Fatalf("unexpected counts %+v", res)
	}
	for _, p := range []string{"scenes/a.json", "scenes/b.json", "textures/t.png"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := r.ReadResource(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("content mismatch for %s", p)
		}
	}
	mb, err := os.ReadFile(filepath.Join(dir, metaDir, "manifest.json"))
	if err != nil || !strings.Contains(string(mb), "scenes/b.json") {
		t.Fatalf("dependency index missing: %v", err)
	}
}

func TestDeterministic(t *testing.T) {
	reqs := []AddRequest{
		{Path: "b", Data: []byte("data-b"), DependsOn: []string{"a"}},
		{Path: "a", Data: []byte("data-a")},
	}
	opts := Options{FragmentSize: 1024}
	p1, m1, _ := Build(reqs, opts)
	p2, m2, _ := Build(reqs, opts)
	if !bytes.Equal(p1, p2) {
		t.Fatal("package bytes not deterministic")
	}
	if m1.IndexDigest != m2.IndexDigest {
		t.Fatal("index digest not stable")
	}
}

func TestCorruptFragmentDetected(t *testing.T) {
	reqs := []AddRequest{
		{Path: "a", Data: bytes.Repeat([]byte("A"), 9000)},
		{Path: "b", Data: bytes.Repeat([]byte("B"), 9000)},
	}
	pkg, _, err := Build(reqs, Options{FragmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	good := openBytes(t, pkg)
	aRes, _ := good.Lookup("a")
	loc := good.fragIndex[aRes.Fragments[0].Digest]
	tampered := append([]byte(nil), pkg...)
	tampered[loc.offset] ^= 0xFF
	tr := openBytes(t, tampered)
	if _, err := tr.ReadResource("a"); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("expected ErrBadDigest for corrupted a, got %v", err)
	}
	if _, err := tr.ReadResource("b"); err != nil {
		t.Fatalf("unaffected resource should still read: %v", err)
	}
	if err := tr.VerifyAll(); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("VerifyAll should flag corruption, got %v", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	cases := []string{"../evil", "a/../../evil", "/etc/passwd", metaDir + "/manifest.json"}
	for _, p := range cases {
		if _, _, err := Build([]AddRequest{{Path: p, Data: []byte("x")}}, Options{}); err == nil {
			t.Fatalf("expected rejection for %q", p)
		}
	}
	if _, err := safeJoin(t.TempDir(), "../x"); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal, got %v", err)
	}
}

func TestIncrementalReusesIdentity(t *testing.T) {
	baseReqs := []AddRequest{
		{Path: "keep", Data: bytes.Repeat([]byte("K"), 9000)},
		{Path: "old", Data: []byte("v1")},
	}
	basePkg, baseM, err := Build(baseReqs, Options{FragmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	baseReader := openBytes(t, basePkg)
	deltaReqs := []AddRequest{
		{Path: "old", Data: []byte("v2-changed")},
		{Path: "new", Data: bytes.Repeat([]byte("N"), 5000)},
	}
	deltaPkg, deltaM, err := Build(deltaReqs, Options{FragmentSize: 4096, Base: baseM})
	if err != nil {
		t.Fatal(err)
	}
	if deltaM.BaseIndex != baseM.IndexDigest {
		t.Fatal("delta must reference base index identity")
	}
	deltaReader := &Reader{ra: bytes.NewReader(deltaPkg), size: int64(len(deltaPkg))}
	if err := deltaReader.parse(); err != nil {
		t.Fatal(err)
	}
	deltaPhys := physicalDigests(t, deltaReader)
	keepRes, _ := baseReader.Lookup("keep")
	if deltaPhys[keepRes.Fragments[0].Digest] {
		t.Fatal("unchanged fragment re-stored in delta")
	}
	// 未变更的 keep(3 个片段)全部不得出现在增量包物理区。
	for _, fr := range keepRes.Fragments {
		if deltaPhys[fr.Digest] {
			t.Fatalf("unchanged keep fragment %s re-stored", fr.Digest[:8])
		}
	}
	chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.VerifyAll(); err != nil {
		t.Fatalf("chain verify: %v", err)
	}
	got, err := chain.ReadResource("keep")
	if err != nil || !bytes.HasPrefix(got, []byte("KKK")) {
		t.Fatalf("unchanged resource not restored from base: %v", err)
	}
	old, err := chain.ReadResource("old")
	if err != nil || string(old) != "v2-changed" {
		t.Fatalf("updated resource mismatch: %q %v", old, err)
	}
	if _, err := chain.ReadResource("new"); err != nil {
		t.Fatalf("new resource missing: %v", err)
	}
	if _, err := OpenReaderAt(bytes.NewReader(deltaPkg), int64(len(deltaPkg))); err == nil {
		t.Fatal("opening delta without base should fail")
	}
}

func TestTruncatedAndTrailerTamper(t *testing.T) {
	pkg, _, _ := Build([]AddRequest{{Path: "a", Data: []byte("hello")}}, Options{})
	if _, err := OpenReaderAt(bytes.NewReader(pkg[:len(pkg)-1]), int64(len(pkg)-1)); err == nil {
		t.Fatal("truncated package must fail")
	}
	tampered := append([]byte(nil), pkg...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := OpenReaderAt(bytes.NewReader(tampered), int64(len(tampered))); err == nil {
		t.Fatal("tampered trailer must fail")
	}
}

func TestUnpackRejectsCorruptBeforePublish(t *testing.T) {
	pkg, _, _ := Build([]AddRequest{{Path: "a", Data: bytes.Repeat([]byte("Z"), 5000)}}, Options{FragmentSize: 2048})
	good := openBytes(t, pkg)
	aRes, _ := good.Lookup("a")
	loc := good.fragIndex[aRes.Fragments[0].Digest]
	tampered := append([]byte(nil), pkg...)
	tampered[loc.offset] ^= 0xFF
	tr := openBytes(t, tampered)
	dir := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(tr, dir); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("expected ErrBadDigest, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("no file may be published from a failing package, stat err=%v", err)
	}
}

func assertPathAbsent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Fatalf("%s should be absent, stat err=%v", name, err)
	}
}

func assertIndexAbsent(t *testing.T, root string) {
	t.Helper()
	assertPathAbsent(t, root, metaDir+"/manifest.json")
}

func readerFromPackage(t *testing.T, pkg []byte) *Reader {
	t.Helper()
	r := &Reader{ra: bytes.NewReader(pkg), size: int64(len(pkg))}
	if err := r.parse(); err != nil {
		t.Fatal(err)
	}
	return r
}

func tamperResourceFragment(t *testing.T, pkg []byte, path string) []byte {
	t.Helper()
	good := readerFromPackage(t, pkg)
	res, ok := good.Lookup(path)
	if !ok {
		t.Fatalf("resource %s missing", path)
	}
	loc := good.fragIndex[res.Fragments[0].Digest]
	out := append([]byte(nil), pkg...)
	out[loc.offset] ^= 0xFF
	return out
}

func TestUnpackProtectsRealPathFullAndIncremental(t *testing.T) {
	t.Run("nested round trip", func(t *testing.T) {
		basePkg, _, err := Build([]AddRequest{
			{Path: "scenes/level/one.json", Data: []byte("{\"id\":1}")},
			{Path: "textures/shared.png", Data: bytes.Repeat([]byte("T"), 5000)},
			{Path: "scenes/level", IsDir: true, Children: []string{"scenes/level/one.json"}},
		}, Options{FragmentSize: 2048})
		if err != nil {
			t.Fatal(err)
		}
		baseDir := filepath.Join(t.TempDir(), "base")
		if _, err := Unpack(openBytes(t, basePkg), baseDir); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(baseDir, "scenes/level/one.json")); err != nil || string(got) != "{\"id\":1}" {
			t.Fatalf("full nested resource mismatch: %q %v", got, err)
		}

		baseReader := readerFromPackage(t, basePkg)
		deltaPkg, deltaM, err := Build([]AddRequest{
			{Path: "scenes/level/two.json", Data: []byte("{\"id\":2}")},
			{Path: "scenes/level", IsDir: true, Children: []string{"scenes/level/one.json", "scenes/level/two.json"}},
		}, Options{FragmentSize: 2048, Base: baseReader.merged})
		if err != nil {
			t.Fatal(err)
		}
		chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
		if err != nil {
			t.Fatal(err)
		}
		deltaDir := filepath.Join(t.TempDir(), "delta")
		if _, err := Unpack(chain, deltaDir); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"scenes/level/one.json", "scenes/level/two.json", "textures/shared.png"} {
			want, err := chain.ReadResource(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(deltaDir, filepath.FromSlash(p)))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("incremental nested resource %s mismatch: %v", got, err)
			}
		}
		if deltaM.BaseIndex == "" {
			t.Fatal("delta package must reference base index")
		}
	})

	t.Run("preset symlink", func(t *testing.T) {
		cases := []struct {
			name      string
			resources []AddRequest
			linkPath  string
			basePkg   []byte
		}{
			{
				name: "full",
				resources: []AddRequest{
					{Path: "outside/child/file.txt", Data: []byte("escaped")},
					{Path: "outside/child", IsDir: true, Children: []string{"outside/child/file.txt"}},
				},
				linkPath: "outside",
			},
			{
				name: "incremental",
				resources: []AddRequest{
					{Path: "delta/child/file.txt", Data: []byte("delta escaped")},
					{Path: "delta/child", IsDir: true, Children: []string{"delta/child/file.txt"}},
				},
				linkPath: "delta",
				basePkg: func() []byte {
					pkg, _, err := Build([]AddRequest{{Path: "base.txt", Data: []byte("base")}}, Options{FragmentSize: 2048})
					if err != nil {
						t.Fatal(err)
					}
					return pkg
				}(),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				work := t.TempDir()
				root := filepath.Join(work, "root")
				sentinel := filepath.Join(work, "outside")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(sentinel, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(root, tc.linkPath)); err != nil {
					t.Fatal(err)
				}

				pkg, _, err := Build(tc.resources, Options{FragmentSize: 2048})
				if err != nil {
					t.Fatal(err)
				}
				var reader *Reader
				if tc.basePkg != nil {
					baseReader := readerFromPackage(t, tc.basePkg)
					deltaPkg, _, err := Build(tc.resources, Options{FragmentSize: 2048, Base: baseReader.merged})
					if err != nil {
						t.Fatal(err)
					}
					reader, err = OpenChain([]ReaderAtSize{bytes.NewReader(tc.basePkg), bytes.NewReader(deltaPkg)})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					reader = openBytes(t, pkg)
				}
				if _, err := Unpack(reader, root); !errors.Is(err, ErrPathTraversal) {
					t.Fatalf("expected ErrPathTraversal, got %v", err)
				}
				assertIndexAbsent(t, root)
				if entries, err := os.ReadDir(sentinel); err != nil || len(entries) != 0 {
					t.Fatalf("sentinel outside root changed: entries=%v err=%v", entries, err)
				}
			})
		}
	})

	t.Run("duplicate resources", func(t *testing.T) {
		pkg, manifest, err := Build([]AddRequest{{Path: "a.txt", Data: []byte("same")}}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		r := readerFromPackage(t, pkg)
		dup := manifest.Resources[0]
		r.merged.Resources = append(r.merged.Resources, dup)
		root := filepath.Join(t.TempDir(), "out")
		if _, err := Unpack(r, root); !errors.Is(err, ErrPathTraversal) {
			t.Fatalf("expected duplicate rejection, got %v", err)
		}
		assertPathAbsent(t, root, "a.txt")
		assertIndexAbsent(t, root)
	})

	t.Run("mid verification failure", func(t *testing.T) {
		resources := []AddRequest{
			{Path: "early/nested.txt", Data: []byte("early")},
			{Path: "late.bin", Data: bytes.Repeat([]byte("L"), 5000)},
		}
		pkg, _, err := Build(resources, Options{FragmentSize: 2048})
		if err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name   string
			reader *Reader
		}{
			{name: "full", reader: openBytes(t, tamperResourceFragment(t, pkg, "late.bin"))},
		}
		basePkg, _, err := Build([]AddRequest{{Path: "base.txt", Data: []byte("base")}}, Options{FragmentSize: 2048})
		if err != nil {
			t.Fatal(err)
		}
		baseReader := readerFromPackage(t, basePkg)
		deltaPkg, _, err := Build(resources, Options{FragmentSize: 2048, Base: baseReader.merged})
		if err != nil {
			t.Fatal(err)
		}
		deltaPkg = tamperResourceFragment(t, deltaPkg, "late.bin")
		chain, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			name   string
			reader *Reader
		}{"incremental", chain})
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "out")
				if _, err := Unpack(tc.reader, root); !errors.Is(err, ErrBadDigest) {
					t.Fatalf("expected ErrBadDigest, got %v", err)
				}
				assertPathAbsent(t, root, "early/nested.txt")
				assertPathAbsent(t, root, "late.bin")
				assertIndexAbsent(t, root)
			})
		}
	})
}

func TestMissingDependencyRejected(t *testing.T) {
	_, _, err := Build([]AddRequest{{Path: "a", Data: []byte("x"), DependsOn: []string{"ghost"}}}, Options{})
	if err == nil {
		t.Fatal("dangling dependency must fail build")
	}
}
