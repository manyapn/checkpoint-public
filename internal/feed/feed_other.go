//go:build !linux

// Non-Linux stub so the tree still builds on a non-Linux dev host; the feed is
// fanotify-only (see feed_linux.go).
package feed

import "errors"

var ErrUnsupported = errors.New("feed: unsupported on this platform")

type Op string

const (
	OpCreate     Op = "create"
	OpWrite      Op = "write"
	OpDelete     Op = "delete"
	OpRenameFrom Op = "rename-from"
	OpRenameTo   Op = "rename-to"
)

type Event struct {
	Op   Op
	Path string
	Pid  int32
	Dir  bool
}

type Feed struct{}

func New(roots []string) (*Feed, error) { return nil, ErrUnsupported }
func (f *Feed) Drain() []Event          { return nil }
func (f *Feed) Overflowed() bool        { return false }
func (f *Feed) ResetWindow()            {}
func (f *Feed) Fd() int                 { return -1 }
func (f *Feed) Close() error            { return nil }
