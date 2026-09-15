# ipwatcher

长期运行的公网出口 IPv4 采集与网段分析工具。

定时查询公网 IP，写入本地 SQLite，统计 IP 变更历史、观察网段分布与生命周期，辅助管理员决定防火墙 CIDR 白名单（本工具**不会**自动修改防火墙）。

## 功能

| 命令 | 说明 |
|------|------|
| `ipwatcher run` | 启动长期采集（默认每 5 分钟一轮） |
| `ipwatcher status` | 查看当前 IP、最近检查/变更时间、当前 IP 持续时长 |
| `ipwatcher history` | 查看 IP 变更历史 |
| `ipwatcher analyze` | 查看 /24–/20 观察网段分布与生命周期统计 |

架构：`Provider(s) → Collector(ticker) → SQLite → Analyzer → CLI`

## 快速开始

### 从源码构建

```bash
git clone <repo>
cd ipwatcher
go build -o ipwatcher ./cmd/ipwatcher

# 可选：复制示例配置
cp configs/config.example.yaml config.yaml

# 启动采集
./ipwatcher run
```

### 查看状态

```bash
./ipwatcher status
./ipwatcher history
./ipwatcher analyze
```

### Docker

```bash
docker compose up -d
# 数据持久化在 ./data/
```

## 配置

默认值已内置；可通过 YAML 文件与环境变量覆盖。

示例 `configs/config.example.yaml`：

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

环境变量（优先级高于配置文件）：

| 变量 | 说明 |
|------|------|
| `IPWATCHER_INTERVAL` | 采集间隔，如 `5m` |
| `IPWATCHER_TIMEOUT` | HTTP 超时，如 `5s` |
| `IPWATCHER_RETRIES` | 失败重试次数 |
| `IPWATCHER_RETRY_DELAY` | 重试间隔，如 `2s` |
| `IPWATCHER_DB_PATH` | SQLite 路径 |
| `IPWATCHER_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `IPWATCHER_PROVIDERS` | 逗号分隔的 Provider URL 列表 |

配置文件路径通过 `-config` 指定（默认 `./config.yaml`；文件不存在时使用内置默认值）。

## systemd 部署（推荐 Linux）

`/etc/systemd/system/ipwatcher.service`：

```ini
[Unit]
Description=ipwatcher public IPv4 collector
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
- 表：`observations`（每次检测）与 `ip_changes`（IP 变更事件）。
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
