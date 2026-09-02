package store

// defaultExcludes is the default-skip list of rebuildable dirs (protect
// meaningful work, not regenerable bulk), plus .git. Git is excluded because
// .git/objects churns on every operation, and capturing it poisons both runtime
// overhead and store size for content git is already keeping safe. The store
// dir lives out-of-tree, so it needs no self-exclusion. The list is fixed, not
// user-configurable.
var defaultExcludes = map[string]bool{
	".git": true, "node_modules": true, "build": true, "target": true,
	"dist": true, "__pycache__": true, ".venv": true,
}
