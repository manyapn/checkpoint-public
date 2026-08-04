//go:build !linux

package selftest

// checkpoint captures writes through fanotify, which exists only on Linux. The
// package still compiles elsewhere so `go build ./...` and `go vet ./...` work
// on a developer's laptop; the environment probes report honestly that they
// could not run rather than inventing values.

func kernelRelease() string { return "unknown (checkpoint only captures writes on Linux)" }

func filesystemName(string) string { return "unknown (not probed off Linux)" }
