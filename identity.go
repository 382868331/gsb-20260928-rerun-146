package assetpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

const chunkSize = 64 * 1024

// ContentID returns the stable, content-derived identity for raw bytes.
func ContentID(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func rawDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestID(raw string) string { return "sha256:" + raw }

func validDigest(id string) bool {
	if !strings.HasPrefix(id, "sha256:") {
		return false
	}
	rest := id[len("sha256:"):]
	if len(rest) != 64 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

func hexSHA256(data []byte) string { return rawDigest(data) }

func manifestID(m *Manifest) (string, error) {
	copyM := *m
	copyM.PackageID = ""
	raw, err := json.Marshal(&copyM)
	if err != nil {
		return "", fmt.Errorf("canonical manifest: %w", err)
	}
	return ContentID(raw), nil
}

// ValidateLogicalPath rejects absolute, escaping, empty, or platform-ambiguous
// logical paths. Paths always use forward slashes, even on Windows.
func ValidateLogicalPath(name string) error {
	if name == "" || path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("path must be relative: %q", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("backslash is not allowed in path: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != name {
		return fmt.Errorf("unsafe logical path: %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".", "..":
			return fmt.Errorf("unsafe path component in %q", name)
		}
	}
	return nil
}

// ValidatePathTree rejects paths that cannot coexist in a file tree, such as
// both "a" and "a/b".
func ValidatePathTree(paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for i := 0; i+1 < len(sorted); i++ {
		prefix := sorted[i] + "/"
		if strings.HasPrefix(sorted[i+1], prefix) {
			return fmt.Errorf("path %q conflicts with nested path %q", sorted[i], sorted[i+1])
		}
	}
	return nil
}
