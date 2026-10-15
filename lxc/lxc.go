// Package lxc 采集纯 LXC 容器的资源维度指标。
//
// 数据源全部来自 cgroup v2 与 /proc，不依赖 LXD/Incus 的 API：
//
//	/sys/fs/cgroup/lxc.payload.<name>/   容器 cgroup 目录（存在即视为运行中）
//	  cpu.stat                          CPU 时间与限流
//	  memory.current / memory.max       内存占用与上限
//	  memory.stat                       内存明细（anon/file/kernel/slab/sock/shmem）
//	  memory.swap.current / .max        swap 占用与上限
//	  io.stat                           块设备读写字节/次数
//	  pids.current / pids.max           进程数
//	  cgroup.procs                      容器内进程（用于定位 netns）
//	/proc/<pid>/net/dev                 容器网卡收发计数
//
// 目录名必须形如 lxc.payload.<name>，lxc.monitor.* / lxc.pivot 等非容器条目会被排除。
package lxc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"lxc_exporter/util"
)

const namespace = "lxc"

// payloadPrefix 是 cgroup v2 下 LXC 容器目录的前缀。
const payloadPrefix = "lxc.payload."

// memoryStatKinds 是 memory.stat 中要导出的字段。
var memoryStatKinds = []string{"anon", "file", "kernel", "slab", "sock", "shmem"}

// IOCounters 是 io.stat 中单个块设备的累计计数。
type IOCounters struct {
	ReadBytes  float64
	WriteBytes float64
	ReadOps    float64
	WriteOps   float64
}

// NetCounters 是单个网卡的累计计数。
type NetCounters struct {
	ReceiveBytes    float64
	TransmitBytes   float64
	ReceivePackets  float64
	TransmitPackets float64
	ReceiveErrors   float64
	TransmitErrors  float64
	ReceiveDropped  float64
	TransmitDropped float64
}

// ContainerStats 保存一个容器的采集结果。
type ContainerStats struct {
	Name string

	CPUUserSeconds      float64
	CPUSystemSeconds    float64
	CPUThrottledSeconds float64
	CPUNrThrottled      float64

	MemoryBytes float64
	// MemoryLimit 为 nil 表示 memory.max 是 "max"（无上限）或缺失。
	MemoryLimit *float64
	MemoryStat  map[string]float64

	SwapBytes *float64
	SwapLimit *float64

	Pids      *float64
	PidsLimit *float64

	IO      map[string]IOCounters
	Network map[string]NetCounters
}

// ParseCPUStat 解析 cpu.stat，返回 user/system/throttled 秒数与限流次数。
func ParseCPUStat(content string) (user, system, throttled, nrThrottled float64) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "user_usec":
			user = util.ParseFloat64(fields[1]) / 1e6
		case "system_usec":
			system = util.ParseFloat64(fields[1]) / 1e6
		case "throttled_usec":
			throttled = util.ParseFloat64(fields[1]) / 1e6
		case "nr_throttled":
			nrThrottled = util.ParseFloat64(fields[1])
		}
	}
	return user, system, throttled, nrThrottled
}

// ParseMemoryStat 解析 memory.stat 的 "key value" 行。
func ParseMemoryStat(content string) map[string]float64 {
	out := make(map[string]float64)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out[fields[0]] = util.ParseFloat64(fields[1])
	}
	return out
}

// ParseIOStat 解析 io.stat，返回以 "maj:min" 为键的累计计数。
//
// 每行形如：
//
//	8:0 rbytes=123 wbytes=456 rios=1 wios=2 dbytes=0 dios=0
func ParseIOStat(content string) map[string]IOCounters {
	out := make(map[string]IOCounters)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		var c IOCounters
		for _, kv := range fields[1:] {
			key, value, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			v := util.ParseFloat64(value)
			switch key {
			case "rbytes":
				c.ReadBytes = v
			case "wbytes":
				c.WriteBytes = v
			case "rios":
				c.ReadOps = v
			case "wios":
				c.WriteOps = v
			}
		}
		out[dev] = c
	}
	return out
}

