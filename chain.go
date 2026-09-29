package assetpack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Chain reads a newest package and falling back through its base packages.
// Only the newest manifest is used; older packages provide missing fragments.
type Chain struct {
	packages []*Package
}

// NewChain requires packages ordered newest-first. Each package except the
// newest must be the declared direct predecessor.
func NewChain(packages ...*Package) (*Chain, error) {
	if len(packages) == 0 {
		return nil, fmt.Errorf("chain requires at least one package")
	}
	for i := 0; i+1 < len(packages); i++ {
		newer := packages[i].Manifest()
		older := packages[i+1].Manifest()
		if newer.BaseID == "" || newer.BaseID != older.PackageID {
			return nil, fmt.Errorf("chain break: %s is not based on %s", newer.PackageID, older.PackageID)
		}
	}
	return &Chain{packages: packages}, nil
}

func (c *Chain) Close() error {
	var first error
	for _, p := range c.packages {
		if err := p.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Manifest is the newest, complete resource view.
func (c *Chain) Manifest() Manifest { return c.packages[0].Manifest() }

// ReadBytes resolves resources from newest manifest and chunks down the chain.
func (c *Chain) ReadBytes(id string) ([]byte, error) {
	r, err := c.packages[0].findResource(id)
	if err != nil {
		return nil, err
	}
	var data []byte
	for _, cid := range r.Chunks {
		var found bool
		for _, p := range c.packages {
			if candidate, ok := chunkIndex(p.manifest.Chunks, cid); ok {
				found = true
				// Read through the package that physically owns this fragment.
				part, err := p.readLocalChunk(candidate)
				if err != nil {
					return nil, err
				}
				data = append(data, part...)
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("missing chunk %s in package chain", cid)
		}
	}
	if int64(len(data)) != r.Size || ContentID(data) != r.ID {
		return nil, fmt.Errorf("resource %s failed verification", r.ID)
	}
	return data, nil
}

// ReadPath resolves and verifies a logical path.
func (c *Chain) ReadPath(path string) ([]byte, error) {
	id, err := c.packages[0].PathID(path)
	if err != nil {
		return nil, err
	}
	return c.ReadBytes(id)
}

func (p *Package) readLocalChunk(c ChunkInfo) ([]byte, error) {
	buf := make([]byte, c.Size)
	if _, err := p.file.ReadAt(buf, c.Offset); err != nil && err != io.EOF {
		return nil, err
	}
	h := sha256.Sum256(buf)
	if int64(len(buf)) != c.Size || hex.EncodeToString(h[:]) != c.Digest {
		return nil, fmt.Errorf("corrupt chunk %s: size or digest mismatch", c.Digest)
	}
	return buf, nil
}
