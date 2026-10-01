// Package metrics reads how loaded the VPS and its sandboxes are.
//
// It is sent up the channel only while Cloud holds a subscription — an
// unwatched dashboard costs the box nothing — and it is sized for a machine
// with one core and two gigabytes, which is why it reads /proc directly and
// shells out to docker rather than pulling in a metrics library.
//
// Host figures come from /proc and so exist on Linux, which is the only host
// the installer supports. Elsewhere they read as zero and only the disk, which
// statfs answers everywhere, is real.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roostlabs/protocol"
)

// Label is the container label a sandbox carries so its usage can be put
// against the task that is running it. It is sandbox.TaskLabel; repeated
// here so this package does not import the one that runs containers.
const Label = "roost.task"

// dockerTimeout bounds one docker call. Stats block for a sampling interval
// even with --no-stream, so this is generous.
const dockerTimeout = 10 * time.Second

// Sampler reads metrics. CPU usage is a rate, so a Sampler keeps the previous
// counters and the first reading after construction reports zero CPU.
type Sampler struct {
	// DataDir is the filesystem whose usage Disk reports: where the clones and
	// the journal live, which is what fills up.
	DataDir string

	mu       sync.Mutex
	lastBusy uint64
	lastAll  uint64
}

// Host reads the whole-machine figures.
func (s *Sampler) Host() protocol.HostMetrics {
	var m protocol.HostMetrics
	m.CPU = s.cpu()
	m.Mem = memPercent()
	m.Load = loadAverage()
	m.Disk = diskPercent(s.DataDir)
	return m
}

// cpu reports busy time as a share of all time since the previous call.
func (s *Sampler) cpu() float64 {
	busy, all, ok := readCPU()
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prevBusy, prevAll := s.lastBusy, s.lastAll
	s.lastBusy, s.lastAll = busy, all
	if prevAll == 0 || all <= prevAll {
		return 0
	}
	return clampPct(100 * float64(busy-prevBusy) / float64(all-prevAll))
}

// readCPU parses the aggregate line of /proc/stat into busy and total jiffies.
func readCPU() (busy, all uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		// user nice system idle iowait irq softirq steal ...
		var values []uint64
		for _, field := range fields[1:] {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			values = append(values, v)
		}
		for i, v := range values {
			all += v
			// idle (3) and iowait (4) are the time the CPU had nothing to do.
			if i != 3 && i != 4 {
				busy += v
			}
		}
		return busy, all, true
	}
	return 0, 0, false
}

// memPercent reads /proc/meminfo: used is total less available, which counts
// cache that could be reclaimed as free, the way free(1) does.
func memPercent() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	var total, available uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total = v
		case "MemAvailable":
			available = v
		}
	}
	if total == 0 || available > total {
		return 0
	}
	return clampPct(100 * float64(total-available) / float64(total))
}

// loadAverage reads the one-minute figure from /proc/loadavg.
func loadAverage() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// Sandboxes reads usage for every container carrying Label, keyed by the task
// id in the label. No daemon, or no containers, is an empty list rather than
// an error: it is the normal state of an idle box.
func Sandboxes(ctx context.Context) []protocol.SandboxMetrics {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--filter", "label="+Label,
		"--format", "{{.ID}}\t{{.Label \""+Label+"\"}}").Output()
	if err != nil {
		return nil
	}
	ids, tasks := parsePS(string(out))
	if len(ids) == 0 {
		return nil
	}

	args := append([]string{"stats", "--no-stream", "--format", "{{.ID}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, ids...)
	out, err = exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil
	}
	return parseStats(string(out), tasks)
}

// parsePS turns docker ps output into container ids and an id-to-task map.
func parsePS(out string) (ids []string, tasks map[string]string) {
	tasks = map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		id, task, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || id == "" || task == "" {
			continue
		}
		ids = append(ids, id)
		tasks[id] = task
	}
	return ids, tasks
}

// parseStats turns docker stats output into per-task figures. Stats prints a
// short id even when given a long one, so ids are matched by prefix.
func parseStats(out string, tasks map[string]string) []protocol.SandboxMetrics {
	var result []protocol.SandboxMetrics
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 {
			continue
		}
		task := taskFor(fields[0], tasks)
		if task == "" {
			continue
		}
		usage, _, _ := strings.Cut(fields[2], "/")
		result = append(result, protocol.SandboxMetrics{
			TaskID: task,
			CPUPct: parsePercent(fields[1]),
			MemMB:  parseBytesMB(strings.TrimSpace(usage)),
		})
	}
	return result
}

func taskFor(id string, tasks map[string]string) string {
	if task, ok := tasks[id]; ok {
		return task
	}
	for full, task := range tasks {
		if strings.HasPrefix(full, id) || strings.HasPrefix(id, full) {
			return task
		}
	}
	return ""
}

// parsePercent reads "12.34%".
func parsePercent(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseBytesMB reads docker's human sizes — "123.4MiB", "1.5GiB", "900kB" —
// into megabytes.
func parseBytesMB(s string) float64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mb     float64
	}{
		{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"GB", 1000}, {"MB", 1}, {"kB", 1.0 / 1000}, {"B", 1.0 / (1024 * 1024)},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0
			}
			return v * u.mb
		}
	}
	return 0
}

func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

// String renders a sample for a log line.
func String(m protocol.Metrics) string {
	return fmt.Sprintf("cpu %.0f%% mem %.0f%% disk %.0f%% load %.2f sandboxes %d",
		m.Host.CPU, m.Host.Mem, m.Host.Disk, m.Host.Load, len(m.Sandboxes))
}
