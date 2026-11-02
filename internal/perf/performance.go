package perf

import (
	"errors"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"golang.org/x/sys/unix"
)

const PerformanceHistoryLimit = 1440

// PerformanceSample contains the host performance measurements captured at a
// point in time. Availability fields indicate whether the corresponding
// platform metric was available when the sample was collected.
type PerformanceSample struct {
	Timestamp               time.Time `json:"timestamp"`
	MemoryUsedBytes         uint64    `json:"memoryUsedBytes"`
	MemoryTotalBytes        uint64    `json:"memoryTotalBytes"`
	SystemDiskUsedBytes     uint64    `json:"systemDiskUsedBytes"`
	SystemDiskTotalBytes    uint64    `json:"systemDiskTotalBytes"`
	VMDiskUsedBytes         uint64    `json:"vmDiskUsedBytes"`
	VMDiskTotalBytes        uint64    `json:"vmDiskTotalBytes"`
	UptimeSeconds           uint64    `json:"uptimeSeconds"`
	CPUPercent              float64   `json:"cpuPercent"`
	DiskReadBytesPerSecond  float64   `json:"diskReadBytesPerSecond"`
	DiskWriteBytesPerSecond float64   `json:"diskWriteBytesPerSecond"`
	MemoryPressure          string    `json:"memoryPressure"`
	CPUAvailable            bool      `json:"cpuAvailable"`
	MemoryAvailable         bool      `json:"memoryAvailable"`
	PressureAvailable       bool      `json:"pressureAvailable"`
	SystemDiskAvailable     bool      `json:"systemDiskAvailable"`
	VMDiskAvailable         bool      `json:"vmDiskAvailable"`
	DiskIOAvailable         bool      `json:"diskIOAvailable"`
	UptimeAvailable         bool      `json:"uptimeAvailable"`
}

type PerformanceSnapshot struct {
	Latest  PerformanceSample   `json:"latest"`
	History []PerformanceSample `json:"history"`
}

type Source interface {
	CPUPercent() (float64, error)
	VirtualMemory() (used, total uint64, err error)
	MemoryPressure() (int, error)
	DiskUsage(path string) (used, total uint64, err error)
	DiskCounters() (readBytes, writeBytes uint64, err error)
	Uptime() (uint64, error)
}

type Collector struct {
	mu                           sync.Mutex
	Source                       Source
	previousDiskReadBytes        uint64
	previousDiskWriteBytes       uint64
	previousDiskCountersRecorded time.Time
}

func (c *Collector) Collect(now time.Time, vmStoragePath string) PerformanceSample {
	c.mu.Lock()
	defer c.mu.Unlock()

	sample := PerformanceSample{Timestamp: now}

	if cpuPercent, err := c.Source.CPUPercent(); err == nil {
		sample.CPUPercent = ClampCPUPercent(cpuPercent)
		sample.CPUAvailable = true
	}
	if used, total, err := c.Source.VirtualMemory(); err == nil {
		sample.MemoryUsedBytes = used
		sample.MemoryTotalBytes = total
		sample.MemoryAvailable = true
	}
	if pressure, err := c.Source.MemoryPressure(); err == nil {
		sample.MemoryPressure = PressureName(pressure)
		sample.PressureAvailable = true
	}
	if used, total, err := c.Source.DiskUsage("/"); err == nil {
		sample.SystemDiskUsedBytes = used
		sample.SystemDiskTotalBytes = total
		sample.SystemDiskAvailable = true
	}
	if used, total, err := c.Source.DiskUsage(vmStoragePath); err == nil {
		sample.VMDiskUsedBytes = used
		sample.VMDiskTotalBytes = total
		sample.VMDiskAvailable = true
	}
	if reads, writes, err := c.Source.DiskCounters(); err == nil {
		sample.DiskIOAvailable = true
		if !c.previousDiskCountersRecorded.IsZero() {
			elapsed := now.Sub(c.previousDiskCountersRecorded).Seconds()
			if elapsed > 0 && reads >= c.previousDiskReadBytes && writes >= c.previousDiskWriteBytes {
				sample.DiskReadBytesPerSecond = float64(reads-c.previousDiskReadBytes) / elapsed
				sample.DiskWriteBytesPerSecond = float64(writes-c.previousDiskWriteBytes) / elapsed
			}
		}
		c.previousDiskReadBytes = reads
		c.previousDiskWriteBytes = writes
		c.previousDiskCountersRecorded = now
	}
	if uptime, err := c.Source.Uptime(); err == nil {
		sample.UptimeSeconds = uptime
		sample.UptimeAvailable = true
	}

	return sample
}

type SystemSource struct{}

func (SystemSource) CPUPercent() (float64, error) {
	percentages, err := cpu.Percent(0, false)
	if err != nil {
		return 0, err
	}
	if len(percentages) == 0 {
		return 0, errors.New("cpu usage unavailable")
	}
	return percentages[0], nil
}

func (SystemSource) VirtualMemory() (uint64, uint64, error) {
	stats, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	return stats.Used, stats.Total, nil
}

func (SystemSource) MemoryPressure() (int, error) {
	pressure, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	return int(pressure), err
}

func (SystemSource) DiskUsage(path string) (uint64, uint64, error) {
	usage, err := disk.Usage(path)
	if err != nil {
		return 0, 0, err
	}
	return usage.Used, usage.Total, nil
}

func (SystemSource) DiskCounters() (uint64, uint64, error) {
	counters, err := disk.IOCounters()
	if err != nil {
		return 0, 0, err
	}

	var reads, writes uint64
	for _, counter := range counters {
		reads += counter.ReadBytes
		writes += counter.WriteBytes
	}
	return reads, writes, nil
}

func (SystemSource) Uptime() (uint64, error) {
	return host.Uptime()
}

// PressureName maps kern.memorystatus_vm_pressure_level, which reports the
// dispatch memory-pressure constants: 1 normal, 2 warning, 4 critical.
func PressureName(level int) string {
	switch {
	case level <= 1:
		return "normal"
	case level == 2:
		return "warning"
	default:
		return "critical"
	}
}

func ClampCPUPercent(percent float64) float64 {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func AppendSample(history []PerformanceSample, sample PerformanceSample) []PerformanceSample {
	if len(history) < PerformanceHistoryLimit {
		return append(history, sample)
	}

	next := make([]PerformanceSample, PerformanceHistoryLimit)
	copy(next, history[1:])
	next[PerformanceHistoryLimit-1] = sample
	return next
}

// NewCollector creates a new performance collector with the system source.
func NewCollector() *Collector {
	return &Collector{Source: SystemSource{}}
}
