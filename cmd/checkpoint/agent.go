package main

import (
	"fmt"

	"github.com/manyapn/checkpoint-public/internal/daemon"
	"github.com/manyapn/checkpoint-public/internal/lineage"
)

// register-agent lets an agent that was not started by `checkpoint run`
// (one driven by a hook, say) claim its process tree.
func cmdRegisterAgent(args []string, register bool) error {
	fs, rootFlag, storeFlag := flags("register-agent")
	pid := fs.Int("pid", 0, "pid of the agent process")
	fs.Parse(args)
	t, err := resolve(*rootFlag, *storeFlag)
	if err != nil {
		return err
	}
	start, ok := lineage.StartTime(*pid)
	if !ok {
		return fmt.Errorf("no live process with pid %d", *pid)
	}
	id := lineage.Identity{Pid: *pid, Start: start}
	if register {
		return daemon.Register(t.sock, id)
	}
	return daemon.Unregister(t.sock, id)
}
