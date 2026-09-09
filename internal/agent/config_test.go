package agent

import (
	"github.com/MOVEI144/RunnerLoom/internal/core"
	"testing"
)

func TestPrivateStateAndGuestDiskDirectoriesNeverOverlap(t *testing.T) {
	for _, dirs := range [][2]string{{"/var/lib/rl", "/var/lib/rl"}, {"/var/lib/rl", "/var/lib/rl/vms"}, {"/var/lib/rl/private", "/var/lib/rl"}} {
		c := Config{Node: "node-a", Cluster: "home", Controller: "https://127.0.0.1:8443", StateDir: dirs[0], DiskDir: dirs[1], Ceiling: core.Resources{CPU: 4, Memory: 8192, Disk: 100}, CacheGiB: 5}
		if c.Validate() == nil {
			t.Fatal("overlap accepted", dirs)
		}
	}
}
