//go:build !linux

// Non-Linux stub: fanotify capture is Linux-only (the product is Linux-only).
// This lets the module build and the CLI compile on a dev host (e.g. macOS); the
// `capture` command errors at runtime, while the cross-platform recover/inspect
// paths (store + version log + salvage) still work everywhere.
package capture

import (
	"errors"

	"github.com/manyapn/checkpoint-public/internal/objstore"
	"github.com/manyapn/checkpoint-public/internal/provenance"
	"github.com/manyapn/checkpoint-public/internal/versionlog"
)

// ErrUnsupported is returned by the capture entry points off Linux.
var ErrUnsupported = errors.New("capture: fanotify requires linux")

// Config mirrors the Linux build's type so callers compile on any OS.
type Config struct {
	Workspace string
	StoreDir  string
}

// Watcher is a stub; every method reports ErrUnsupported.
type Watcher struct{}

func New(cfg Config) (*Watcher, error)                         { return nil, ErrUnsupported }
func (w *Watcher) Run(stop <-chan struct{}) error              { return ErrUnsupported }
func (w *Watcher) Drain() int                                  { return 0 }
func (w *Watcher) Poll(timeoutMs int) bool                     { return false }
func (w *Watcher) Objects() *objstore.Store                    { return nil }
func (w *Watcher) Versions() []versionlog.Version              { return nil }
func (w *Watcher) Missed() int                                 { return 0 }
func (w *Watcher) MissedPaths() []string                       { return nil }
func (w *Watcher) OutsideWrites() ([]string, int)              { return nil, 0 }
func (w *Watcher) Overflowed() bool                            { return false }
func (w *Watcher) ResetWindow()                                {}
func (w *Watcher) AgentSessionCount() int                      { return 0 }
func (w *Watcher) RegisterAgentRoot(id provenance.Identity)    {}
func (w *Watcher) UnregisterAgentRoot(id provenance.Identity)  {}
func (w *Watcher) RecordDelete(path string, evPID int32) error { return ErrUnsupported }
func (w *Watcher) Fd() int                                     { return -1 }
func (w *Watcher) Sync() error                                 { return ErrUnsupported }
func (w *Watcher) Close() error                                { return nil }
