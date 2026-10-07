// Package daemon runs standing protection for one folder: it records every
// write as it happens and cuts a checkpoint when asked (a turn ends, `save`
// is run) or every few minutes during an agent session. The CLI talks to it
// over a Unix socket in the store.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/manyapn/checkpoint-public/internal/lineage"
)

func SocketPath(storeDir string) string { return filepath.Join(storeDir, "daemon.sock") }

type Request struct {
	Op     string           `json:"op"` // checkpoint | register | unregister | status
	Source string           `json:"source,omitempty"`
	Name   string           `json:"name,omitempty"`
	Agent  lineage.Identity `json:"agent,omitempty"`
}

type Result struct {
	ID             int    `json:"id"`
	Badge          string `json:"badge"`
	Entries        int    `json:"entries"`
	SettleTimedOut bool   `json:"settle_timed_out"`
	Skipped        bool   `json:"skipped"` // nothing changed, no checkpoint cut
	Error          string `json:"error,omitempty"`
}

type Status struct {
	Protected        bool     `json:"protected"`
	Root             string   `json:"root"`
	Checkpoints      int      `json:"checkpoints"`
	LastCheckpointNS int64    `json:"last_checkpoint_ns"`
	SinceNS          int64    `json:"since_ns"`
	AgentSessions    int      `json:"agent_sessions"`
	FeedActive       bool     `json:"feed_active"`
	Missed           []string `json:"missed"`
	Overflowed       bool     `json:"overflowed"`
	Outside          []string `json:"outside"`
	OutsideCount     int      `json:"outside_count"`
}

func (s Status) Limited() bool {
	return len(s.Missed) > 0 || s.Overflowed || s.OutsideCount > 0
}

func Checkpoint(sock, source, name string) (Result, error) {
	var res Result
	err := call(sock, Request{Op: "checkpoint", Source: source, Name: name}, &res, 30*time.Second)
	if err == nil && res.Error != "" {
		err = fmt.Errorf("daemon: %s", res.Error)
	}
	return res, err
}

func Register(sock string, id lineage.Identity) error {
	return call(sock, Request{Op: "register", Agent: id}, &Result{}, 5*time.Second)
}

func Unregister(sock string, id lineage.Identity) error {
	return call(sock, Request{Op: "unregister", Agent: id}, &Result{}, 5*time.Second)
}

func GetStatus(sock string) (Status, error) {
	var st Status
	err := call(sock, Request{Op: "status"}, &st, 5*time.Second)
	return st, err
}

// Running reports whether a daemon answers on sock.
func Running(sock string) bool {
	_, err := GetStatus(sock)
	return err == nil
}

func call(sock string, req Request, out any, timeout time.Duration) error {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return fmt.Errorf("no daemon at %s: %w", sock, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	return json.NewDecoder(c).Decode(out)
}
