//go:build !linux

package daemon

import "errors"

// ErrUnsupported is returned by Serve off Linux (the daemon needs fanotify).
var ErrUnsupported = errors.New("daemon: fanotify capture requires linux")

// Config mirrors the Linux build's type so callers compile on any OS.
type Config struct {
	Workspace string
	StoreDir  string
}

// Serve is unavailable off Linux. The cross-platform client (RequestCheckpoint,
// in protocol.go) still works; it just talks to a daemon running on a Linux box.
func Serve(cfg Config, ready chan<- struct{}, stop <-chan struct{}) error {
	return ErrUnsupported
}
