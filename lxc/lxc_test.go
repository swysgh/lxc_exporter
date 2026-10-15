package lxc

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"lxc_exporter/util"
)

// mustFilter 编译筛选规则，失败即测试失败。
func mustFilter(t *testing.T, include, exclude string) *util.Filter {
	t.Helper()
	f, err := util.NewFilter(include, exclude)
	if err != nil {
		t.Fatalf("NewFilter(%q, %q) 失败: %v", include, exclude, err)
	}
	return f
}

// gather 采集并返回「指标名 -> (标签串 -> 值)」。
func gather(t *testing.T, e *Exporter) map[string]map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]map[string]float64)
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			key := ""
			for _, lp := range m.GetLabel() {
				key += lp.GetName() + "=" + lp.GetValue() + ","
			}
			val := 0.0
			switch {
			case m.GetGauge() != nil:
				val = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				val = m.GetCounter().GetValue()
			}
			if out[mf.GetName()] == nil {
				out[mf.GetName()] = map[string]float64{}
			}
			out[mf.GetName()][key] = val
		}
	}
	return out
}

func TestParseCPUStat(t *testing.T) {
	content := "usage_usec 12345678\nuser_usec 3000000\nsystem_usec 2000000\nnr_throttled 7\nthrottled_usec 1500000\n"
	user, system, throttled, nr := ParseCPUStat(content)
	if user != 3.0 {
		t.Errorf("user = %v，期望 3", user)
	}
	if system != 2.0 {
		t.Errorf("system = %v，期望 2", system)
	}
	if throttled != 1.5 {
		t.Errorf("throttled = %v，期望 1.5", throttled)
	}
	if nr != 7 {
		t.Errorf("nr_throttled = %v，期望 7", nr)
	}
}

func TestParseIOStat(t *testing.T) {
	content := "8:0 rbytes=1048576 wbytes=2097152 rios=1024 wios=2048 dbytes=0 dios=0\n"
	io := ParseIOStat(content)
	c, ok := io["8:0"]
	if !ok {
		t.Fatalf("缺少 8:0: %+v", io)
	}
	if c.ReadBytes != 1048576 || c.WriteBytes != 2097152 || c.ReadOps != 1024 || c.WriteOps != 2048 {
		t.Errorf("io 计数 = %+v", c)
	}
}

