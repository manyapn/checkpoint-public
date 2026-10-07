//go:build !linux

package daemon

import "errors"

type Config struct {
	Root     string
	StoreDir string
}

func Serve(cfg Config, ready chan<- struct{}, stop <-chan struct{}) error {
	return errors.New("the daemon needs Linux (fanotify)")
}
