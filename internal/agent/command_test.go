package agent

import (
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestAcceptCommandAllowsTeardownBeyondCeiling(t *testing.T) {
	ceiling := core.Resources{CPU: 2, Memory: 2048, Disk: 10}
	pool := core.Pool{Name: "linux-lite", VCPU: 4, MemoryMiB: 4096, OverheadMiB: 512, RootGiB: 20, ScratchGiB: 0, DiskOverheadGiB: 2}
	cmd := core.Command{Action: "stop", Instance: core.Instance{Node: "node-a", Pool: pool}}
	if e := acceptCommand("node-a", ceiling, cmd); e != nil {
		t.Fatal(e)
	}
	cmd.Action = "delete"
	if e := acceptCommand("node-a", ceiling, cmd); e != nil {
		t.Fatal(e)
	}
	cmd.Action = "ensure"
	if e := acceptCommand("node-a", ceiling, cmd); e == nil {
		t.Fatal("ensure above ceiling accepted")
	}
}

func TestAcceptCommandRejectsOtherNode(t *testing.T) {
	cmd := core.Command{Action: "delete", Instance: core.Instance{Node: "node-b"}}
	if e := acceptCommand("node-a", core.Resources{CPU: 4, Memory: 8192, Disk: 80}, cmd); e == nil {
		t.Fatal("foreign node command accepted")
	}
}
