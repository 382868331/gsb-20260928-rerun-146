package assetpack

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExtractOptions controls safe materialization of package paths.
type ExtractOptions struct {
	Root string
	// Overwrite permits replacement of regular destination files.
	Overwrite bool
}

// Extract writes all logical paths after validating each path and verifying
// resource size and digest. Destinations are written to temporary files first
// and renamed only after successful verification.
func (p *Package) Extract(opts ExtractOptions) error {
	if opts.Root == "" {
		return fmt.Errorf("extract root is required")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("extract root is not a directory")
	}

	for _, r := range p.manifest.Resources {
		for _, logical := range r.Paths {
			if err := ValidateLogicalPath(logical); err != nil {
				return err
			}
			dest := filepath.Join(root, filepath.FromSlash(logical))
			rel, err := filepath.Rel(root, dest)
			if err != nil || rel == ".." || filepath.IsAbs(rel) || hasParentEscape(rel) {
				return fmt.Errorf("path traversal rejected: %q", logical)
			}
			if err := rejectExistingUnsafe(dest, opts.Overwrite); err != nil {
				return err
			}
		}
	}

	written := make(map[string]string)
	for _, r := range p.manifest.Resources {
		data, err := p.ReadBytes(r.ID)
		if err != nil {
			return err
		}
		if int64(len(data)) != r.Size || ContentID(data) != r.ID {
			return fmt.Errorf("resource %s failed size/digest verification", r.ID)
		}
		for _, logical := range r.Paths {
			dest := filepath.Join(root, filepath.FromSlash(logical))
			if existing, ok := written[dest]; ok {
				if existing != r.ID {
					return fmt.Errorf("internal error: conflicting path %q", logical)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if err := writeVerifiedFile(dest, data); err != nil {
				return err
			}
			written[dest] = r.ID
		}
	}
	return nil
}

func writeVerifiedFile(dest string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".extract-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	var closeErr error
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if closeErr = f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func rejectExistingUnsafe(path string, overwrite bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("destination is a directory: %s", path)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular file: %s", path)
	}
	if !overwrite {
		return fmt.Errorf("destination exists: %s", path)
	}
	return nil
}

func hasParentEscape(rel string) bool {
	rel = filepath.ToSlash(rel)
	for rel != "." && rel != "/" && rel != "" {
		dir, base := filepath.Split(filepath.FromSlash(rel))
		if base == ".." {
			return true
		}
		rel = filepath.ToSlash(filepath.Clean(dir))
	}
	return false
}
