# lxc_exporter

纯 LXC 容器（读 cgroup v2 与 `/proc`）的 Prometheus 指标采集器，用 Go 编写，单文件部署。
从 `node_exporter` 拆分而来，只负责 LXC 容器单一职责，部署在跑纯 LXC 的主机上（例如 debnas）。

**不使用 LXD/Incus API**，因此适用于只有纯 LXC（没有 `lxd`/`incus`）的主机：
容器发现完全依赖 cgroup v2 的 `lxc.payload.<name>` 目录。

所有输出指标都带 `instance="<hostname>"` 标签（自动识别本机主机名，可用 `-hostname` 覆盖）。

## 功能

| 模块 | 采集内容 | 默认 | 开关 |
| --- | --- | --- | --- |
| 容器发现 | cgroup v2 下 `lxc.payload.<name>` 目录（自动排除 `lxc.monitor.*`、`lxc.pivot`） | 开启 | 始终启用 |
| CPU | `cpu.stat`：user/system 累计秒数、限流时间与次数 | 开启 | 始终启用 |
| 内存 | `memory.current` / `memory.max` / `memory.stat` / swap | 开启 | 始终启用 |
| 进程数 | `pids.current` / `pids.max` | 开启 | 始终启用 |
| 块 IO | `io.stat`：按 `maj:min` 的读写字节与次数 | 开启 | 始终启用 |
| 网络 | 容器进程的 `/proc/<pid>/net/dev`：收发字节/包/错误/丢包（**支持按网卡筛选**） | 开启 | `-lxc.network` |
| 运行时 | `go_*`、`process_*`（exporter 自身） | 开启 | 始终启用 |

监听端口默认 **9328**，路径 `/metrics`。

## 快速开始

```bash
# 编译
make build

# 前台运行
./lxc_exporter

# 验证
curl -s localhost:9328/metrics | head
```

浏览器访问 `http://<host>:9328/` 也能看到当前 instance 与已启用模块。

### 配置文件

除命令行参数外，还可以用 `-config <path>` 读取一个 JSON 配置文件。
优先级为：**命令行显式给出的参数 > 配置文件 > 内置默认值**。
仓库根目录的 `config.example.json` 列出了全部可用字段。
配置文件使用 `DisallowUnknownFields` 解析，拼错的键会在启动时报错退出。

## 安装为 systemd 服务

```bash
sudo make install            # 安装二进制与 service 文件
sudo make install-service    # 安装并启动
```

> 采集需要读取 `/sys/fs/cgroup` 与容器进程的 `/proc/<pid>/net/dev`，
> 因此服务以 `root` 运行。

## 常用参数

```bash
./lxc_exporter -h          # 查看全部参数
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-web.listen-address` | `:9328` | 监听地址 |
| `-web.telemetry-path` | `/metrics` | 指标路径 |
| `-auth-token` | 空 | 非空时要求 `Authorization: Bearer <token>`，不匹配返回 401 |
| `-config` | 空 | JSON 配置文件路径 |
| `-hostname` | 空（自动识别） | 覆盖 instance 标签使用的 hostname |
| `-cgroup-root` | `/sys/fs/cgroup` | cgroup v2 根目录 |
| `-proc-root` | `/proc` | procfs 根目录（用于读取容器进程的 net/dev） |
| `-lxc.include` | 空 | 正则，只导出匹配的容器名 |
| `-lxc.exclude` | 空 | 正则，导出时排除匹配的容器名 |
| `-lxc.network` | `true` | 是否采集容器网卡流量 |
| `-lxc.network-exclude` | `^(lo\|veth.*\|docker[0-9]*\|br-.*\|virbr.*\|tun[0-9]*\|tap[0-9]*\|zt.*\|wg[0-9]*)$` | 网卡名排除正则；容器里跑 docker 会带出大量 `br-*`/`veth*` 序列，默认排掉；传空字符串可采集全部 |
| `-command-timeout` | `20s` | 读取超时时间 |

`-lxc.include` / `-lxc.exclude` 是 Go 正则，非法正则会在启动时报错退出。

```bash
# 只看 web- 开头的容器，排除临时容器
./lxc_exporter -lxc.include='^web-' -lxc.exclude='-tmp$'
```

## 数据来源

容器发现与各指标全部来自 cgroup v2，不调用任何 LXD/Incus API：

