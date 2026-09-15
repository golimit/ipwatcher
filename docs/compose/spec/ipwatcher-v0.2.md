---
feature: ipwatcher-v0.2
status: delivered
updated: 2026-09-16
branch: feat/ipwatcher-v0.2
commits: e5eb00a..HEAD
---

# ipwatcher v0.2 — ignore/purge + IPv6 + operator tooling

## Report

**What was built** — 过滤器升级为 IP+CIDR（v4/v6），Failover 命中即跳过；`purge` 支持 `-ip`/`-cidr`/`-dry-run` 事后清理脏数据。IPv6 尽力采集（`providers_v6`，失败静默），表结构增加 `family` 并自动从 v0.1 迁移回填。新增 `last`/`check`/`export`，`status`/`history`/`analyze` 双家族并支持 `-json`；IP 变更可 POST webhook。

**Verification** — `go test ./... -count=1` 全绿；`go vet ./...` 干净；`gofmt -l .` 空。独立审查：Spec PASS、Correctness 无 CRITICAL。CLI 冒烟：seeded 库上 `last`/`status`/`history`/`analyze`/`purge -cidr`/`export`/`check` 行为符合预期；purge `154.3.0.0/16` 删除 1 observation + 1 change。

**Journey log** —
- v0.1 旧库迁移时 `CREATE INDEX ... ON family` 必须在 `ADD COLUMN family` 之后，否则 Open 直接失败。
- `netip.Addr.Is6()` 对 IPv4-mapped 为真，推断 family 需 `Is4()||Is4In6()`。
- IPv6 失败不能写 failure observation，否则双栈无 v6 的主机会刷屏。
- 审查指出的验收测试缺口（全忽略 failover、v0.1 迁移、混族 analyze、webhook 失败）补测后发现并修掉真实迁移 bug。

## [S1] Problem

v0.1 采集结果里一旦出现错误 IP（captive portal、Provider 劫持、历史脏数据），会永久污染 `history` / `analyze`，无法排除或清理。过滤只能针对单 IP，而错误结果经常是一整段网段。同时本工具只采 IPv4，双栈出口的 IPv6 无法记录。脚本化场景还缺 one-shot 当前 IP、Provider 健康检查、JSON 输出与变更 webhook。

## [S2] Design

### 2.1 过滤器：单 IP + CIDR

- 配置 `ignore_ips`：接受 `A.B.C.D` / `x::y` / `A.B.C.D/len` / `x::y/len`。
- 解析为 `[]netip.Prefix`（单 IP 自动 /32 或 /128）。
- Failover：命中过滤器的 Provider 结果视为失败并尝试下一个；全部命中则本轮记失败 observation（error 含 “ignore list”）。
- 环境变量：`IPWATCHER_IGNORE_IPS`（逗号分隔）。

### 2.2 purge：事后清理

```
ipwatcher purge [-ip a,b] [-cidr p1,p2] [-dry-run]
```

- 至少提供 `-ip` 或 `-cidr` 之一。
- 删除 `observations.ip` 与 `ip_changes.old_ip/new_ip` 命中的行。
- CIDR 按前缀匹配删除命中的 observation IP，并清理引用这些 IP 的 change 行。

### 2.3 IPv6 采集（有则记，无则忽略）

- 配置 `providers_v6`（默认空 → 不采 IPv6，向后兼容）。
- 双 HTTP client：`tcp4` / `tcp6`。
- 每轮：IPv4 必查（重试策略不变）；IPv6 尽力查一次，失败只 debug，不写 failure observation、不退出。
- 表增加 `family TEXT NOT NULL DEFAULT '4'`（`observations`、`ip_changes`）；迁移先 `ALTER TABLE ADD COLUMN` 再建 family 索引，并按 IP 是否含 `:` 回填。
- 变更检测按 family 独立：v4 只与 v4 比，v6 只与 v6 比。
- `analyze` 分家族输出：v4 前缀 24–20；v6 前缀 64/56/48/32。

### 2.4 Operator CLI

| 命令 | 行为 |
|------|------|
| `ipwatcher last` | one-shot 打印库中当前 IPv4（及 IPv6 若有）；`-json` |
| `ipwatcher check` | 逐个探测 providers / providers_v6，报告 OK / FAIL / IGNORED |
| `ipwatcher status` | 双家族当前 IP、最近检查、各自最近变更；`-json` |
| `ipwatcher history` | 增加 FAMILY 列；`-json` |
| `ipwatcher analyze` | 分家族；`-json` |
| `ipwatcher export` | `-table observations\|changes` `-format csv\|json` `[-out file]` `[-limit N]`（limit 为最近 N 条，输出按时间正序） |
| `ipwatcher purge` | 见 2.2 |

### 2.5 Webhook 通知

```yaml
notify:
  webhook_url: https://example/hook
  timeout: 5s
```

IP 变更后 POST JSON：`{"family":"ipv4","old_ip":"...","new_ip":"...","changed_at":"..."}`。失败仅 WARN，不影响采集。

### 2.6 配置/env 增量

| YAML | ENV |
|------|-----|
| `ignore_ips` | `IPWATCHER_IGNORE_IPS` |
| `providers_v6` | `IPWATCHER_PROVIDERS_V6` |
| `notify.webhook_url` | `IPWATCHER_WEBHOOK_URL` |
| `notify.timeout` | `IPWATCHER_WEBHOOK_TIMEOUT` |

## [S3] Out of Scope

- 自动改防火墙
- Webhook 签名/鉴权
- Web UI
- Provider 响应缓存

## Tasks

- [x] T1: IgnoreFilter（IP+CIDR）+ Failover 过滤 + 配置/env/Validate — acceptance: 单测覆盖解析、CIDR 命中、全忽略失败路径 (covers: S2.1)
- [x] T2: Storage family 迁移 + DeleteIP/DeletePrefix + family 查询 — acceptance: 旧库自动加列回填；purge 单测 (covers: S2.2, S2.3)
- [x] T3: 双栈 Collector + providers_v6 + ParseIPv6 — acceptance: 仅 IPv4 配置行为不变；IPv6 成功写入/失败静默 (covers: S2.3)
- [x] T4: Analyzer 分家族 — acceptance: 混合库分别输出 v4/v6 前缀分布 (covers: S2.3)
- [x] T5: CLI last/check/export/-json/purge-cidr/status 双家族 — acceptance: 各命令冒烟 + JSON 可解析 (covers: S2.2, S2.4)
- [x] T6: Webhook — acceptance: 变更后收到 JSON，失败不中断 (covers: S2.5)
- [x] T7: README/example config/Makefile/测试全绿 — acceptance: go test ./... 通过 (covers: S2)