// ParseNetDev 解析 /proc/<pid>/net/dev 的内容，返回以接口名为键的计数。
//
// 格式与 /proc/net/dev 相同：
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes  packets errs drop fifo frame compressed multicast|bytes  packets errs drop fifo colls carrier compressed
//	    eth0: 900000 900 1 2 0 0 0 0 800000 800 0 1 0 0 0 0
//
// 字段顺序（接收 0-7，发送 8-15）：
//
//	0 bytes 1 packets 2 errs 3 drop ... 8 bytes 9 packets 10 errs 11 drop ...
func ParseNetDev(content string) map[string]NetCounters {
	out := make(map[string]NetCounters)
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		name, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if name == "" || len(fields) < 16 {
			continue
		}
		out[name] = NetCounters{
			ReceiveBytes:    util.ParseFloat64(fields[0]),
			ReceivePackets:  util.ParseFloat64(fields[1]),
			ReceiveErrors:   util.ParseFloat64(fields[2]),
			ReceiveDropped:  util.ParseFloat64(fields[3]),
			TransmitBytes:   util.ParseFloat64(fields[8]),
			TransmitPackets: util.ParseFloat64(fields[9]),
			TransmitErrors:  util.ParseFloat64(fields[10]),
			TransmitDropped: util.ParseFloat64(fields[11]),
		}
	}
	return out
}

// DiscoverContainers 列出 cgroup 根目录下所有 lxc.payload.<name> 目录的容器名。
//
// 只接受 lxc.payload. 前缀，因此 lxc.monitor.*、lxc.pivot 等条目天然被排除。
func DiscoverContainers(cgroupRoot string) ([]string, error) {
	entries, err := os.ReadDir(cgroupRoot)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, payloadPrefix) {
			continue
		}
		name := strings.TrimPrefix(n, payloadPrefix)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// readTrim 读取文件并去掉首尾空白。
