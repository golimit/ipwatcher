---
feature: ipwatcher
status: delivered
updated: 2026-09-16
branch: feat/ipwatcher-v0.1.0
commits: 449a4fe..c3aa3c3
---

# ipwatcher — 动态出口 IP 采集与网段分析工具

## Report

**What was built** — 首版可长期运行的公网出口 IPv4 采集 CLI：`run` 按 interval（默认 5m）轮询多 Provider（failover + 重试），成功/失败均写入 SQLite `observations`，IP 变化写入 `ip_changes`；SIGINT/SIGTERM 停止新轮次并用 `context.WithoutCancel` 完成在途写入。`status` / `history` / `analyze` 分别输出当前状态、变更历史、/24–/20 观察网段分布与已结束生命周期统计。配置支持 YAML + `IPWATCHER_*` 环境变量；时间库内 UTC、展示本地时区；IP/CIDR 一律 `net/netip`。

**Verification** — `gofmt -l .` 空；`go vet ./...` 干净；`go test ./... -count=1` 全绿（analyzer/collector/config/provider/storage）。独立审查（449a4fe..d2517ff）：Spec PASS、Consistency PASS、Critical none；Correctness 指出 4 项已在 c3aa3c3 修复（优雅写完在途记录、latency 不含重试等待、YAML `retries: 0` 生效、未结束 IP 段不计入 min/max/avg）。本地 CLI 冒烟：假 Provider 下 `run`/`status`/`history`/`analyze` 输出符合 plan §11。

**Journey log** — 
- `.gitignore` 中的 `ipwatcher` 误匹配了 `cmd/ipwatcher/` 源码目录，已改为 `/ipwatcher` 仅根二进制。
- YAML 零值合并不能区分「未写」与「显式 0」，改为指针化 `fileConfig` 解析。
- 优雅退出不能复用被 cancel 的主 ctx 做 DB 写入，需 `context.WithoutCancel`。
- 生命周期统计应只累计已切换完成的 IP 段，开放中的当前段会把 Min 拉成 0。
- 审查子代理两次卡住：范围收窄（只 diff 修复提交）后仍可能无响应时，应改为对照代码逐项核验 + 测试复跑，而不是无限等待。

## [S1] Problem

家庭/公司宽带出口 IPv4 随拨号、重启、租约更新而变化。手工维护防火墙 /32 白名单成本高，且无法反推运营商地址池范围与 IP 更换周期。需要一个长期运行的采集工具：定期获取公网 IP、落库、统计变化与网段分布，为管理员提供候选 CIDR 决策依据（不自动改防火墙）。

## [S2] Design

### 架构

```
Provider(s) → Collector(ticker) → Storage(SQLite) → Analyzer → CLI 输出
```

- 单进程、主循环 + Ticker + HTTP + SQLite；第一版不做复杂并发。
- 业务逻辑全部在 `internal/`；`cmd/ipwatcher/main.go` 只做参数解析与启动。

### CLI 命令

| 命令 | 行为 |
|------|------|
| `ipwatcher run` | 长期监控：按 interval 轮询，写 observation；检测到 IP 变化时写 ip_changes |
| `ipwatcher status` | 当前 IP、最近检查时间、最近变更时间、当前 IP 持续时长 |
| `ipwatcher history` | 变更历史表：TIME / OLD IP / NEW IP |
| `ipwatcher analyze` | 观察周期、变更次数、唯一 IP 数、/24–/20 分布百分比、生命周期统计 |

### Provider 接口

```go
type Provider interface {
    Name() string
    Lookup(ctx context.Context) (netip.Addr, error)
}
```

- 默认内置：ipify、ipv4.icanhazip.com、4.ident.me（HTTPS，返回纯文本 IPv4）。共享 `http.Client` 强制 `tcp4` 拨号，避免双栈网络返回出口 IPv6。
- Failover：按配置顺序依次尝试；单次失败→下一 Provider；全部失败记 WARN 并等下一周期。
- 必须用 `net/netip` 校验合法 IPv4；仅接受 IPv4。

### 请求策略

- 超时 5s（`http.Client.Timeout` + per-request context）
- 失败重试 2 次，重试间隔 2s
- 任一次网络失败不导致进程退出

### Storage（SQLite）

Driver：`modernc.org/sqlite`（纯 Go，无 CGO）。

