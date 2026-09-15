# ipwatcher

长期运行的公网出口 IP 采集与网段分析工具。IPv4 为主，IPv6 尽力采集。

定时查询公网 IP，写入本地 SQLite，统计 IP 变更历史、观察网段分布与生命周期，辅助管理员决定防火墙 CIDR 白名单（本工具**不会**自动修改防火墙）。

过滤规则通过二进制命令写入数据库管理，**不要把真实 IP/CIDR 写进仓库文件**。

## 功能

| 命令 | 说明 |
|------|------|
| `ipwatcher run` | 启动长期采集（默认每 5 分钟一轮） |
| `ipwatcher status` | 当前 IPv4/IPv6、最近检查/变更、持续时长（`-json`） |
| `ipwatcher last` | 脚本友好的 one-shot 当前 IP |
| `ipwatcher history` | IP 变更历史（含 FAMILY 列，`-json`） |
| `ipwatcher analyze` | /24–/20（v4）与 /64–/32（v6）观察网段分布与生命周期（`-json`） |
| `ipwatcher ignore` | 查看/新增/删除永久过滤规则（存 SQLite） |
| `ipwatcher purge` | 按 IP 或 CIDR 清理脏数据（可 `--also-ignore`） |
| `ipwatcher check` | 逐个探测 Provider，报告 OK/FAIL/IGNORED |
| `ipwatcher export` | 导出 observations/changes 为 CSV/JSON |
| `ipwatcher config` | 查看当前生效配置（含 ignore 列表） |

架构：`Provider(s) → Collector(ticker) → SQLite → Analyzer → CLI`

## 快速开始

### 从源码构建

```bash
git clone <repo>
cd ipwatcher
go build -o ipwatcher ./cmd/ipwatcher

cp configs/config.example.yaml config.yaml
./ipwatcher run
```

### 查看状态

```bash
./ipwatcher status
./ipwatcher last
./ipwatcher history
./ipwatcher analyze
./ipwatcher check
./ipwatcher config
```

### 排除错误 IP / 网段（推荐：全部用 CLI，不改文件）

规则存在 SQLite 的 `ignore_rules` 表，运行中的 collector **下一轮自动生效**，无需重启。

```bash
# 查看
./ipwatcher ignore list
./ipwatcher ignore list -json

# 永久过滤（单 IP 或 CIDR）
./ipwatcher ignore add 203.0.113.66
./ipwatcher ignore add 203.0.113.0/24
./ipwatcher ignore add 2001:db8::/32

# 取消过滤
./ipwatcher ignore remove 203.0.113.66

# 只清这一次历史（不加入永久过滤）
./ipwatcher purge -ip 203.0.113.66
./ipwatcher purge -cidr 203.0.113.0/24 -dry-run

# 清历史 + 写入永久过滤（一键）
./ipwatcher purge -ip 203.0.113.66 --also-ignore
./ipwatcher purge -cidr 203.0.113.0/24 --also-ignore
```

### IPv6（可选）

`providers_v6` 非空时启用。IPv4 每轮必查（失败重试）；IPv6 尽力查一次，失败静默跳过。

```yaml
providers_v6:
  - https://ipv6.icanhazip.com
  - https://api6.ipify.org
  - https://6.ident.me
```

### 变更 Webhook（可选）

敏感 URL 建议用环境变量，不要提交到仓库：

```bash
export IPWATCHER_WEBHOOK_URL=https://example.com/hooks/ipwatcher
```

变更时 POST：`{"family":"ipv4","old_ip":"...","new_ip":"...","changed_at":"..."}`。失败仅 WARN。

### Docker

镜像由 GitHub Actions 自动构建并发布到 [GitHub Container Registry](https://github.com/golimit/ipwatcher/pkgs/container/ipwatcher)。

```bash
docker compose up -d
# 数据持久化在 ./data/（含 ignore_rules）
```

`docker exec` 不会走镜像的 `ENTRYPOINT`，需要显式写出容器内的二进制名：

```bash
docker compose exec ipwatcher ipwatcher status
docker compose exec ipwatcher ipwatcher ignore list
docker compose exec ipwatcher ipwatcher ignore add 203.0.113.66
docker compose exec ipwatcher ipwatcher purge -cidr 203.0.113.0/24 --also-ignore
```

## 配置

默认值已内置；YAML 仅作引导（interval/providers 等）。**永久 ignore 规则不要写在配置文件里**，用 `ipwatcher ignore` 管理。

完整示例见 `configs/config.example.yaml`。

环境变量（优先级高于配置文件）：

| 变量 | 说明 |
|------|------|
| `IPWATCHER_INTERVAL` | 采集间隔，如 `5m` |
| `IPWATCHER_TIMEOUT` | HTTP 超时，如 `5s` |
| `IPWATCHER_RETRIES` | 失败重试次数 |
| `IPWATCHER_RETRY_DELAY` | 重试间隔，如 `2s` |
| `IPWATCHER_DB_PATH` | SQLite 路径 |
| `IPWATCHER_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `IPWATCHER_PROVIDERS` | 逗号分隔的 IPv4 Provider URL |
| `IPWATCHER_PROVIDERS_V6` | 逗号分隔的 IPv6 Provider URL |
| `IPWATCHER_IGNORE_IPS` | 额外 IP/CIDR（与 DB 规则合并；一般不用） |
| `IPWATCHER_WEBHOOK_URL` | 变更通知 URL（敏感，勿入库） |
| `IPWATCHER_WEBHOOK_TIMEOUT` | Webhook 超时，如 `5s` |

配置文件路径通过 `-config` 指定（默认 `./config.yaml`；文件不存在时使用内置默认值）。

## systemd 部署（推荐 Linux）

`/etc/systemd/system/ipwatcher.service`：

```ini
[Unit]
Description=ipwatcher public IP collector
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ipwatcher
WorkingDirectory=/opt/ipwatcher
ExecStart=/opt/ipwatcher/ipwatcher run -config /opt/ipwatcher/config.yaml
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now ipwatcher
```

## 数据说明

- 库内时间一律 **UTC**；CLI 展示时转换为本地时区。
- 表：`observations`、`ip_changes`（均含 `family`）、`ignore_rules`（CLI 管理的永久过滤）。
- v0.1 库会自动迁移补列并回填。
- `analyze` 输出使用「观察网段 / 候选网段」措辞。历史 IP 相近**不能**证明运营商会分配整个 `/24` 或 `/22`；建议积累至少 30–90 天数据后再决定是否扩大防火墙 CIDR。

## 开发

```bash
make          # fmt + vet + test + build
make test
make vet
make fmt
```

提交信息使用 `feat:` / `fix:` / `test:` / `docs:` / `chore:` 前缀。

**仓库内禁止提交真实公网 IP/CIDR**；测试与文档一律使用 RFC 5737 / RFC 3849 文档地址（`203.0.113.0/24`、`198.51.100.0/24`、`2001:db8::/32`）。

## License

MIT