func readTrim(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// readNumber 读取一个数值文件；值为 "max" 时返回 nil。
func readNumber(path string) (*float64, error) {
	s, err := readTrim(path)
	if err != nil {
		return nil, err
	}
	if s == "max" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return &v, nil
}

// Options 是 LXC Collector 的配置项。
type Options struct {
	// CgroupRoot 是 cgroup v2 根目录（默认 /sys/fs/cgroup）。
	CgroupRoot string
	// ProcRoot 是 procfs 根目录（默认 /proc），用于读取容器进程的 net/dev。
	ProcRoot string
	// Filter 用于筛选容器名（nil 表示全部）。
	Filter *util.Filter
	// Network 为 true 时采集容器网卡流量。
	Network bool
	// NetworkFilter 用于筛选网卡名（nil 表示全部）。
	// 容器里往往还跑着 docker，会带出 br-*/veth* 一堆虚拟网卡，
	// 默认按 DefaultNetworkExcludeRegex 排除，避免序列数爆掉。
	NetworkFilter *util.Filter
	// Timeout 预留给未来的读取超时控制。
	Timeout time.Duration
}

// Exporter 是 LXC 容器指标的 Prometheus Collector。
type Exporter struct {
	opts Options

	up             *prometheus.Desc
	cpuSeconds     *prometheus.Desc
	cpuThrottled   *prometheus.Desc
	cpuNrThrottled *prometheus.Desc

	memoryBytes       *prometheus.Desc
	memoryLimit       *prometheus.Desc
	memoryUsedPercent *prometheus.Desc
	memoryStat        *prometheus.Desc

	swapBytes *prometheus.Desc
	swapLimit *prometheus.Desc

	pids      *prometheus.Desc
	pidsLimit *prometheus.Desc

	ioReadBytes  *prometheus.Desc
	ioWriteBytes *prometheus.Desc
	ioReadOps    *prometheus.Desc
	ioWriteOps   *prometheus.Desc

	netRecvBytes    *prometheus.Desc
	netTransBytes   *prometheus.Desc
	netRecvPackets  *prometheus.Desc
	netTransPackets *prometheus.Desc
	netRecvErrors   *prometheus.Desc
	netTransErrors  *prometheus.Desc
	netRecvDropped  *prometheus.Desc
	netTransDropped *prometheus.Desc

	scrapeDuration *prometheus.Desc
	scrapeError    *prometheus.Desc
}

// New 使用给定配置创建 LXC Collector。
func New(opts Options) *Exporter {
	if opts.CgroupRoot == "" {
		opts.CgroupRoot = "/sys/fs/cgroup"
	}
	if opts.ProcRoot == "" {
		opts.ProcRoot = "/proc"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}

	name := []string{"name"}
	net := []string{"name", "device"}
	stat := []string{"name", "kind"}

	return &Exporter{
		opts: opts,

		up: prometheus.NewDesc(
			namespace+"_container_up", "LXC 容器是否运行中（1=是）", name, nil),
		cpuSeconds: prometheus.NewDesc(
			namespace+"_container_cpu_seconds_total",
			"LXC 容器累计使用的 CPU 时间（秒）", []string{"name", "mode"}, nil),
		cpuThrottled: prometheus.NewDesc(
			namespace+"_container_cpu_throttled_seconds_total",
			"LXC 容器被 CPU 限流的累计时间（秒）", name, nil),
		cpuNrThrottled: prometheus.NewDesc(
			namespace+"_container_cpu_nr_throttled_total",
			"LXC 容器被 CPU 限流的累计次数", name, nil),

		memoryBytes: prometheus.NewDesc(
			namespace+"_container_memory_bytes", "LXC 容器当前内存占用（字节）", name, nil),
		memoryLimit: prometheus.NewDesc(
			namespace+"_container_memory_limit_bytes",
			"LXC 容器内存上限（字节），memory.max 为 max 时不输出", name, nil),
		memoryUsedPercent: prometheus.NewDesc(
			namespace+"_container_memory_used_percent",
			"LXC 容器内存占用占上限的百分比，仅在有上限时输出", name, nil),
		memoryStat: prometheus.NewDesc(
			namespace+"_container_memory_stat_bytes",
			"LXC 容器 memory.stat 明细（anon/file/kernel/slab/sock/shmem）", stat, nil),

		swapBytes: prometheus.NewDesc(
			namespace+"_container_swap_bytes", "LXC 容器当前 swap 占用（字节）", name, nil),
		swapLimit: prometheus.NewDesc(
			namespace+"_container_swap_limit_bytes",
			"LXC 容器 swap 上限（字节），为 max 时不输出", name, nil),

		pids: prometheus.NewDesc(
			namespace+"_container_pids", "LXC 容器当前进程/线程数", name, nil),
		pidsLimit: prometheus.NewDesc(
			namespace+"_container_pids_limit",
			"LXC 容器进程数上限，为 max 时不输出", name, nil),

		ioReadBytes: prometheus.NewDesc(
			namespace+"_container_io_read_bytes_total", "LXC 容器累计读取字节数", net, nil),
		ioWriteBytes: prometheus.NewDesc(
			namespace+"_container_io_write_bytes_total", "LXC 容器累计写入字节数", net, nil),
		ioReadOps: prometheus.NewDesc(
			namespace+"_container_io_read_ops_total", "LXC 容器累计读操作数", net, nil),
		ioWriteOps: prometheus.NewDesc(
			namespace+"_container_io_write_ops_total", "LXC 容器累计写操作数", net, nil),

		netRecvBytes: prometheus.NewDesc(
			namespace+"_container_network_receive_bytes_total", "LXC 容器网卡累计接收字节数", net, nil),
		netTransBytes: prometheus.NewDesc(
			namespace+"_container_network_transmit_bytes_total", "LXC 容器网卡累计发送字节数", net, nil),
		netRecvPackets: prometheus.NewDesc(
			namespace+"_container_network_receive_packets_total", "LXC 容器网卡累计接收包数", net, nil),
		netTransPackets: prometheus.NewDesc(
			namespace+"_container_network_transmit_packets_total", "LXC 容器网卡累计发送包数", net, nil),
		netRecvErrors: prometheus.NewDesc(
			namespace+"_container_network_receive_errors_total", "LXC 容器网卡累计接收错误数", net, nil),
		netTransErrors: prometheus.NewDesc(
			namespace+"_container_network_transmit_errors_total", "LXC 容器网卡累计发送错误数", net, nil),
		netRecvDropped: prometheus.NewDesc(
			namespace+"_container_network_receive_dropped_total", "LXC 容器网卡累计接收丢包数", net, nil),
		netTransDropped: prometheus.NewDesc(
			namespace+"_container_network_transmit_dropped_total", "LXC 容器网卡累计发送丢包数", net, nil),

		scrapeDuration: prometheus.NewDesc(
			namespace+"_scrape_duration_seconds", "LXC 指标采集耗时（秒）", nil, nil),
		scrapeError: prometheus.NewDesc(
			namespace+"_scrape_error", "LXC 指标采集是否出错（1=出错, 0=正常）", nil, nil),
	}
}

// Describe 实现 prometheus.Collector。
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		e.up, e.cpuSeconds, e.cpuThrottled, e.cpuNrThrottled,
		e.memoryBytes, e.memoryLimit, e.memoryUsedPercent, e.memoryStat,
		e.swapBytes, e.swapLimit, e.pids, e.pidsLimit,
		e.ioReadBytes, e.ioWriteBytes, e.ioReadOps, e.ioWriteOps,
		e.netRecvBytes, e.netTransBytes, e.netRecvPackets, e.netTransPackets,
		e.netRecvErrors, e.netTransErrors, e.netRecvDropped, e.netTransDropped,
		e.scrapeDuration, e.scrapeError,
	} {
		ch <- d
	}
}

