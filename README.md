# ipwatcher

长期运行的公网出口 IP 采集与网段分析工具。IPv4 为主，IPv6 尽力采集。

定时查询公网 IP，写入本地 SQLite，统计 IP 变更历史、观察网段分布与生命周期，辅助管理员决定防火墙 CIDR 白名单（本工具**不会**自动修改防火墙）。

## 功能

| 命令 | 说明 |
|------|------|
| `ipwatcher run` | 启动长期采集（默认每 5 分钟一轮） |
| `ipwatcher status` | 当前 IPv4/IPv6、最近检查/变更、持续时长（`-json`） |
| `ipwatcher last` | 脚本友好的 one-shot 当前 IP |
| `ipwatcher history` | IP 变更历史（含 FAMILY 列，`-json`） |
| `ipwatcher analyze` | /24–/20（v4）与 /64–/32（v6）观察网段分布与生命周期（`-json`） |
| `ipwatcher purge` | 按 IP 或 CIDR 清理脏数据（`-dry-run`） |
| `ipwatcher check` | 逐个探测 Provider，报告 OK/FAIL/IGNORED |
| `ipwatcher export` | 导出 observations/changes 为 CSV/JSON |

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
```

### 排除错误 IP / 网段

采集时永久过滤（配置 `ignore_ips`，支持单 IP 与 CIDR，v4/v6）：

```yaml
ignore_ips:
  - 154.3.34.66
  - 154.3.0.0/16
  - 2001:db8::/32
```

事后从库里清掉历史脏数据：

```bash
./ipwatcher purge -ip 154.3.34.66
./ipwatcher purge -cidr 154.3.0.0/16 -dry-run
./ipwatcher purge -ip 1.2.3.4,5.6.7.8 -cidr 10.0.0.0/8
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

```yaml
notify:
  webhook_url: https://example.com/hooks/ipwatcher
  timeout: 5s
```

变更时 POST：`{"family":"ipv4","old_ip":"...","new_ip":"...","changed_at":"..."}`。失败仅 WARN。

### Docker

镜像由 GitHub Actions 自动构建并发布到 [GitHub Container Registry](https://github.com/golimit/ipwatcher/pkgs/container/ipwatcher)，**正常情况下无需本地构建**，直接拉取即可。

**用 compose 启动（推荐）：**

```bash
docker compose up -d
# 数据持久化在 ./data/
```

查看子命令时，注意 `docker exec` 不会走镜像的 `ENTRYPOINT`，需要显式写出容器内的二进制名：

```bash
docker compose exec ipwatcher ipwatcher status
docker compose exec ipwatcher ipwatcher history
docker compose exec ipwatcher ipwatcher analyze
docker compose exec ipwatcher ipwatcher purge -ip 154.3.34.66
```

## 配置

默认值已内置；可通过 YAML 文件与环境变量覆盖。完整示例见 `configs/config.example.yaml`。

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
| `IPWATCHER_IGNORE_IPS` | 逗号分隔的 IP/CIDR（永不入库） |
| `IPWATCHER_WEBHOOK_URL` | 变更通知 URL |
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
- 表：`observations`（每次检测）与 `ip_changes`（IP 变更事件），均含 `family`（`4`/`6`）。v0.1 库会自动迁移补列并回填。
- `analyze` 输出使用「观察网段 / 候选网段」措辞。历史 IP 相近**不能**证明运营商会分配整个 `/24` 或 `/22`；建议积累至少 30–90 天数据后再决定是否扩大防火墙 CIDR。

## 开发

```bash
make          # fmt + vet + test + build
make test
make vet
make fmt
```

提交信息使用 `feat:` / `fix:` / `test:` / `docs:` / `chore:` 前缀。

## License

MIT
