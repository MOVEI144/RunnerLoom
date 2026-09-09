package host

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type MemoryInfo struct {
	TotalMiB     int64
	AvailableMiB int64
}

func parseMemory(data string) (MemoryInfo, error) {
	var out MemoryInfo
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		if f[0] != "MemTotal:" && f[0] != "MemAvailable:" {
			continue
		}
		v, e := strconv.ParseInt(f[1], 10, 64)
		if e != nil || v < 0 || v > 1<<50 {
			return out, errors.New("invalid host memory information")
		}
		switch f[0] {
		case "MemTotal:":
			out.TotalMiB = v / 1024
		case "MemAvailable:":
			out.AvailableMiB = v / 1024
		}
	}
	if out.TotalMiB < 512 || out.AvailableMiB > out.TotalMiB {
		return out, errors.New("cannot establish host memory capacity")
	}
	return out, nil
}
func hostMemory() (MemoryInfo, error) {
	b, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return MemoryInfo{}, e
	}
	return parseMemory(string(b))
}
func validateCeiling(ceiling core.Resources, cpus int, memory MemoryInfo) error {
	if !ceiling.Valid() {
		return errors.New("invalid local resource ceiling")
	}
	if ceiling.CPU > int64(cpus) {
		return fmt.Errorf("local vCPU ceiling %d exceeds the %d logical CPUs detected; CPU overcommit is not enabled", ceiling.CPU, cpus)
	}
	if ceiling.Memory > memory.TotalMiB-512 {
		return fmt.Errorf("local RAM ceiling %d MiB leaves less than the minimum 512 MiB host reserve from %d MiB; reduce the ceiling", ceiling.Memory, memory.TotalMiB)
	}
	return nil
}
func (l *Libvirt) checkPhysicalCapacity() error {
	m, e := hostMemory()
	if e != nil {
		return e
	}
	return validateCeiling(l.Ceiling, runtime.NumCPU(), m)
}