// Collect 实现 prometheus.Collector。
//
// 读取 cgroup 根目录失败会让 lxc_scrape_error 置为 1；
// 单个容器读文件失败（正在启停）只跳过该容器，不算错误。
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	names, err := DiscoverContainers(e.opts.CgroupRoot)
	errValue := 0.0
	if err != nil {
		errValue = 1.0
		log.Printf("读取 cgroup 根目录 %s 失败: %v", e.opts.CgroupRoot, err)
	}

	for _, name := range names {
		if !e.opts.Filter.Match(name) {
			continue
		}
		stats, err := e.readContainer(name)
		if err != nil {
			// 容器正在启停时文件可能读不到，跳过即可，不算采集错误。
			log.Printf("跳过容器 %s: %v", name, err)
			continue
		}
		e.emit(ch, stats)
	}

	ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, time.Since(start).Seconds())
	ch <- prometheus.MustNewConstMetric(e.scrapeError, prometheus.GaugeValue, errValue)
}

// readContainer 读取单个容器的 cgroup 数据。
//
// cpu.stat 是核心文件，读不到就认为容器正在启停并返回错误；
// 其余文件缺失时只跳过对应指标。
func (e *Exporter) readContainer(name string) (ContainerStats, error) {
	dir := filepath.Join(e.opts.CgroupRoot, payloadPrefix+name)
	stats := ContainerStats{Name: name}

	content, err := readTrim(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return stats, fmt.Errorf("读取 cpu.stat 失败: %w", err)
	}
	stats.CPUUserSeconds, stats.CPUSystemSeconds,
		stats.CPUThrottledSeconds, stats.CPUNrThrottled = ParseCPUStat(content)

	if v, err := readNumber(filepath.Join(dir, "memory.current")); err == nil && v != nil {
		stats.MemoryBytes = *v
	}
	if v, err := readNumber(filepath.Join(dir, "memory.max")); err == nil {
		stats.MemoryLimit = v
	}
	if content, err := readTrim(filepath.Join(dir, "memory.stat")); err == nil {
		all := ParseMemoryStat(content)
		stats.MemoryStat = make(map[string]float64)
		for _, kind := range memoryStatKinds {
			if v, ok := all[kind]; ok {
				stats.MemoryStat[kind] = v
			}
		}
	}
	if v, err := readNumber(filepath.Join(dir, "memory.swap.current")); err == nil {
		stats.SwapBytes = v
	}
	if v, err := readNumber(filepath.Join(dir, "memory.swap.max")); err == nil {
		stats.SwapLimit = v
	}
	if v, err := readNumber(filepath.Join(dir, "pids.current")); err == nil {
		stats.Pids = v
	}
	if v, err := readNumber(filepath.Join(dir, "pids.max")); err == nil {
		stats.PidsLimit = v
	}
	if content, err := readTrim(filepath.Join(dir, "io.stat")); err == nil {
		stats.IO = ParseIOStat(content)
	}
	if e.opts.Network {
		stats.Network = e.readNetwork(dir)
	}

	return stats, nil
}