```sql
CREATE TABLE IF NOT EXISTS observations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  observed_at TEXT NOT NULL,   -- RFC3339 UTC
  ip TEXT,
  provider TEXT,
  success INTEGER NOT NULL,
  latency_ms INTEGER,
  error TEXT
);
CREATE INDEX IF NOT EXISTS idx_obs_time ON observations(observed_at);
CREATE INDEX IF NOT EXISTS idx_obs_ip ON observations(ip);

CREATE TABLE IF NOT EXISTS ip_changes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  changed_at TEXT NOT NULL,    -- RFC3339 UTC
  old_ip TEXT NOT NULL,
  new_ip TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chg_time ON ip_changes(changed_at);
```

- 库内时间一律 UTC RFC3339；展示时转本地时区。
- 访问集中在 `internal/storage`，方法接受 `context.Context`。

### Collector

- Ticker 按 `interval`（默认 5m）触发。
- 每轮：按序调用 Provider → 校验 IPv4 → 与上次成功 IP 比较。
  - 相同：写 success observation。
  - 不同：写 success observation + ip_changes 行，INFO 日志 `public ip changed old=... new=...`。
  - 全失败：写 failure observation（error 字段），WARN 日志，等待下一周期。
- SIGINT/SIGTERM：停止新轮次 → 等当前写入完成 → 关闭 DB → 退出。

### Analyzer

- `/24 /23 /22 /21 /20` 分布：对历史成功 observation 的唯一 IP 做 `netip.Prefix` 归并，统计各 prefix 下 IP 数与占比。
- 生命周期：由 `ip_changes` + 首末观测时间推算平均/最短/最长使用时长与更换次数。
- 输出用语必须是「观察网段 / 候选网段」，禁止暗示「运营商确定分配此段」。
- 建议文案：数据不足 30 天时提示仅供参考。

### Config

YAML 文件 + 环境变量（`IPWATCHER_*` 覆盖同名键）。

```yaml
collector:
  interval: 5m
  timeout: 5s
  retries: 2
  retry_delay: 2s
database:
  path: ./data/ipwatcher.db
logging:
  level: info   # debug|info|warn|error
providers:
  - https://api.ipify.org
  - https://ipv4.icanhazip.com
  - https://4.ident.me
```

- 默认值内置于代码；配置文件可选；环境变量可选。

### Logger

标准库 `log/slog`，文本 handler 到 stderr。级别：debug/info/warn/error。

### 工程规范

- IP/CIDR 一律 `net/netip`，禁止 split(".")
- HTTP 全局复用一个 `http.Client`
- `gofmt` + `go vet` + `go test` 必须通过
- 提交信息使用 `feat:` / `fix:` / `test:` / `docs:` / `chore:` 前缀

## [S3] Out of Scope

- WebUI / Web API / Prometheus / Webhook / 通知
- IPv6、ASN/WHOIS
- 自动修改防火墙
- 多 Provider 并发一致性校验（第一版串行 failover）

## Tasks

- [x] T1: 项目脚手架（go.mod、目录、Makefile 骨架） — acceptance: `go mod tidy` 成功，目录符合 plan §13 (covers: S2)
- [x] T2: config 包（YAML + env 覆盖 + 默认值） — acceptance: 单元测试证明默认值、文件覆盖、env 覆盖 (covers: S2)
- [x] T3: logger 包（slog 初始化） — acceptance: 按 level 过滤 (covers: S2)
- [x] T4: provider 包（接口 + 三源 + failover + IPv4 校验） — acceptance: httptest 覆盖成功/500/非法/超时/空响应 (covers: S2)
- [x] T5: storage 包（Open/迁移/InsertObservation/InsertChange/LatestSuccess/Changes/UniqueIPs） — acceptance: 临时库写入后可读回 (covers: S2)
- [x] T6: collector 包（ticker 循环、变更检测、优雅退出） — acceptance: 注入 fake provider/存储后可跑一轮并触发变更 (covers: S2)
- [x] T7: analyzer 包（prefix 分布 + 生命周期） — acceptance: 固定 IP 集合下 /24//23//22 统计正确 (covers: S2)
- [x] T8: CLI 集成（run/status/history/analyze） — acceptance: 四条子命令可执行并输出 plan §11 对应格式 (covers: S2)
- [x] T9: 工程文件（Dockerfile、docker-compose、README、config.example.yaml） — acceptance: 文件齐全且内容可部署 (covers: S2)
- [x] T10: 全量验证 gofmt/vet/test — acceptance: `gofmt -l .` 空、`go vet ./...` 干净、`go test ./...` 全绿 (covers: S2)
