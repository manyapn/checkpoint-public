// Package daemon (checkpointd) is what commits the session history, so that
// nobody has to remember to: it owns the long-lived fanotify capture for a
// watched root, keeps recording the content of deleted files, and, on an
// explicit boundary request over a Unix socket, settles briefly and cuts a
// boundary-scan manifest, which is this system's commit.
//
// The boundary protocol is deliberately SOURCE-AGNOSTIC: a wrapped command's
// completion (`checkpoint run`), an agent-turn hook, a manual save, and the
// daemon's own autosave cadence all cut through the same path rather than
// inventing separate checkpoint semantics. Retention/GC is NOT here: it is
// store.Prune, run offline by the CLI with the daemon stopped.
//
// This file (protocol + client) is cross-platform; the server is Linux-only
// (fanotify). See daemon_linux.go / daemon_other.go.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

// SocketPath is the daemon's Unix socket for a given store dir.
func SocketPath(storeDir string) string { return filepath.Join(storeDir, "daemon.sock") }

// Request is a daemon request.
//   - "checkpoint": settle + cut a checkpoint (Source labels it; Name, when set,
//     names the manifest AND forces a cut, since a named request is never
//     skipped as empty).
//   - "register" / "unregister": add/remove an agent root (AgentPid+AgentStart, a
//     stable identity) so writes descended from it attribute to the agent.
type Request struct {
	Op         string `json:"op"`
	Source     string `json:"source,omitempty"`
	Name       string `json:"name,omitempty"`
	AgentPid   int    `json:"agent_pid,omitempty"`
	AgentStart uint64 `json:"agent_start,omitempty"`
}

// Status is the daemon's live protection status for a watched root (the "status"
// op). Protected is always true from a responding daemon; a caller that cannot
// connect reports "Not protected" itself.
type Status struct {
	Protected     bool     `json:"protected"`
	Root          string   `json:"root"`
	ProtectedDirs []string `json:"protected_dirs"` // workspace first, then --protect extras; ALWAYS an array (client contract)
	Checkpoints   int      `json:"checkpoints"`
	Limited       bool     `json:"limited"`    // the current (unsaved) window has detected loss
	Missed        int      `json:"missed"`     // bounded per-file misses this window
	Overflowed    bool     `json:"overflowed"` // unbounded loss (queue overflow) this window
	AgentSessions int      `json:"agent_sessions"`
	SinceUnixNS   int64    `json:"since_unix_ns"`
	SettingUp     bool     `json:"setting_up"`              // first scan of a fresh store still running
	SetupScanned  int64    `json:"setup_scanned,omitempty"` // files scanned so far while SettingUp
	LastCkptNS    int64    `json:"last_ckpt_ns"`            // TimeNS of the last COMPLETE checkpoint (0 = none yet)
	FeedActive    bool     `json:"feed_active"`             // dirent change-feed armed (delete provenance + O(changes) boundaries)
	// BaselineComplete: one DURABLE (overflow-free window) baseline checkpoint
	// exists. False = the first scan was holed; the daemon is degraded and
	// auto-rescans; "daemon running" must never read as "protected" without it.
	BaselineComplete bool `json:"baseline_complete"`
	// Unprotected changes: AGENT-tree writes that landed outside
	// every protected folder: uncaptured, surfaced never hidden. Outside is a
	// bounded distinct-path list; OutsideCount counts every such write.
	Outside      []string `json:"outside"`
	OutsideCount int      `json:"outside_count"`
}

// Response reports the outcome of a boundary request.
type Response struct {
	ID             int    `json:"id"`               // manifest id created (or the existing latest on skip-empty)
	Coverage       string `json:"coverage"`         // DURABLE | PARTIAL
	Entries        int    `json:"entries"`          // paths in the manifest
	SettleTimedOut bool   `json:"settle_timed_out"` // the 250ms settle hit its 2s ceiling
	// SkippedEmpty: the window was PROVABLY empty (feed active, no overflow,
	// zero captured versions, zero dirty paths, zero misses), so no new manifest
	// was cut, so ID/Coverage/Entries describe the existing latest. Never set in
	// scan mode (deletes are invisible there; emptiness cannot be proven) and
	// never for a named request.
	SkippedEmpty bool   `json:"skipped_empty,omitempty"`
	Error        string `json:"error,omitempty"`
}

// RequestCheckpoint connects to the daemon at sockPath, asks it to cut a
// checkpoint labelled source, and returns its response.
func RequestCheckpoint(sockPath, source string) (Response, error) {
	return RequestNamedCheckpoint(sockPath, source, "")
}

// RequestNamedCheckpoint is RequestCheckpoint with a user-visible name recorded
// on the manifest; a named checkpoint survives pruning and is never skipped as
// empty. Cross-platform: the client is just a socket round-trip (the fanotify
// work is all daemon-side).
func RequestNamedCheckpoint(sockPath, source, name string) (Response, error) {
	c, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return Response{}, fmt.Errorf("daemon: connect %s: %w", sockPath, err)
	}
	defer c.Close()
	// The settle can take up to the hard ceiling; allow generous headroom.
	c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(c).Encode(Request{Op: "checkpoint", Source: source, Name: name}); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return Response{}, err
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("daemon: %s", resp.Error)
	}
	return resp, nil
}

// RegisterAgentRoot tells the daemon that (pid, start) is an agent root: writes
// whose process lineage terminates at it attribute to the agent.
func RegisterAgentRoot(sockPath string, pid int, start uint64) error {
	return simpleReq(sockPath, Request{Op: "register", AgentPid: pid, AgentStart: start})
}

// UnregisterAgentRoot removes an agent root after its session ends.
func UnregisterAgentRoot(sockPath string, pid int, start uint64) error {
	return simpleReq(sockPath, Request{Op: "unregister", AgentPid: pid, AgentStart: start})
}

// RequestStatus asks the daemon for its live protection status.
func RequestStatus(sockPath string) (Status, error) {
	c, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return Status{}, fmt.Errorf("daemon: connect %s: %w", sockPath, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(Request{Op: "status"}); err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.NewDecoder(c).Decode(&st); err != nil {
		return Status{}, err
	}
	return st, nil
}

func simpleReq(sockPath string, req Request) error {
	c, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("daemon: connect %s: %w", sockPath, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("daemon: %s", resp.Error)
	}
	return nil
}
