// Package sysinfo reads the live figures of the machine: GPUs (nvidia-smi),
// RAM (/proc/meminfo), the disk of the data directory and the CPU.
package sysinfo

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// GPU is one graphics card.
type GPU struct {
	Name   string  `json:"name"`
	Total  int64   `json:"total"`
	Used   int64   `json:"used"`
	Free   int64   `json:"free"`
	Util   int     `json:"util"`
	TempC  int     `json:"tempC"`
	PowerW float64 `json:"powerW"`
}

// RAM is the system memory.
type RAM struct {
	Total      int64 `json:"total"`
	Used       int64 `json:"used"` // not available to new allocations (total - available)
	Available  int64 `json:"available"`
	Cached     int64 `json:"cached"`
	EngineRSS  int64 `json:"engineRss"` // resident memory of the engine process
	EngineAnon int64 `json:"engineAnon"`
	EngineFile int64 `json:"engineFile"` // of it: mapped model file pages
	SwapUsed   int64 `json:"swapUsed"`
}

// Disk is the file system of the data directory.
type Disk struct {
	Path  string `json:"path"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
}

// CPU is the processor.
type CPU struct {
	Name    string  `json:"name"`
	Threads int     `json:"threads"`
	Load    float64 `json:"load"` // 0..1, over the last interval
}

// Snapshot is the machine at one moment.
type Snapshot struct {
	GPUs []GPU `json:"gpus"`
	RAM  RAM   `json:"ram"`
	Disk Disk  `json:"disk"`
	CPU  CPU   `json:"cpu"`
}

// Reader keeps what it needs between two reads (CPU times).
type Reader struct {
	DataDir string

	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	cpuName   string
	noSMI     bool
}

// Read takes a snapshot; pid is the engine process (0 if none).
func (r *Reader) Read(pid int) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{GPUs: []GPU{}}
	if !r.noSMI {
		gpus, err := readGPUs()
		if err != nil {
			if _, lookErr := exec.LookPath("nvidia-smi"); lookErr != nil {
				r.noSMI = true
			}
		}
		s.GPUs = gpus
	}
	s.RAM = readRAM()
	if pid > 0 {
		s.RAM.EngineRSS, s.RAM.EngineAnon, s.RAM.EngineFile = readRSS(pid)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.DataDir, &st); err == nil {
		s.Disk = Disk{Path: r.DataDir, Total: int64(st.Blocks) * int64(st.Bsize), Free: int64(st.Bavail) * int64(st.Bsize)}
	}
	if r.cpuName == "" {
		r.cpuName = cpuName()
	}
	s.CPU = CPU{Name: r.cpuName, Threads: cpuThreads(), Load: r.cpuLoad()}
	return s
}

// HasGPU reports whether nvidia-smi answered.
func (r *Reader) HasGPU() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.noSMI
}

func readGPUs() ([]GPU, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return []GPU{}, err
	}
	gpus := []GPU{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 7 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		mib := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n << 20 }
		num := func(s string) int { n, _ := strconv.Atoi(s); return n }
		pw, _ := strconv.ParseFloat(f[6], 64)
		gpus = append(gpus, GPU{Name: f[0], Total: mib(f[1]), Used: mib(f[2]), Free: mib(f[3]), Util: num(f[4]), TempC: num(f[5]), PowerW: pw})
	}
	return gpus, nil
}

func readRAM() RAM {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return RAM{}
	}
	defer f.Close()
	v := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 {
			n, _ := strconv.ParseInt(parts[1], 10, 64)
			v[strings.TrimSuffix(parts[0], ":")] = n << 10
		}
	}
	r := RAM{Total: v["MemTotal"], Available: v["MemAvailable"], Cached: v["Cached"] + v["Buffers"]}
	r.Used = r.Total - r.Available
	r.SwapUsed = v["SwapTotal"] - v["SwapFree"]
	return r
}

func readRSS(pid int) (rss, anon, file int64) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(parts[1], 10, 64)
		switch parts[0] {
		case "VmRSS:":
			rss = n << 10
		case "RssAnon:":
			anon = n << 10
		case "RssFile:":
			file = n << 10
		}
	}
	return
}

func cpuName() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cpuThreads() int {
	n := 0
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	return n
}

func (r *Reader) cpuLoad() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 {
		return 0
	}
	var total, idle uint64
	for i, s := range f[1:] {
		n, _ := strconv.ParseUint(s, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	dt, di := total-r.prevTotal, idle-r.prevIdle
	r.prevTotal, r.prevIdle = total, idle
	if dt == 0 || r.prevTotal == total-dt && dt == total {
		return 0
	}
	return 1 - float64(di)/float64(dt)
}
