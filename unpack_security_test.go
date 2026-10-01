package assetpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repackManifest 重写包尾清单并重新签名, 用于构造 Build 拒绝的恶意清单。
func repackManifest(t *testing.T, pkg []byte, mutate func(*Manifest)) []byte {
	t.Helper()
	trailerAt := len(pkg) - trailerLen
	indexOffset := int64(binary.BigEndian.Uint64(pkg[trailerAt : trailerAt+8]))
	var m Manifest
	if err := json.Unmarshal(pkg[indexOffset:trailerAt], &m); err != nil {
		t.Fatal(err)
	}
	mutate(&m)
	// 清单身份随内容重算(解析时会核对无 indexDigest 的规范 JSON)。
	var raw map[string]json.RawMessage
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "indexDigest")
	canon, _ := canonicalJSON(raw)
	m.IndexDigest = hashBytes(canon)
	mf, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), pkg[:indexOffset]...)
	out = append(out, mf...)
	var tr [trailerLen]byte
	binary.BigEndian.PutUint64(tr[0:8], uint64(indexOffset))
	binary.BigEndian.PutUint64(tr[8:16], uint64(len(mf)))
	sum := sha256.Sum256(mf)
	copy(tr[16:48], sum[:])
	copy(tr[48:], endMagic)
	return append(out, tr[:]...)
}

func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, metaDir))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stage-") {
			t.Fatalf("staging dir leaked after failure: %s", e.Name())
		}
	}
}

