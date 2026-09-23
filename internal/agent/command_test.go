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

func TestAcceptCommandTaskMatrix(t *testing.T) {
	ceiling := core.Resources{CPU: 8, Memory: 16384, Disk: 100}
	gh := core.Pool{Name: "linux-lite", VCPU: 2, MemoryMiB: 2048, OverheadMiB: 512, RootGiB: 20, DiskOverheadGiB: 2}
	tp := gh
	tp.Tasks = true
	id := core.ID()
	payload := &core.TaskPayload{ID: id}
	cases := []struct {
		name string
		cmd  core.Command
		ok   bool
	}{
		{"runner ensure", core.Command{Action: "ensure", JIT: "j", Instance: core.Instance{Node: "node-a", Pool: gh}}, true},
		{"task ensure", core.Command{Action: "ensure", Task: payload, Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, true},
		{"task stop", core.Command{Action: "stop", Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, true},
		{"task payload on stop", core.Command{Action: "stop", Task: payload, Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, false},
		{"payload for another task", core.Command{Action: "ensure", Task: &core.TaskPayload{ID: core.ID()}, Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, false},
		{"payload on a runner instance", core.Command{Action: "ensure", Task: &core.TaskPayload{}, Instance: core.Instance{Node: "node-a", Pool: gh}}, false},
		{"task instance without payload", core.Command{Action: "ensure", Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, false},
		{"task instance with JIT", core.Command{Action: "ensure", JIT: "j", Task: payload, Instance: core.Instance{Node: "node-a", Pool: tp, Task: id}}, false},
		{"JIT into a task pool", core.Command{Action: "ensure", JIT: "j", Instance: core.Instance{Node: "node-a", Pool: tp}}, false},
		{"task link on a GitHub pool", core.Command{Action: "ensure", Task: payload, Instance: core.Instance{Node: "node-a", Pool: gh, Task: id}}, false},
	}
	for _, c := range cases {
		if got := acceptCommand("node-a", ceiling, c.cmd) == nil; got != c.ok {
			t.Errorf("%s: accepted=%v want %v", c.name, got, c.ok)
		}
	}
}