// DefaultNetworkExcludeRegex 是默认排除的网卡正则（与 node_exporter 的默认口径一致）：
// 回环、veth、docker 网桥、tun/tap 等虚拟网卡没有监控意义，且数量随容器内 docker 网络增长。
const DefaultNetworkExcludeRegex = `^(lo|veth.*|docker[0-9]*|br-.*|virbr.*|tun[0-9]*|tap[0-9]*|zt.*|wg[0-9]*)$`

// maxCgroupDepth 是递归查找容器进程时允许的最大子 cgroup 深度。
// 容器内的 systemd 会把进程放进 init.scope / system.slice/xxx.service 这类子 cgroup，
// 实测两层就够，留点余量。
const maxCgroupDepth = 6

// readNetwork 取容器内第一个 pid，再读 /proc/<pid>/net/dev 拿到容器视角的网卡计数。
//
// 容器 cgroup 根上的 cgroup.procs 经常是空的（进程在子 cgroup 里，实测 debnas 的
// lxc.payload.<name>/cgroup.procs 为 0 行，pid 在 init.scope/cgroup.procs），
// 所以要往下递归找第一个有 pid 的 cgroup，否则网络指标会整体缺失。
//
// 读不到时返回 nil（不影响其它指标）。
func (e *Exporter) readNetwork(dir string) map[string]NetCounters {
	pid := firstPID(dir, 0)
	if pid == "" {
		return nil
	}
	netDev, err := readTrim(filepath.Join(e.opts.ProcRoot, pid, "net", "dev"))
	if err != nil {
		return nil
	}
	counters := ParseNetDev(netDev)
	if e.opts.NetworkFilter != nil {
		for name := range counters {
			if !e.opts.NetworkFilter.Match(name) {
				delete(counters, name)
			}
		}
	}
	return counters
}

// firstPID 在 cgroup 目录（含子目录，最多 maxCgroupDepth 层）里找第一个进程 pid。
func firstPID(dir string, depth int) string {
	if depth > maxCgroupDepth {
		return ""
	}
	if content, err := readTrim(filepath.Join(dir, "cgroup.procs")); err == nil {
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if pid := firstPID(filepath.Join(dir, ent.Name()), depth+1); pid != "" {
			return pid
		}
	}
	return ""
}