```
/sys/fs/cgroup/lxc.payload.<name>/   容器 cgroup 目录（存在即视为运行中）
  cpu.stat                          CPU 时间与限流
  memory.current / memory.max       内存占用与上限
  memory.stat                       内存明细（anon/file/kernel/slab/sock/shmem）
  memory.swap.current / .max        swap 占用与上限
  io.stat                           块设备读写字节/次数
  pids.current / pids.max           进程数
  cgroup.procs                      容器内进程（用于定位 netns）
/proc/<pid>/net/dev                 容器网卡收发计数
```

目录名必须形如 `lxc.payload.<name>`，`lxc.monitor.*` / `lxc.pivot` 等非容器条目会被排除。
**已停止的容器没有该目录，不会为它们保留序列**（不是 `up=0`）。

网络采集先在容器 cgroup 里找第一个 pid（**cgroup 根上的 `cgroup.procs` 常为空** —— 容器内 systemd 把进程放在
`init.scope/`、`system.slice/` 等子 cgroup 里，所以要往下递归找），再读 `/proc/<pid>/net/dev`；
该 pid 在容器的 netns 里，宿主读它拿到的是容器视角的计数。读不到时跳过网络指标，不影响其它指标。

## 指标一览

前缀统一为 `lxc_`，并带有 `lxc_scrape_error`（0 正常 / 1 出错）与
`lxc_scrape_duration_seconds`。单个容器读文件失败（正在启停）只跳过该容器，不算错误；
只有 cgroup 根目录整体读取失败才会把 `lxc_scrape_error` 置为 1。

```
lxc_container_up{name}                                        运行中=1
lxc_container_cpu_seconds_total{name,mode}                    mode=user|system
lxc_container_cpu_throttled_seconds_total{name}
lxc_container_cpu_nr_throttled_total{name}
lxc_container_memory_bytes{name}
lxc_container_memory_limit_bytes{name}                        memory.max 为 max 时不输出
lxc_container_memory_used_percent{name}                       仅在有上限时输出
lxc_container_memory_stat_bytes{name,kind}                    kind=anon|file|kernel|slab|sock|shmem
lxc_container_swap_bytes{name} / lxc_container_swap_limit_bytes{name}
lxc_container_pids{name} / lxc_container_pids_limit{name}
lxc_container_io_read_bytes_total{name,device}                device=maj:min
lxc_container_io_write_bytes_total{name,device}
lxc_container_io_read_ops_total{name,device}
lxc_container_io_write_ops_total{name,device}
lxc_container_network_receive_bytes_total{name,device}
lxc_container_network_transmit_bytes_total{name,device}
lxc_container_network_receive_packets_total{name,device}
lxc_container_network_transmit_packets_total{name,device}
lxc_container_network_receive_errors_total{name,device}
lxc_container_network_transmit_errors_total{name,device}
lxc_container_network_receive_dropped_total{name,device}
lxc_container_network_transmit_dropped_total{name,device}
```

## Prometheus 配置示例

```yaml
scrape_configs:
  - job_name: lxc
    static_configs:
      - targets: ['192.168.1.10:9328']
    scrape_interval: 30s
```

因为 exporter 会主动上报 `instance="<hostname>"`，Prometheus 默认 `honor_labels: false`
会让 exporter 的标签覆盖目标地址，所以面板里显示的是主机名。

## 常用告警示例

```yaml
groups:
  - name: lxc_exporter
    rules:
      - alert: LXC 容器内存接近上限
        expr: lxc_container_memory_used_percent > 90
        for: 10m
        labels: {severity: warning}

      - alert: LXC 容器被 CPU 限流
        expr: rate(lxc_container_cpu_throttled_seconds_total[5m]) > 0.1
        for: 15m
        labels: {severity: warning}

      - alert: LXC 容器进程数接近上限
        expr: lxc_container_pids / lxc_container_pids_limit > 0.9
        for: 10m
        labels: {severity: warning}

      - alert: LXC 网卡出现丢包
        expr: rate(lxc_container_network_receive_dropped_total[5m]) > 0
        for: 15m
        labels: {severity: warning}

      - alert: 采集器自身异常
        expr: lxc_scrape_error > 0
        for: 10m
        labels: {severity: warning}
```

## 依赖说明

| 模块 | 依赖 |
| --- | --- |
| 容器发现 / CPU / 内存 / 进程数 / IO | cgroup v2（`/sys/fs/cgroup`） |
| 网络 | `/proc/<pid>/net/dev` |

不依赖 `lxd` / `incus`。cgroup 根目录不存在或不可读时不会导致整体抓取失败：
`lxc_scrape_error` 会被置为 1，其余指标照常输出。

## 开发

