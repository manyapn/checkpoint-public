package store

// defaultExcludes is the default-skip list of rebuildable dirs (protect
// meaningful work, not regenerable bulk), plus .git. Git is excluded because
// .git/objects churns on every operation, and capturing it poisons both runtime
// overhead and store size for content git is already keeping safe. The store
// dir lives out-of-tree, so it needs no self-exclusion. The list is fixed, not
// user-configurable.
//
// This map is the ONE authoritative definition of WHICH names are excluded.
// Every consumer keeps its own matching semantics: scanTree skips by basename,
// the fold scans path segments, capture matches components relative to the
// protected root (see IsBulkExcluded), and an exact restore must NEVER remove
// an excluded tree (pruneToManifest). Only the vocabulary is shared; drift
// between copies of this list once shipped a real exclusion bug.
var defaultExcludes = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}

// IsBulkExcluded reports whether a single path component names a
// bulk-excluded directory. It is the exported face of defaultExcludes for
// consumers outside this package (capture's component matching, doctor's
// sizing walk); it deliberately takes one component, not a path, so each
// consumer keeps its own path-interpretation semantics.
func IsBulkExcluded(component string) bool { return defaultExcludes[component] }