// TestUnpackNestedRoundTrip: 普通嵌套资源在隔离临时目录中正常往返。
func TestUnpackNestedRoundTrip(t *testing.T) {
	reqs := []AddRequest{
		{Path: "a/b/c1.txt", Data: []byte("c1")},
		{Path: "a/b/c2.txt", Data: bytes.Repeat([]byte("x"), 7000)},
		{Path: "a", IsDir: true, Children: []string{"a/b/c1.txt", "a/b/c2.txt"}},
		{Path: "a/b", IsDir: true, Children: []string{"a/b/c1.txt", "a/b/c2.txt"}},
	}
	pkg, _, err := Build(reqs, Options{FragmentSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Unpack(openBytes(t, pkg), dir); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"a/b/c1.txt": "c1"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || string(got) != want {
			t.Fatalf("round trip mismatch for %s: %q %v", p, got, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "a", "b")); err != nil || !fi.IsDir() {
		t.Fatalf("nested dir missing: %v %v", fi, err)
	}
	mb, err := os.ReadFile(filepath.Join(dir, metaDir, "manifest.json"))
	if err != nil || !strings.Contains(string(mb), "a/b/c2.txt") {
		t.Fatalf("manifest index missing: %v", err)
	}
}

// TestUnpackPreexistingSymlinkRejected: 目标目录中预置的逃逸符号链接
// (根级、嵌套逐级、目录型) 都不能把写入带到 root 之外。
func TestUnpackPreexistingSymlinkRejected(t *testing.T) {
	cases := []struct {
		name string
		link string // 相对 root 的预置链接位置
		res  string // 包内试图借链接写出的资源
	}{
		{"root level dir link", "evil", "evil/payload.txt"},
		{"nested level dir link", "sub/evil", "sub/evil/payload.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "payload.txt")

			linkParent := filepath.Join(dir, filepath.FromSlash(filepath.Dir(tc.link)))
			if err := os.MkdirAll(linkParent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, filepath.FromSlash(tc.link))); err != nil {
				t.Fatal(err)
			}

			reqs := []AddRequest{{Path: tc.res, Data: []byte("PWNED")}}
			pkg, _, err := Build(reqs, Options{FragmentSize: 1024})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Unpack(openBytes(t, pkg), dir); !errors.Is(err, ErrPathTraversal) {
				t.Fatalf("expected ErrPathTraversal, got %v", err)
			}
			if _, err := os.Lstat(sentinel); !os.IsNotExist(err) {
				t.Fatalf("content escaped root via symlink: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, metaDir, "manifest.json")); !os.IsNotExist(err) {
				t.Fatal("relationship index must not be published on failure")
			}
			assertNoStaging(t, dir)
		})
	}
}

// TestUnpackFileSymlinkReplacedSafely: 末级是指向 root 外的符号链接时,
// 发布只能替换链接本身, 绝不跟随链接修改外部目标。
func TestUnpackFileSymlinkReplacedSafely(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	pkg, _, err := Build([]AddRequest{{Path: "a.txt", Data: []byte("NEW")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(openBytes(t, pkg), dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "ORIGINAL" {
		t.Fatalf("outside target modified through symlink: %q %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "a.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("symlink must be replaced by a regular in-root file: %v %v", fi, err)
	}
	inRoot, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(inRoot) != "NEW" {
		t.Fatalf("published content mismatch: %q", inRoot)
	}
}

// TestUnpackDuplicateResourceRejected: 绕过 Build 直接构造的重复路径清单
// 必须在发布前拒绝, 且不修改 root。
func TestUnpackDuplicateResourceRejected(t *testing.T) {
	pkg, _, err := Build([]AddRequest{{Path: "dup", Data: []byte("x")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	evil := repackManifest(t, pkg, func(m *Manifest) {
		m.Resources = append(m.Resources, m.Resources[0])
	})
	dir := t.TempDir()
	if _, err := Unpack(openBytes(t, evil), dir); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal for duplicate path, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dup")); !os.IsNotExist(err) {
		t.Fatal("nothing may land in root on duplicate failure")
	}
	if _, err := os.Stat(filepath.Join(dir, metaDir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("index must not be published on duplicate failure")
	}
	assertNoStaging(t, dir)
}

// TestUnpackFailureWithSymlinkLeavesRootUntouched: 内容核对阶段失败(损坏包)
// 叠加预置链接时, 既不发布索引, 也不触碰 root 外。
func TestUnpackFailureWithSymlinkLeavesRootUntouched(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "payload.txt")
	if err := os.WriteFile(sentinel, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	pkg, _, _ := Build([]AddRequest{
		{Path: "evil/payload.txt", Data: bytes.Repeat([]byte("Z"), 5000)},
	}, Options{FragmentSize: 2048})
	good := openBytes(t, pkg)
	aRes, _ := good.Lookup("evil/payload.txt")
	loc := good.fragIndex[aRes.Fragments[0].Digest]
	tampered := append([]byte(nil), pkg...)
	tampered[loc.offset] ^= 0xFF
	if _, err := Unpack(openBytes(t, tampered), dir); !errors.Is(err, ErrBadDigest) {
		t.Fatalf("expected ErrBadDigest, got %v", err)
	}
	got, _ := os.ReadFile(sentinel)
	if string(got) != "KEEP" {
		t.Fatalf("outside content modified: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, metaDir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("index must not be published on failed unpack")
	}
	assertNoStaging(t, dir)
}

// TestUnpackIncrementalSharesProtection: 增量包走同一发布路径,
// 预置链接同样被拒; 正常增量包则完整往返。
func TestUnpackIncrementalSharesProtection(t *testing.T) {
	basePkg, baseM, err := Build([]AddRequest{
		{Path: "keep.txt", Data: []byte("keep-content")},
		{Path: "old.txt", Data: []byte("v1")},
	}, Options{FragmentSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	deltaPkg, _, err := Build([]AddRequest{
		{Path: "old.txt", Data: []byte("v2")},
		{Path: "new.txt", Data: []byte("NEW")},
	}, Options{FragmentSize: 1024, Base: baseM})
	if err != nil {
		t.Fatal(err)
	}
	chainPkgs := []ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)}

	// 攻击面: 增量发布目录预置逃逸链接。
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	ch, err := OpenChain(chainPkgs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(ch, dir); err != nil {
		t.Fatalf("symlink at file target should be safely replaced, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("delta content escaped through symlink")
	}

	// 正常增量包完整往返: 未变更资源来自 base, 变更资源取 delta。
	dir2 := t.TempDir()
	ch2, err := OpenChain([]ReaderAtSize{bytes.NewReader(basePkg), bytes.NewReader(deltaPkg)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(ch2, dir2); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{"keep.txt": "keep-content", "old.txt": "v2", "new.txt": "NEW"}
	for name, want := range checks {
		got, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil || string(got) != want {
			t.Fatalf("incremental round trip %s: %q %v", name, got, err)
		}
	}
}
