package api

import (
	"bufio"
	"context"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/events"
)

const resourcePollInterval = 5 * time.Second

// PollResourceUsage polls the runc container's cgroup every 5 seconds while at
// least one SSE client is connected and emits a resource.usage event only when
// cpu_percent or memory_bytes has changed since the last emission.
//
// cgroupPath is the absolute cgroup path written into the runc config
// (e.g. "/sandbox-<hostname>"); the cgroup v2 files are read from
// /sys/fs/cgroup<cgroupPath>/.
func PollResourceUsage(ctx context.Context, broker *events.Broker, cgroupPath string) {
	if cgroupPath == "" {
		return
	}
	cgroupDir := filepath.Join("/sys/fs/cgroup", cgroupPath)

	type sample struct {
		usageUsec int64
		at        time.Time
	}

	var prev sample
	var lastCPU float32 = -1
	var lastMem int = -1

	ticker := time.NewTicker(resourcePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		usageUsec, err := readCPUUsageUsec(cgroupDir)
		if err != nil {
			log.Printf("resource_poller: cpu.stat: %v", err)
			continue
		}
		now := time.Now()

		memBytes, err := readMemoryWorkingSet(cgroupDir)
		if err != nil {
			log.Printf("resource_poller: memory.current: %v", err)
			continue
		}

		firstSample := prev.at.IsZero()
		var cpuPercent float32
		if !firstSample {
			wallUs := now.Sub(prev.at).Microseconds()
			if wallUs > 0 {
				cpuPercent = float32(math.Round(float64(usageUsec-prev.usageUsec)/float64(wallUs)*1000) / 10)
			}
		}
		prev = sample{usageUsec: usageUsec, at: now}

		if firstSample {
			// Establish baseline; delta not yet computable.
			continue
		}
		if roundCPU(cpuPercent) == roundCPU(lastCPU) && memBytes == lastMem {
			continue
		}
		lastCPU = cpuPercent
		lastMem = memBytes

		broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
			var ev gen.SandboxEvent
			_ = ev.FromResourceUsageEvent(gen.ResourceUsageEvent{
				Type:        "resource.usage",
				Id:          int(id),
				Timestamp:   ts,
				CpuPercent:  cpuPercent,
				MemoryBytes: memBytes,
			})
			return ev
		})
	}
}

// readCPUUsageUsec parses the usage_usec field from cpu.stat.
func readCPUUsageUsec(cgroupDir string) (int64, error) {
	f, err := os.Open(filepath.Join(cgroupDir, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, sc.Err()
}

// readMemoryWorkingSet returns the working-set memory for the cgroup:
// memory.current minus inactive_file from memory.stat (reclaimable page cache).
// This matches how Docker/containerd compute "MEM USAGE".
func readMemoryWorkingSet(cgroupDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "memory.current"))
	if err != nil {
		return 0, err
	}
	total, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	inactiveFile, err := readStatField(cgroupDir, "inactive_file")
	if err != nil {
		return 0, err
	}
	if inactiveFile > total {
		return 0, nil
	}
	return total - inactiveFile, nil
}

func readStatField(cgroupDir, field string) (int, error) {
	f, err := os.Open(filepath.Join(cgroupDir, "memory.stat"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	prefix := field + " "
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), prefix); ok {
			return strconv.Atoi(v)
		}
	}
	return 0, sc.Err()
}

// roundCPU rounds to 1 decimal place for change detection.
func roundCPU(v float32) float32 {
	return float32(math.Round(float64(v)*10) / 10)
}