```bash
make test        # 单元测试
make test-race   # 竞态检测
make cover       # 覆盖率
make lint        # gofmt + go vet
make build       # 编译
```

测试用 `testdata/` 里的假 cgroup 树与假 `/proc`，不依赖真实 `/sys/fs/cgroup`。

目录结构：

```
main.go                入口：参数/配置解析、HTTP 服务、instance 标签改写
lxc/                   LXC 容器发现与 cgroup/proc 指标采集与解析
util/                  最小工具函数：数值解析、正则筛选
instance/              hostname 识别与 instance 标签改写
```

## Grafana 面板

面板标题 **LXC 容器（debnas）**（uid `lxc-monitor`，11 块）。`grafana/` 下两份是同一套面板，粘的地方不同：

| 文件 | 用在哪 |
| --- | --- |
| `grafana/dashboard.json` | Dashboards → New → **Import**（v1，Grafana 会自动迁移成 v2） |
| `grafana/dashboard-v2.json` | Dashboard settings → **JSON Model**（v2，Grafana 13 的编辑器只认这个格式） |

导入时把 `DS_PROMETHEUS` 绑到自己的 Prometheus 数据源。面板清单：

| # | 面板 | 类型 | PromQL |
| --- | --- | --- | --- |
| 1 | 运行中的容器 | stat | `count(lxc_container_up{instance!~"[0-9.]+:[0-9]+"})` |
| 2 | 容器内存合计 | stat | `sum(lxc_container_memory_bytes{instance!~"[0-9.]+:[0-9]+"})` |
| 3 | CPU 被限流次数（1h） | stat | `sum(round(increase(lxc_container_cpu_nr_throttled_total{instance!~"[0-9.]+:[0-9]+"}[1h])))` |
| 6 | 容器内存 | timeseries | `lxc_container_memory_bytes{name=~"$name",instance!~"[0-9.]+:[0-9]+"}` |
| 4 | 容器概览 | table | `lxc_container_memory_bytes{instance!~"[0-9.]+:[0-9]+"}<br>lxc_container_pids{instance!~"[0-9.]+:[0-9]+"}<br>sum by (instance, name) (rate(lxc_container_cpu_seconds_total{instance!~"[0-9.]+:[0-9]+"}[5m]))` |
| 5 | 容器 CPU 使用（核数） | timeseries | `sum by (name) (rate(lxc_container_cpu_seconds_total{name=~"$name",instance!~"[0-9.]+:[0-9]+"}[$__rate_interval]))` |
| 8 | 容器磁盘 IO | timeseries | `sum by (name) (rate(lxc_container_io_read_bytes_total{name=~"$name",instance!~"[0-9.]+:[0-9]+"}[$__rate_interval]))<br>sum by (name) (rate(lxc_container_io_write_bytes_total{name=~"$name",instance!~"[0-9.]+:[0-9]+"}[$__rate_interval]))` |
| 9 | 容器网络 | timeseries | `sum by (name) (rate(lxc_container_network_receive_bytes_total{name=~"$name",instance!~"[0-9.]+:[0-9]+"}[$__rate_interval]))<br>sum by (name) (rate(lxc_container_network_transmit_bytes_total{name=~"$name",instance!~"[0-9.]+:[0-9]+"}[$__rate_interval]))` |
| 10 | 容器网络累计（重启后归零） | table | `sum without (device) (lxc_container_network_receive_bytes_total{instance!~"[0-9.]+:[0-9]+"})<br>sum without (device) (lxc_container_network_transmit_bytes_total{instance!~"[0-9.]+:[0-9]+"})` |
| 11 | 容器 swap | timeseries | `lxc_container_swap_bytes{name=~"$name",instance!~"[0-9.]+:[0-9]+"}` |
| 7 | 容器内存构成 | timeseries | `sum by (kind) (lxc_container_memory_stat_bytes{name=~"$name",instance!~"[0-9.]+:[0-9]+"})` |

变量：DS_PROMETHEUS、name。

表格面板的变换流水线固定为 `labelsToFields → merge → groupBy → organize`（多查询表格必须有 `groupBy`）：各查询是 instant 求值、时间戳各不相同，`merge` 无法按时间把行对齐，会把每个查询的结果摞成独立行块（表现是「进程数」「温度」这类列整块错位、同一维度出现多行）；`groupBy` 按标签列分组、对 `Value #X` 取 `lastNotNull` 才能合并成一行一个维度。分组键要每个查询都存在，且涵盖所有要保留显示的标签列（`groupBy` 会丢掉没列进 `fields` 的字段）。
