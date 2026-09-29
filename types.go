package assetpack

// Manifest is the published index of an asset package.
//
// A manifest only becomes visible after all data chunks are written and the
// package file is atomically renamed into place.
type Manifest struct {
	// PackageID identifies the exact, canonical manifest contents.
	PackageID string `json:"package_id"`
	Version   int    `json:"version"`

	// Chunks are content-addressed storage fragments, ordered by digest.
	Chunks []ChunkInfo `json:"chunks"`

	// Resources are logical digital assets, ordered by content ID.
	Resources []Resource `json:"resources"`

	// BaseID is the package this incremental package is built on.
	// It is empty for a complete package.
	BaseID string `json:"base_id,omitempty"`
}

// ChunkInfo records where one immutable fragment is stored.
type ChunkInfo struct {
	Digest string `json:"digest"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// Resource is a logical asset. Multiple paths may point at one shared content
// ID while dependency and nesting edges remain explicit.
type Resource struct {
	ID       string     `json:"id"`
	Paths    []string   `json:"paths"`
	Size     int64      `json:"size"`
	Chunks   []string   `json:"chunks"`
	Deps     []string   `json:"deps,omitempty"`
	Children []ChildRef `json:"children,omitempty"`
}

// ChildRef binds a nested logical resource to a name inside its parent.
type ChildRef struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// InputResource describes one logical asset supplied to the packer.
type InputResource struct {
	// Paths are logical names. Repeated or equal paths across resources are
	// rejected; identical data with different paths stores bytes only once.
	Paths []string
	Data  []byte

	// Deps indexes other InputResource values and may form cycles.
	Deps []int

	// Children indexes resources nested beneath this resource. Cycles are
	// allowed and remain recoverable from the manifest.
	Children []NestedInput
}

// NestedInput associates a nested resource index with a stable member name.
type NestedInput struct {
	Name string
	Ref  int
}
