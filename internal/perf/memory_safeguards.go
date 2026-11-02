package perf

import (
	"runtime/debug"
	"runtime/metrics"
)

const GoMemoryReleaseThreshold = 64 << 20

type GoMemory interface {
	HeapMemory() (idle uint64, released uint64)
	FreeOSMemory()
}

type RuntimeGoMemory struct{}

func (RuntimeGoMemory) HeapMemory() (idle uint64, released uint64) {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/idle:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		idle = samples[0].Value.Uint64()
	}
	if samples[1].Value.Kind() == metrics.KindUint64 {
		released = samples[1].Value.Uint64()
	}
	return idle, released
}

func (RuntimeGoMemory) FreeOSMemory() { debug.FreeOSMemory() }

// maybeReleaseGoMemory returns idle heap pages to macOS only when Tart Oven is
// retaining enough unreleased memory to justify a synchronous GC/scavenge.
func MaybeReleaseGoMemory(memory GoMemory) (uint64, bool) {
	idle, released := memory.HeapMemory()
	if released >= idle {
		return 0, false
	}
	releasable := idle - released
	if releasable < GoMemoryReleaseThreshold {
		return releasable, false
	}
	memory.FreeOSMemory()
	return releasable, true
}

func DeferVMStartForPressure(sample PerformanceSample) bool {
	return sample.PressureAvailable && sample.MemoryPressure == "critical"
}

func DeferVMStartForHistory(history []PerformanceSample) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].PressureAvailable {
			return DeferVMStartForPressure(history[i])
		}
	}
	return false
}