// emit 输出单个容器的全部指标。
func (e *Exporter) emit(ch chan<- prometheus.Metric, s ContainerStats) {
	name := s.Name
	ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 1, name)

	ch <- prometheus.MustNewConstMetric(e.cpuSeconds, prometheus.CounterValue, s.CPUUserSeconds, name, "user")
	ch <- prometheus.MustNewConstMetric(e.cpuSeconds, prometheus.CounterValue, s.CPUSystemSeconds, name, "system")
	ch <- prometheus.MustNewConstMetric(e.cpuThrottled, prometheus.CounterValue, s.CPUThrottledSeconds, name)
	ch <- prometheus.MustNewConstMetric(e.cpuNrThrottled, prometheus.CounterValue, s.CPUNrThrottled, name)

	ch <- prometheus.MustNewConstMetric(e.memoryBytes, prometheus.GaugeValue, s.MemoryBytes, name)
	if s.MemoryLimit != nil {
		ch <- prometheus.MustNewConstMetric(e.memoryLimit, prometheus.GaugeValue, *s.MemoryLimit, name)
		if *s.MemoryLimit > 0 {
			ch <- prometheus.MustNewConstMetric(e.memoryUsedPercent, prometheus.GaugeValue,
				100*util.SafeDiv(s.MemoryBytes, *s.MemoryLimit), name)
		}
	}
	for _, kind := range memoryStatKinds {
		if v, ok := s.MemoryStat[kind]; ok {
			ch <- prometheus.MustNewConstMetric(e.memoryStat, prometheus.GaugeValue, v, name, kind)
		}
	}

	if s.SwapBytes != nil {
		ch <- prometheus.MustNewConstMetric(e.swapBytes, prometheus.GaugeValue, *s.SwapBytes, name)
	}
	if s.SwapLimit != nil {
		ch <- prometheus.MustNewConstMetric(e.swapLimit, prometheus.GaugeValue, *s.SwapLimit, name)
	}
	if s.Pids != nil {
		ch <- prometheus.MustNewConstMetric(e.pids, prometheus.GaugeValue, *s.Pids, name)
	}
	if s.PidsLimit != nil {
		ch <- prometheus.MustNewConstMetric(e.pidsLimit, prometheus.GaugeValue, *s.PidsLimit, name)
	}

	for _, dev := range sortedKeys(s.IO) {
		c := s.IO[dev]
		ch <- prometheus.MustNewConstMetric(e.ioReadBytes, prometheus.CounterValue, c.ReadBytes, name, dev)
		ch <- prometheus.MustNewConstMetric(e.ioWriteBytes, prometheus.CounterValue, c.WriteBytes, name, dev)
		ch <- prometheus.MustNewConstMetric(e.ioReadOps, prometheus.CounterValue, c.ReadOps, name, dev)
		ch <- prometheus.MustNewConstMetric(e.ioWriteOps, prometheus.CounterValue, c.WriteOps, name, dev)
	}

	for _, dev := range sortedKeys(s.Network) {
		c := s.Network[dev]
		ch <- prometheus.MustNewConstMetric(e.netRecvBytes, prometheus.CounterValue, c.ReceiveBytes, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netTransBytes, prometheus.CounterValue, c.TransmitBytes, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netRecvPackets, prometheus.CounterValue, c.ReceivePackets, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netTransPackets, prometheus.CounterValue, c.TransmitPackets, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netRecvErrors, prometheus.CounterValue, c.ReceiveErrors, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netTransErrors, prometheus.CounterValue, c.TransmitErrors, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netRecvDropped, prometheus.CounterValue, c.ReceiveDropped, name, dev)
		ch <- prometheus.MustNewConstMetric(e.netTransDropped, prometheus.CounterValue, c.TransmitDropped, name, dev)
	}
}

// sortedKeys 返回 map 的键并按字典序排序，保证输出顺序稳定。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// String 返回采集配置的可读描述，用于启动日志。
func (e *Exporter) String() string {
	return fmt.Sprintf("cgroup-root=%s proc-root=%s network=%v filter(%s)",
		e.opts.CgroupRoot, e.opts.ProcRoot, e.opts.Network, e.opts.Filter)
}
