// lxc_exporter 采集纯 LXC 容器的资源维度指标（读 cgroup v2 与 /proc），
// 是 node_exporter 拆分出来的单一职责 exporter，默认监听 :9328。
//
// 不使用 LXD/Incus API，因此适用于只有纯 LXC（没有 lxd/incus）的主机。
// 所有指标都会带上 instance=<hostname> 标签（见 instance 包）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"lxc_exporter/instance"
	"lxc_exporter/lxc"
	"lxc_exporter/util"
)

// buildVersion 与 buildCommit 在编译时通过 -ldflags 注入。
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

// version 由 main 在解析参数前填充，这里只为 -version 兜底。
var version = fmt.Sprintf("lxc_exporter %s", buildVersion)

// flags 保存全部命令行参数。
type flags struct {
	listenAddress  string
	telemetryPath  string
	authToken      string
	configPath     string
	hostname       string
	commandTimeout time.Duration

	cgroupRoot string
	procRoot   string

	lxcInclude        string
	lxcExclude        string
	lxcNetwork        bool
	lxcNetworkExclude string
}

// fileConfig 对应 -config 指定的 JSON 配置文件，字段一律使用指针。
type fileConfig struct {
	ListenAddress  *string `json:"listen_address"`
	TelemetryPath  *string `json:"telemetry_path"`
	AuthToken      *string `json:"auth_token"`
	Hostname       *string `json:"hostname"`
	CommandTimeout *string `json:"command_timeout"`

	CgroupRoot *string `json:"cgroup_root"`
	ProcRoot   *string `json:"proc_root"`

	LxcInclude        *string `json:"lxc_include"`
	LxcExclude        *string `json:"lxc_exclude"`
	LxcNetwork        *bool   `json:"lxc_network"`
	LxcNetworkExclude *string `json:"lxc_network_exclude"`
}

// registerFlags 定义全部命令行参数。
func registerFlags(fs *flag.FlagSet, f *flags) {
	fs.StringVar(&f.listenAddress, "web.listen-address", ":9328",
		"HTTP 监听地址，lxc_exporter 惯例为 :9328")
	fs.StringVar(&f.telemetryPath, "web.telemetry-path", "/metrics",
		"指标暴露路径")
	fs.StringVar(&f.authToken, "auth-token", "",
		"鉴权 token；非空时要求 Authorization: Bearer <token>")
	fs.StringVar(&f.configPath, "config", "",
		"JSON 配置文件路径；命令行显式给出的参数优先于配置文件")
	fs.StringVar(&f.hostname, "hostname", "",
		"覆盖自动识别的 hostname，留空则自动识别（用于 instance 标签）")
	fs.DurationVar(&f.commandTimeout, "command-timeout", 20*time.Second,
		"读取超时时间")

	fs.StringVar(&f.cgroupRoot, "cgroup-root", "/sys/fs/cgroup",
		"cgroup v2 根目录")
	fs.StringVar(&f.procRoot, "proc-root", "/proc",
		"procfs 根目录（用于读取容器进程的 net/dev）")

	fs.StringVar(&f.lxcInclude, "lxc.include", "",
		"容器名包含正则：只导出匹配的容器")
	fs.StringVar(&f.lxcExclude, "lxc.exclude", "",
		"容器名排除正则：导出时不包含匹配的容器")
	fs.BoolVar(&f.lxcNetwork, "lxc.network", true,
		"采集容器网卡流量（需要读取容器进程的 /proc/<pid>/net/dev）")
	fs.StringVar(&f.lxcNetworkExclude, "lxc.network-exclude", lxc.DefaultNetworkExcludeRegex,
		"网卡名排除正则；传空字符串可采集全部网卡（容器内 docker 会带来大量 br-*/veth* 序列）")
}

// loadConfig 读取并解析 JSON 配置文件，未知键会报错。
func loadConfig(path string) (*fileConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法打开配置文件 %s: %w", path, err)
	}
	defer file.Close()

	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()

	var cfg fileConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return &cfg, nil
}

