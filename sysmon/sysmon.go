// Package sysmon samples per-process CPU and memory usage using pure
// Windows syscalls (kernel32.dll only — no external dependencies).
package sysmon

import (
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procGetProcessTimes            = kernel32.NewProc("GetProcessTimes")
	procK32EnumProcesses           = kernel32.NewProc("K32EnumProcesses")
	procK32GetProcessImageFileName = kernel32.NewProc("K32GetProcessImageFileNameW")
	procK32GetProcessMemoryInfo    = kernel32.NewProc("K32GetProcessMemoryInfo")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
)

const (
	processQueryLimitedInfo = 0x1000
	processVMRead           = 0x0010
)

// fileTime represents a Windows FILETIME (100-nanosecond intervals since 1601-01-01).
type fileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (ft fileTime) ticks() uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// processMemCounters mirrors PROCESS_MEMORY_COUNTERS.
type processMemCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// ProcessStat holds resource usage for one process.
type ProcessStat struct {
	PID    uint32  `json:"pid"`
	Name   string  `json:"name"`
	CPUPct float64 `json:"cpu_pct"` // 0–100% of total system capacity
	MemMB  float64 `json:"mem_mb"`
}

type cpuSample struct {
	ticks uint64    // CPU ticks (100ns units) at last sample
	at    time.Time // wall time of last sample
}

// Sampler accumulates CPU time deltas between consecutive polls.
// It is safe for concurrent use.
type Sampler struct {
	mu   sync.Mutex
	prev map[uint32]cpuSample
}

// DefaultSampler is the package-level sampler used by TopProcesses.
var DefaultSampler = &Sampler{prev: make(map[uint32]cpuSample)}

// TopProcesses returns the top n processes by CPU usage, sorted descending.
// The first call returns CPU values of 0 (establishes baseline); subsequent
// calls reflect actual usage since the previous call.
func TopProcesses(n int) []ProcessStat {
	return DefaultSampler.Top(n)
}

// Top samples all processes and returns the top n by CPU%.
func (s *Sampler) Top(n int) []ProcessStat {
	s.mu.Lock()
	defer s.mu.Unlock()

	pids := enumPIDs()
	now := time.Now()
	ncpu := float64(runtime.NumCPU())

	next := make(map[uint32]cpuSample, len(pids))
	var stats []ProcessStat

	for _, pid := range pids {
		if pid == 0 {
			continue
		}

		// Try with limited info first; fall back to VM_READ for memory.
		h, _, _ := procOpenProcess.Call(
			processQueryLimitedInfo|processVMRead, 0, uintptr(pid))
		if h == 0 {
			h, _, _ = procOpenProcess.Call(
				processQueryLimitedInfo, 0, uintptr(pid))
		}
		if h == 0 {
			continue
		}

		// CPU times
		var creation, exit, kernelFT, userFT fileTime
		procGetProcessTimes.Call(h,
			uintptr(unsafe.Pointer(&creation)),
			uintptr(unsafe.Pointer(&exit)),
			uintptr(unsafe.Pointer(&kernelFT)),
			uintptr(unsafe.Pointer(&userFT)),
		)
		ticks := kernelFT.ticks() + userFT.ticks()

		// Memory
		var mem processMemCounters
		mem.CB = uint32(unsafe.Sizeof(mem))
		procK32GetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&mem)), unsafe.Sizeof(mem))

		// Image name
		var nameBuf [512]uint16
		r, _, _ := procK32GetProcessImageFileName.Call(
			h, uintptr(unsafe.Pointer(&nameBuf[0])), 512)

		procCloseHandle.Call(h)

		name := "unknown"
		if r > 0 {
			full := syscall.UTF16ToString(nameBuf[:r])
			name = baseName(full)
		}

		next[pid] = cpuSample{ticks, now}

		// Calculate CPU% using delta from previous sample.
		cpuPct := 0.0
		if prev, ok := s.prev[pid]; ok && now.After(prev.at) {
			wallTicks := float64(now.Sub(prev.at).Nanoseconds()) / 100 // convert to 100ns
			cpuDelta := float64(ticks - prev.ticks)
			if wallTicks > 0 && cpuDelta >= 0 {
				// Normalise to total system capacity (0–100%).
				cpuPct = cpuDelta / (wallTicks * ncpu) * 100.0
				if cpuPct > 100.0 {
					cpuPct = 100.0
				}
			}
		}

		memMB := float64(mem.WorkingSetSize) / 1024 / 1024
		if cpuPct > 0 || memMB >= 1 {
			stats = append(stats, ProcessStat{
				PID:    pid,
				Name:   name,
				CPUPct: cpuPct,
				MemMB:  memMB,
			})
		}
	}

	s.prev = next

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].CPUPct > stats[j].CPUPct
	})
	if len(stats) > n {
		stats = stats[:n]
	}
	return stats
}

// enumPIDs returns the list of all active process IDs.
func enumPIDs() []uint32 {
	buf := make([]uint32, 1024)
	for {
		var needed uint32
		procK32EnumProcesses.Call(
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(uint32(len(buf)*4)),
			uintptr(unsafe.Pointer(&needed)),
		)
		count := needed / 4
		if int(count) < len(buf) {
			return buf[:count]
		}
		buf = make([]uint32, len(buf)*2)
	}
}

// baseName extracts the filename from a Windows NT device path or a normal path.
// E.g. "\Device\HarddiskVolume3\Windows\explorer.exe" → "explorer.exe"
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '\\' || path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
