# 集群域名解析仿真、诊断与节点本地缓存代理

在高峰负载下复现并量化"一次外部域名查询被搜索域放大成十几次查询、丢包时应用等 5 秒"的集群 DNS 问题，并对比以下方案：

| 方案 | 说明 |
|---|---|
| **A 无缓存** | 应用解析库直接查询集群 DNS 服务（CoreDNS 副本） |
| **B 仅节点缓存** | 每个节点一个缓存代理：正向 + 否定缓存、上游快速重试、服务变更即时失效 |
| **C 节点缓存 + 配置调整** | 在 B 之上按应用调整 `ndots`、搜索域，并对外部域名使用完整域名 |
| **D 仅 TTL 失效（诊断）** | B 但不做 watch 失效，用于量化服务变更后返回旧地址的时长 |

## 快速开始

```bash
go run ./cmd/dnsim run                 # 60 分钟高峰仿真，输出 web/report.json 和 web/index.html
go run ./cmd/dnsim run -duration 20 -out /tmp/r.json -html /tmp/i.html
go run ./cmd/dnsim explain             # 打印建模的解析规则与场景说明
open web/index.html                    # 仪表盘
```

`run` 的常用参数：

```
-seed 1            随机种子（同一配置可复现）
-duration 60       仿真分钟数
-apps 120          应用数
-namespaces 60 -services 40   命名空间数与每命名空间服务数
-nodes 200 -containers 15     节点数与每节点容器数
-coredns-replicas 4 -coredns-capacity 12000  集群 DNS 副本数与单副本每秒查询容量
-peak-qps 40       高峰期单应用每秒解析次数
-loss 0.0015       高峰丢包率
-ndots 5           默认 nadots
-client glibc      客户端解析库策略（glibc / go）
```

## 交付内容

### 1. 客户端解析库行为（`internal/resolv`）

按 `resolv.conf` 精确实现：

- 结尾带点 → 只查该名字（rooted）；
- 点数 ≥ `ndots` → 先查原名，再依次拼接搜索域；
- 点数 < `ndots` → 先依次拼接搜索域，最后查原名；
- 拼接后超过 254 字节跳过；最多 6 个搜索域、3 个 nameserver；
- A 与 AAAA 默认并行（`single-request` 时串行），可配置 IPv4-only；
- 每个候选名按 `attempts × nameserver` 重试，单次超时 `timeout`；
- 失败语义：NXDOMAIN/NOERROR-NoData/SERVFAIL 继续下一个候选，丢包重试耗尽后放弃（h_errno TRY_AGAIN）。

这些规则**用本机真实 libresolv 的 `res_search` 对拍**：`internal/oracle` 编译一个 C harness，把 nameserver/端口/ndots/search/timeout/retry 全部覆盖后向一个记录型 UDP 服务器发起查询，逐条比较查询序列；`internal/livezone` 把仿真集群的权威区用真实 UDP 提供出来，让 libresolv 和本仿真解析器查询同一个活区并比对候选序列。见 `docs/resolver-notes.md`。

### 2. 查询放大与无效查询统计（`internal/sim`、`internal/report`）

- 每个应用记录：解析次数、实际发出的查询数、放大倍数、**无效查询**（必然失败的搜索域拼接，用独立的"名字能否存在"判定器分类）、超时次数、时延分位数、缓存命中率；
- 按域名聚合，给出放大最严重的应用与域名。

### 3. 节点本地缓存代理（`internal/sim/cache.go`）

- 每节点一个缓存，按 (name,type) 键；
- 正向结果按 min(记录 TTL, 配置上限) 缓存；**否定结果单独控制 TTL**（默认 5s）；
- SERVFAIL 不缓存，避免延长故障；
- 同键并发请求合并；
- **上游丢包时按 200–400ms 短超时快速重试**（默认 2 次），而不是让应用等自己的 5 秒超时；
- 每个节点以 `node.IP:5353` 的 node-local 监听形式提供缓存（模型中以地址标识）。

### 4. 缓存失效

- 集群内服务：监听服务变更事件（watch），变更后按传播延迟（默认 15ms）使对应记录失效；缓存条目带服务版本号，已刷新的条目不会被误删；
- 外部域名：只按记录的 TTL 过期；缓存取 min(记录 TTL, 配置上限)，不延长记录寿命。

### 5. 配置建议（`sim.Suggest`、仪表盘"配置建议"）

按访问模式给出：`ndots:2`、删除从未命中的搜索域（如 `corp.internal`）、对外部域名使用带结尾点的 FQDN、跨命名空间调用写全限定名；并给出调整前后的查询/解析对比。

## 验收指标

`dnsim run` 的文本汇总与仪表盘首屏直接给出四种方案的：

- 集群解析服务收到的查询量（峰值时段）
- 应用解析时延 p50 / p95 / p99 / max
- 5 秒超时（停顿）发生次数
- 服务变更后缓存返回旧地址的时长（B 由 watch 延迟决定，D 由 TTL 决定）
- 缓存命中率、否定缓存命中、无效查询占比

测试：

```bash
go test ./...          # 解析规则、libresolv 对拍、活区对拍、仿真不变量
go test ./internal/oracle -run TestSearchExpansion -v
```

没有 C 编译器时 libresolv 对拍会自动跳过；其余测试仍然运行。

## 建模与简化（重要）

- **虚拟时间、单线程离散事件**：数分钟高峰、上千万查询在数秒到一分钟内跑完，同一 seed 完全可复现。
- **CoreDNS**：固定副本、每副本每秒容量、超过容量按比例丢包（UDP/conntrack 溢出）、队列延迟超过 40ms 视为丢弃。这样客户端看到的时延会停在应用超时附近，而不是无限增长。
- **应用时延**：应用有自己的截止时间（默认 5s，即验收中的"五秒超时"）；解析库内部可以继续重试多个搜索候选，但应用在截止时间到达时放弃并记录一次失败解析。
- **外部域名**：集群 DNS 对它们仍消耗副本容量（真实场景如此），记录 TTL 在 30–300s 内随机。
- 有意简化：不仿真 TCP fallback、DNSSEC、EDNS 大小；CoreDNS 的上游递归解析用固定 RTT 代替；服务变更事件是合成的。

## 目录结构

```
cmd/dnsim            命令行入口（run / explain）
internal/engine      虚拟时钟与事件队列
internal/resolv      客户端解析库状态机（搜索域/ndots/超时重试/地址族）
internal/dnswire     DNS 报文编解码（供活区与对拍使用）
internal/oracle      libresolv C harness + 记录型 UDP 服务器
internal/livezone    把仿真集群权威区用真实 UDP 提供出来
internal/sim         集群拓扑、应用、CoreDNS、网络、节点缓存、指标
internal/report      JSON 报告、文本汇总、HTML 仪表盘
docs/                解析规则验证记录
```