// mergeConfig 把配置文件里出现、且命令行没有显式设置的项合并进 flags。
func mergeConfig(fs *flag.FlagSet, f *flags, cfg *fileConfig) error {
	visited := make(map[string]bool)
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	setStr := func(name string, dst *string, src *string) {
		if src != nil && !visited[name] {
			*dst = *src
		}
	}
	setBool := func(name string, dst *bool, src *bool) {
		if src != nil && !visited[name] {
			*dst = *src
		}
	}

	setStr("web.listen-address", &f.listenAddress, cfg.ListenAddress)
	setStr("web.telemetry-path", &f.telemetryPath, cfg.TelemetryPath)
	setStr("auth-token", &f.authToken, cfg.AuthToken)
	setStr("hostname", &f.hostname, cfg.Hostname)

	if cfg.CommandTimeout != nil && !visited["command-timeout"] {
		d, err := time.ParseDuration(*cfg.CommandTimeout)
		if err != nil {
			return fmt.Errorf("配置文件中的 command_timeout 非法: %w", err)
		}
		f.commandTimeout = d
	}

	setStr("cgroup-root", &f.cgroupRoot, cfg.CgroupRoot)
	setStr("proc-root", &f.procRoot, cfg.ProcRoot)
	setStr("lxc.include", &f.lxcInclude, cfg.LxcInclude)
	setStr("lxc.exclude", &f.lxcExclude, cfg.LxcExclude)
	setBool("lxc.network", &f.lxcNetwork, cfg.LxcNetwork)
	setStr("lxc.network-exclude", &f.lxcNetworkExclude, cfg.LxcNetworkExclude)

	return nil
}

// validateListenAddress 校验监听地址：必须是 host:port 且端口在 1-65535。
func validateListenAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("监听地址 %q 非法: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("监听地址 %q 的端口 %q 非法（应在 1-65535 之间）", addr, port)
	}
	return nil
}

// maskToken 对鉴权 token 打码：只保留前 4 位，空 token 显示为 (未启用)。
func maskToken(token string) string {
	if token == "" {
		return "(未启用)"
	}
	n := 4
	if len(token) < n {
		n = len(token)
	}
	return token[:n] + "***"
}