func TestParseNetDev(t *testing.T) {
	content := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 1048576 2048 3 4 0 0 0 0 2097152 4096 5 6 0 0 0 0
`
	nets := ParseNetDev(content)
	if len(nets) != 2 {
		t.Fatalf("解析到 %d 个网卡，期望 2: %+v", len(nets), nets)
	}
	eth := nets["eth0"]
	if eth.ReceiveBytes != 1048576 || eth.ReceivePackets != 2048 {
		t.Errorf("eth0 接收 = %+v", eth)
	}
	if eth.ReceiveErrors != 3 || eth.ReceiveDropped != 4 {
		t.Errorf("eth0 接收错误/丢包 = %v/%v", eth.ReceiveErrors, eth.ReceiveDropped)
	}
	if eth.TransmitBytes != 2097152 || eth.TransmitPackets != 4096 {
		t.Errorf("eth0 发送 = %+v", eth)
	}
	if eth.TransmitErrors != 5 || eth.TransmitDropped != 6 {
		t.Errorf("eth0 发送错误/丢包 = %v/%v", eth.TransmitErrors, eth.TransmitDropped)
	}
}

func TestDiscoverContainers(t *testing.T) {
	names, err := DiscoverContainers("testdata/cgroup")
	if err != nil {
		t.Fatalf("DiscoverContainers 失败: %v", err)
	}
	if len(names) != 2 || names[0] != "demo" || names[1] != "maxed" {
		t.Errorf("发现容器 = %v，期望 [demo maxed]（应排除 lxc.monitor.* 与 lxc.pivot）", names)
	}
}

// TestCollectMetrics 断言各指标等于 testdata 里的数值。
func TestCollectMetrics(t *testing.T) {
	e := New(Options{
		CgroupRoot: "testdata/cgroup",
		ProcRoot:   "testdata/proc",
		Network:    true,
	})
	got := gather(t, e)

	if v := got["lxc_container_up"]["name=demo,"]; v != 1 {
		t.Errorf("up{demo} = %v，期望 1", v)
	}
	if v := got["lxc_container_up"]["name=maxed,"]; v != 1 {
		t.Errorf("up{maxed} = %v，期望 1", v)
	}

	// CPU：user=3s system=2s。
	if v := got["lxc_container_cpu_seconds_total"]["mode=user,name=demo,"]; v != 3 {
		t.Errorf("cpu user = %v，期望 3", v)
	}
	if v := got["lxc_container_cpu_seconds_total"]["mode=system,name=demo,"]; v != 2 {
		t.Errorf("cpu system = %v，期望 2", v)
	}
	if v := got["lxc_container_cpu_throttled_seconds_total"]["name=demo,"]; v != 1.5 {
		t.Errorf("cpu throttled = %v，期望 1.5", v)
	}
	if v := got["lxc_container_cpu_nr_throttled_total"]["name=demo,"]; v != 7 {
		t.Errorf("nr_throttled = %v，期望 7", v)
	}

	// 内存。
	if v := got["lxc_container_memory_bytes"]["name=demo,"]; v != 536870912 {
		t.Errorf("memory = %v，期望 536870912", v)
	}
	if v := got["lxc_container_memory_limit_bytes"]["name=demo,"]; v != 1073741824 {
		t.Errorf("memory limit = %v，期望 1073741824", v)
	}
	if v := got["lxc_container_memory_used_percent"]["name=demo,"]; v != 50 {
		t.Errorf("memory used_percent = %v，期望 50", v)
	}
	if v := got["lxc_container_memory_stat_bytes"]["kind=anon,name=demo,"]; v != 104857600 {
		t.Errorf("memory.stat anon = %v，期望 104857600", v)
	}
	if v := got["lxc_container_memory_stat_bytes"]["kind=slab,name=demo,"]; v != 5242880 {
		t.Errorf("memory.stat slab = %v，期望 5242880", v)
	}

	// swap。
	if v := got["lxc_container_swap_bytes"]["name=demo,"]; v != 1048576 {
		t.Errorf("swap = %v，期望 1048576", v)
	}
	if v := got["lxc_container_swap_limit_bytes"]["name=demo,"]; v != 2147483648 {
		t.Errorf("swap limit = %v，期望 2147483648", v)
	}

	// pids。
	if v := got["lxc_container_pids"]["name=demo,"]; v != 12 {
		t.Errorf("pids = %v，期望 12", v)
	}
	if v := got["lxc_container_pids_limit"]["name=demo,"]; v != 100 {
		t.Errorf("pids limit = %v，期望 100", v)
	}

	// io：设备标签为 maj:min。
	if v := got["lxc_container_io_read_bytes_total"]["device=8:0,name=demo,"]; v != 1048576 {
		t.Errorf("io 8:0 read = %v，期望 1048576", v)
	}
	if v := got["lxc_container_io_write_ops_total"]["device=259:1,name=demo,"]; v != 8 {
		t.Errorf("io 259:1 write ops = %v，期望 8", v)
	}

	// 网络：来自 testdata/proc/4242/net/dev。
	if v := got["lxc_container_network_receive_bytes_total"]["device=eth0,name=demo,"]; v != 1048576 {
		t.Errorf("net recv bytes = %v，期望 1048576", v)
	}
	if v := got["lxc_container_network_transmit_bytes_total"]["device=eth0,name=demo,"]; v != 2097152 {
		t.Errorf("net trans bytes = %v，期望 2097152", v)
	}
	if v := got["lxc_container_network_receive_dropped_total"]["device=eth0,name=demo,"]; v != 4 {
		t.Errorf("net recv dropped = %v，期望 4", v)
	}
	if v := got["lxc_container_network_transmit_errors_total"]["device=eth0,name=demo,"]; v != 5 {
		t.Errorf("net trans errors = %v，期望 5", v)
	}

	// 采集本身没有出错。
	if v := got["lxc_scrape_error"][""]; v != 0 {
		t.Errorf("scrape_error = %v，期望 0", v)
	}
}

// TestMemoryMaxNotReported 覆盖：memory.max 为 "max" 时
// 不上报 lxc_container_memory_limit_bytes 与 lxc_container_memory_used_percent。
func TestMemoryMaxNotReported(t *testing.T) {
	e := New(Options{CgroupRoot: "testdata/cgroup", ProcRoot: "testdata/proc"})
	got := gather(t, e)

	if _, ok := got["lxc_container_memory_limit_bytes"]["name=maxed,"]; ok {
		t.Error("memory.max=max 时不应上报 memory_limit_bytes")
	}
	if _, ok := got["lxc_container_memory_used_percent"]["name=maxed,"]; ok {
		t.Error("memory.max=max 时不应上报 memory_used_percent")
	}
	// pids.max=max 同理。
	if _, ok := got["lxc_container_pids_limit"]["name=maxed,"]; ok {
		t.Error("pids.max=max 时不应上报 pids_limit")
	}
	// 但其它指标仍应存在。
	if v := got["lxc_container_memory_bytes"]["name=maxed,"]; v != 100000 {
		t.Errorf("maxed memory = %v，期望 100000", v)
	}
}

// TestAbsentContainerNotExported 覆盖：容器目录不存在时，
// 该容器不出现在指标里（而不是 up=0）。
func TestAbsentContainerNotExported(t *testing.T) {
	e := New(Options{CgroupRoot: "testdata/cgroup-absent", ProcRoot: "testdata/proc"})
	got := gather(t, e)

	for metric, series := range got {
		for key := range series {
			if strings.Contains(key, "name=demo,") {
				t.Errorf("不存在的容器 demo 不应出现: %s{%s}", metric, key)
			}
		}
	}
	// 存在的容器 other 仍应上报。
	if v := got["lxc_container_up"]["name=other,"]; v != 1 {
		t.Errorf("other up = %v，期望 1", v)
	}
}

// TestMonitorDirNotExported 覆盖：lxc.monitor.demo 存在但 lxc.payload.demo 不存在时，
// 不产生任何 name="demo" 的序列。
func TestMonitorDirNotExported(t *testing.T) {
	e := New(Options{CgroupRoot: "testdata/cgroup-monitor", ProcRoot: "testdata/proc"})
	got := gather(t, e)

	for metric, series := range got {
		for key := range series {
			if strings.Contains(key, "name=demo,") {
				t.Errorf("lxc.monitor.demo 不应产生容器序列: %s{%s}", metric, key)
			}
		}
	}
	// 没有 payload 目录 -> 没有任何 up 序列。
	if len(got["lxc_container_up"]) != 0 {
		t.Errorf("不应有任何容器: %v", got["lxc_container_up"])
	}
}

// TestNetworkDisabled 验证 -lxc.network=false 时不采集网络指标。
func TestNetworkDisabled(t *testing.T) {
	e := New(Options{CgroupRoot: "testdata/cgroup", ProcRoot: "testdata/proc", Network: false})
	got := gather(t, e)
	if len(got["lxc_container_network_receive_bytes_total"]) != 0 {
		t.Error("关闭网络采集后不应有网络指标")
	}
}

// TestContainerFilter 验证 include/exclude 规则。
func TestContainerFilter(t *testing.T) {
	filter := mustFilter(t, "^demo$", "")
	e := New(Options{CgroupRoot: "testdata/cgroup", ProcRoot: "testdata/proc", Filter: filter})
	got := gather(t, e)
	if _, ok := got["lxc_container_up"]["name=demo,"]; !ok {
		t.Error("demo 应通过 include 规则")
	}
	if _, ok := got["lxc_container_up"]["name=maxed,"]; ok {
		t.Error("maxed 不应通过 include 规则")
	}
}

// TestMissingCgroupRoot 验证 cgroup 根目录不存在时 scrape_error=1 且不 panic。
func TestMissingCgroupRoot(t *testing.T) {
	e := New(Options{CgroupRoot: "testdata/does-not-exist"})
	got := gather(t, e)
	if v := got["lxc_scrape_error"][""]; v != 1 {
		t.Errorf("scrape_error = %v，期望 1", v)
	}
}

// TestNewDefaults 验证默认路径与超时。
func TestNewDefaults(t *testing.T) {
	e := New(Options{})
	if e.opts.CgroupRoot != "/sys/fs/cgroup" {
		t.Errorf("CgroupRoot = %q", e.opts.CgroupRoot)
	}
	if e.opts.ProcRoot != "/proc" {
		t.Errorf("ProcRoot = %q", e.opts.ProcRoot)
	}
	if e.opts.Timeout <= 0 {
		t.Errorf("Timeout = %v", e.opts.Timeout)
	}
}

// TestNestedCgroupNetwork 覆盖：容器 cgroup 根上的 cgroup.procs 为空
// （进程在 init.scope 这类子 cgroup 里）时，仍能取到 pid 并采到网络指标。
// 实测 debnas 上就是这个形态，早期实现只读根 cgroup.procs，导致所有容器都没有网络指标。
func TestNestedCgroupNetwork(t *testing.T) {
	e := New(Options{
		CgroupRoot:    "testdata/cgroup-nested",
		ProcRoot:      "testdata/proc",
		Network:       true,
		NetworkFilter: mustFilter(t, "", DefaultNetworkExcludeRegex),
	})
	got := gather(t, e)

	if v := got["lxc_container_up"]["name=nested,"]; v != 1 {
		t.Fatalf("up{nested} = %v，期望 1", v)
	}
	if v := got["lxc_container_network_receive_bytes_total"]["device=eth0,name=nested,"]; v != 1234567 {
		t.Errorf("嵌套 cgroup 下 net recv bytes = %v，期望 1234567", v)
	}
	if v := got["lxc_container_network_transmit_bytes_total"]["device=eth0,name=nested,"]; v != 7654321 {
		t.Errorf("嵌套 cgroup 下 net trans bytes = %v，期望 7654321", v)
	}
	// 默认网卡筛选要把 lo 这类虚拟网卡排掉（fixture 里 lo 也存在）。
	if _, ok := got["lxc_container_network_receive_bytes_total"]["device=lo,name=nested,"]; ok {
		t.Errorf("lo 应被默认网卡筛选排除")
	}
}

// TestFirstPID 直接测递归查找：根 cgroup.procs 为空时要往下找到 init.scope 里的 pid。
func TestFirstPID(t *testing.T) {
	if pid := firstPID("testdata/cgroup-nested/lxc.payload.nested", 0); pid != "7777" {
		t.Errorf("firstPID = %q，期望 7777", pid)
	}
	if pid := firstPID("testdata/cgroup-nested/lxc.payload.does-not-exist", 0); pid != "" {
		t.Errorf("目录不存在时期望空串，得到 %q", pid)
	}
}
