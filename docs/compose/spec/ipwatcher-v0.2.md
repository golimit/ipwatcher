---
feature: ipwatcher-v0.2
status: delivered
updated: 2026-09-16
branch: feat/ipwatcher-v0.2
commits: e5eb00a..e652f55
---

# ipwatcher v0.2 — ignore/purge + IPv6 + operator tooling

## Report

**What was built** — 过滤规则升级为 IP+CIDR，并改为 **CLI 管理 + SQLite 持久化**（`ignore list/add/remove`），仓库不放真实 IP。`purge` 支持 `-ip`/`-cidr`/`-dry-run`/`--also-ignore`。IPv6 尽力采集（`providers_v6`），表结构增加 `family` 并自动迁移 v0.1。新增 `last`/`check`/`export`/`config`，`status`/`history`/`analyze` 双家族并支持 `-json`；IP 变更可 POST webhook。collector 每轮重载 DB 规则，`ignore add` 无需重启。

**Verification** — `go test ./... -count=1` 全绿；`go vet` / `gofmt` 干净。独立审查无 CRITICAL。CLI 冒烟：`ignore add/list/remove`、`purge --also-ignore`、`config`、双家族 analyze/purge/export 符合预期。

**Journey log** —
- v0.1 旧库迁移：`CREATE INDEX ... ON family` 必须在 `ADD COLUMN family` 之后。
- `netip.Addr.Is6()` 对 IPv4-mapped 为真，推断 family 需 `Is4()||Is4In6()`。
- IPv6 失败不能写 failure observation。
- 每轮 `refreshIgnoreFilter` 会覆盖 Failover 上预设的 filter，测试应通过 `ExtraIgnore`/`AddIgnoreRule` 注入。
- PowerShell 对 UTF-8 文件做 `-replace` 会损坏中文，文档类文件应用 write 整文件重写。

## [S1] Problem

v0.1 错误 IP 会永久污染 history/analyze，且无法排除。过滤只能单 IP。只采 IPv6 缺失。运维还缺 one-shot 当前 IP、Provider 检查、JSON、webhook，以及**不改仓库文件**即可管理过滤规则的能力。

## [S2] Design

### 2.1 过滤器：CLI + DB 永久规则

- `ipwatcher ignore list|add|remove`，规则存 SQLite `ignore_rules`（不进 git）。
- `purge --also-ignore`：清历史并写入永久过滤。
- collector 每轮从 DB 重载；配置 `ignore_ips` / `IPWATCHER_IGNORE_IPS` 仅作额外合并项。
- 规则支持单 IP 与 CIDR（v4/v6）。Failover 命中即跳过该 Provider。

### 2.2 purge

```
ipwatcher purge [-ip a,b] [-cidr p1,p2] [--also-ignore] [-dry-run]
```

删除 observations / ip_changes 命中行；CIDR 前缀匹配。

### 2.3 IPv6（有则记，无则忽略）

- `providers_v6` 默认空。IPv4 必查+重试；IPv6 尽力一次，失败 debug。
- 表增加 `family`；迁移先 ADD COLUMN 再建索引；按 IP 是否含 `:` 回填。
- 变更检测按 family 独立。analyze：v4 24–20，v6 64/56/48/32。

### 2.4 Operator CLI

| 命令 | 行为 |
|------|------|
| `last` / `status` / `history` / `analyze` | 双家族，支持 `-json` |
| `ignore` | list / add / remove（DB） |
| `purge` | IP/CIDR 清理，`--also-ignore` |
| `check` | Provider OK/FAIL/IGNORED |
| `export` | observations\|changes → csv\|json |
| `config` | 生效配置与 ignore 列表 |

### 2.5 Webhook

`notify.webhook_url` 或 `IPWATCHER_WEBHOOK_URL`。变更后 POST JSON；失败仅 WARN。密钥走 env，不入库。

### 2.6 配置/env

| YAML | ENV |
|------|-----|
| `providers_v6` | `IPWATCHER_PROVIDERS_V6` |
| `ignore_ips`（额外合并） | `IPWATCHER_IGNORE_IPS` |
| `notify.webhook_url` | `IPWATCHER_WEBHOOK_URL` |
| `notify.timeout` | `IPWATCHER_WEBHOOK_TIMEOUT` |

## [S3] Out of Scope

- 自动改防火墙
- Webhook 签名/鉴权
- Web UI
- 全量配置 CRUD（providers/interval 仍用 env/引导 YAML）

## Tasks

- [x] T1: IgnoreFilter（IP+CIDR）+ Failover + 配置/env
- [x] T2: Storage family 迁移 + DeleteIP/DeletePrefix + ignore_rules CRUD
- [x] T3: 双栈 Collector + providers_v6 + 每轮重载 ignore
- [x] T4: Analyzer 分家族
- [x] T5: CLI last/check/export/-json/purge/ignore/config
- [x] T6: Webhook
- [x] T7: README/example/测试全绿；仓库无真实 IP