// hasValidToken 校验请求头中的 Bearer token。
func hasValidToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix)) == token
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[lxc_exporter] ")

	var (
		f           flags
		showVersion bool
	)
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	registerFlags(fs, &f)
	fs.BoolVar(&showVersion, "version", false, "打印版本信息后退出")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "lxc_exporter —— LXC 容器指标采集器（默认监听 :9328）\n\n用法: %s [flags]\n\n", fs.Name())
		fs.PrintDefaults()
	}
	version = versionString()
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatalf("解析命令行参数失败: %v", err)
	}
	if showVersion {
		fmt.Println(version)
		return
	}

	// ---- 配置文件合并（必须发生在任何派生变量计算之前）----
	if f.configPath != "" {
		cfg, err := loadConfig(f.configPath)
		if err != nil {
			log.Fatalf("配置非法: %v", err)
		}
		if err := mergeConfig(fs, &f, cfg); err != nil {
			log.Fatalf("配置非法: %v", err)
		}
	}

	// ---- 校验 ----
	if err := validateListenAddress(f.listenAddress); err != nil {
		log.Fatalf("配置非法: %v", err)
	}
	if f.commandTimeout <= 0 {
		log.Fatalf("配置非法: command-timeout 必须大于 0，当前为 %v", f.commandTimeout)
	}
	if !strings.HasPrefix(f.telemetryPath, "/") {
		log.Fatalf("配置非法: web.telemetry-path 必须以 / 开头，当前为 %q", f.telemetryPath)
	}

	containerFilter, err := util.NewFilter(f.lxcInclude, f.lxcExclude)
	if err != nil {
		log.Fatalf("容器筛选规则无效: %v", err)
	}
	networkFilter, err := util.NewFilter("", f.lxcNetworkExclude)
	if err != nil {
		log.Fatalf("网卡筛选规则无效: %v", err)
	}

	// ---- hostname 与 instance 标签 ----
	host := instance.Detect(f.hostname)
	if host == "unknown" {
		log.Printf("警告: 未能识别 hostname，instance 标签将使用 %q，"+
			"可通过 -hostname 显式指定", host)
	}

	// ---- 注册采集器 ----
	registry := prometheus.NewRegistry()
	enabled := []string{"lxc"}

	lxcExporter := lxc.New(lxc.Options{
		CgroupRoot:    f.cgroupRoot,
		ProcRoot:      f.procRoot,
		Filter:        containerFilter,
		Network:       f.lxcNetwork,
		NetworkFilter: networkFilter,
		Timeout:       f.commandTimeout,
	})
	registry.MustRegister(lxcExporter)
	log.Printf("LXC 采集: %s", lxcExporter)

	// go_* 反映 exporter 自身状态，process_* 反映 exporter 进程资源占用。
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	enabled = append(enabled, "runtime")

	gatherer := instance.WrapGatherer(registry, host)
	handler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})

	mux := http.NewServeMux()
	mux.Handle(f.telemetryPath, handler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html>
<head><title>lxc_exporter</title></head>
<body>
<h1>lxc_exporter</h1>
<p>instance: <code>%s</code></p>
<p>版本: <code>%s</code></p>
<p>已启用模块: <code>%s</code></p>
<p><a href="%s">Metrics</a></p>
</body>
</html>`, host, version, strings.Join(enabled, ", "), f.telemetryPath)
	})

	// ---- 鉴权（/favicon.ico 在鉴权前短路返回 204）----
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f.authToken != "" {
			if !hasValidToken(r, f.authToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              f.listenAddress,
		Handler:           authHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * f.commandTimeout,
		IdleTimeout:       60 * time.Second,
	}

	// ---- 启动日志（打印实际生效的完整配置）----
	log.Printf("版本: %s (%s %s)", buildVersion, runtime.Version(), runtime.GOARCH)
	log.Printf("监听地址: %s", f.listenAddress)
	log.Printf("指标路径: %s", f.telemetryPath)
	log.Printf("鉴权 token: %s", maskToken(f.authToken))
	if f.authToken == "" {
		log.Printf("警告: 未设置 auth_token，/metrics 将不做鉴权")
	}
	log.Printf("instance 标签: %s", host)
	log.Printf("cgroup-root=%s proc-root=%s lxc.network=%v", f.cgroupRoot, f.procRoot, f.lxcNetwork)
	if f.lxcNetwork {
		log.Printf("网卡筛选: %s", networkFilter)
	}
	log.Printf("容器筛选: %s", containerFilter)
	log.Printf("command-timeout: %s", f.commandTimeout)
	log.Printf("已启用模块: %s", strings.Join(enabled, ", "))
	log.Printf("开始监听 %s%s", f.listenAddress, f.telemetryPath)

	// ---- 优雅退出 ----
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("HTTP 服务异常退出: %v", err)
	case <-ctx.Done():
		log.Printf("收到退出信号，正在关闭 HTTP 服务…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("关闭 HTTP 服务时出错: %v", err)
	}
	log.Printf("已退出")
}

// versionString 拼装版本信息，包含 VCS 信息（若编译时可用）。
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fmt.Sprintf("lxc_exporter %s (commit %s, built %s)",
			buildVersion, buildCommit, buildDate)
	}

	var rev, dirty, date string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value
		case "vcs.time":
			date = s.Value
		}
	}
	if buildCommit != "unknown" {
		rev = buildCommit
	}
	if buildDate != "unknown" {
		date = buildDate
	}
	if rev != "" && len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty == "true" {
		rev += "-dirty"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "lxc_exporter %s", buildVersion)
	fmt.Fprintf(&b, " (%s %s/%s", info.GoVersion, runtime.GOOS, runtime.GOARCH)
	if rev != "" && rev != "unknown" {
		fmt.Fprintf(&b, ", commit %s", rev)
	}
	if date != "" && date != "unknown" {
		fmt.Fprintf(&b, ", built %s", date)
	}
	b.WriteString(")")
	return b.String()
}
