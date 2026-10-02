# Relay-Api

OpenAI 兼容的 API 中转网关，后端使用 Go 实现，前端管理界面以 `go:embed` 内嵌进单个可执行文件，运行时无外部依赖（除 SQLite 数据文件外）。

与参考实现（`mini-api-gateway`，Bun + Hono）相比的核心差异：**只监听一个端口**，同一端口同时接受 HTTP/1.1 与明文 HTTP/2（h2c），因此没有 `H2C_PORT` 这个额外端口。

## 功能

- OpenAI 兼容接口：`/v1/models`、`/v1/chat/completions`，支持 SSE 流式响应
- 多上游管理：provider / model / model-route（优先级 + 故障转移）配置
- API Key 鉴权与限流，密钥仅存加密值（AES-GCM）
- 非 AI 场景转发：`/open/*`（需 API Key 的 JSON 转发）、`/free/*`（公开的原始流式隧道）
- 内嵌管理后台：用户、模型、密钥、配置、用量统计、审计日志、登录限流
- 审计与用量落库，按 `LOG_RETENTION_DAYS` 每日定时清理
- 单端口 HTTP/1.1 + h2c，`SIGINT`/`SIGTERM` 优雅退出

## 快速开始

```bash
# 编译（静态二进制，无需 CGO）
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o relay-api .

# 运行，首次启动会自动建库、迁移并创建默认管理员
./relay-api
```

启动后访问：

- 管理界面：<http://localhost:5630/>
- 健康检查：<http://localhost:5630/health>

默认管理员账号来自 `ADMIN_DEFAULT_USERNAME` / `ADMIN_DEFAULT_PASSWORD`（默认 `admin` / `admin123`），**首次登录后请立即修改密码**。已存在的账号密码不会被启动流程覆盖。

首次启动还会在 `data/.secret-key` 生成随机的密钥加密密钥（base64，32 字节），用于加密库中保存的上游 API Key。该文件必须与数据库一起持久化，丢失后已加密的密钥将无法解密。

## 环境变量

全部配置通过环境变量注入，程序不读取 `.env` 文件（可用 shell、systemd、Docker 等注入）。整数类变量为空、非法或非正数时回退到默认值。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `5630` | 监听端口，同一端口服务 HTTP/1.1 与 h2c |
| `DATABASE_PATH` | `./data/gateway.db` | SQLite 数据库路径，父目录会自动创建；使用 WAL 模式 |
| `ADMIN_TOKEN` | 空 | 管理后台 API 的 Bearer Token；设置后可直接调用 `/admin/*` 资源接口 |
| `ADMIN_DEFAULT_USERNAME` | `admin` | 首次启动创建的管理员用户名 |
| `ADMIN_DEFAULT_PASSWORD` | `admin123` | 首次启动创建的管理员初始密码 |
| `ADMIN_SESSION_TTL_HOURS` | `24` | 管理后台会话有效期（小时） |
| `ADMIN_LOGIN_RATE_LIMIT` | `10` | 单 IP 每分钟允许的失败登录次数，超出后该 IP 被限流 |
| `LOG_LEVEL` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `REQUEST_SIZE_LIMIT_MB` | `10` | `/v1/*` 与 `/open/*` 的请求体大小上限（MB）；`/free/*` 不受此限制 |
| `REQUEST_TIMEOUT_MS` | `120000` | 非流式聊天请求与 `/open/*` 转发的上游超时（毫秒） |
| `STREAM_IDLE_TIMEOUT_MS` | `60000` | SSE 流无数据块的最长空闲时间（毫秒），超时主动断开 |
| `LOG_RETENTION_DAYS` | `7` | 用量/审计日志保留天数；设为 `0` 关闭清理任务 |
| `LOG_CLEANUP_TIME` | `03:00` | 每日清理任务的本地时间（`HH:MM`），格式非法时回退到 `03:00` |
| `SECRET_ENCRYPTION_KEY` | 空 | 32 字节 base64 密钥，优先级高于密钥文件；`openssl rand -base64 32` 可生成 |
| `SECRET_KEY_FILE` | `data/.secret-key` | 密钥文件路径；文件不存在时自动生成（权限 `0600`） |

示例：

```bash
PORT=8080 LOG_LEVEL=debug DATABASE_PATH=/var/lib/relay-api/gateway.db \
SECRET_KEY_FILE=/var/lib/relay-api/.secret-key ./relay-api
```

## 路由概览

| 路径 | 鉴权 | 说明 |
| --- | --- | --- |
| `/` | 无 | 内嵌管理界面（静态资源） |
| `/health` | 无 | 健康检查，返回 `{"status":"ok"}` |
| `/v1/models`、`/v1/chat/completions` | API Key | OpenAI 兼容接口，受请求体大小限制 |
| `/open/*` | API Key | 非 AI 配置的 JSON 转发，按配置的 mount 挂载 |
| `/free/*` | 无（按配置内限额） | 公开的原始流式隧道，请求体直接透传 |
| `/admin/auth/*` | 登录会话 | 登录、登出、当前用户、改密、重置、用户管理 |
| `/admin/*` | 登录会话 / `ADMIN_TOKEN` | 统计、用量、日志、provider、模型、model-route、API Key、API 配置 |

## 目录结构

```
.
├── main.go                  # 程序入口：装配中间件、路由、h2c 服务与优雅退出
├── go.mod / go.sum
├── task.md                  # 原始任务说明
├── 实现方案.md               # 详细实现方案与验收清单
├── internal/
│   ├── config/              # 环境变量解析、默认值与前向路由常量
│   ├── db/                  # SQLite 打开、迁移、建表与全部数据访问
│   │   ├── migrate.go       #   基线 schema（与参考实现保持一致，可复用旧库）
│   │   ├── resources.go     #   provider / model / api_key / api_config 读写
│   │   ├── routing.go       #   model-route 解析与优先级选择
│   │   ├── forward.go       #   前向配置与挂载查询
│   │   ├── usages.go        #   用量记录
│   │   └── stats.go         #   统计聚合与日志清理
│   ├── httperr/             # 统一 JSON 错误响应
│   ├── middleware/          # 管理员鉴权、API Key 鉴权、请求体限制、限流、请求日志
│   ├── providers/           # 上游 provider 适配（OpenAI / Anthropic）
│   ├── router/              # 按模型名 + 优先级 + 故障转移选择上游
│   ├── routes/              # HTTP 处理函数：网关、聊天、转发、隧道、后台资源、登录
│   └── util/                # 密钥加密、登录挑战、SSE、SSRF 防护、路径模板、密码哈希等
├── web/                     # 内嵌前端（go:embed）
│   ├── embed.go
│   ├── index.html
│   ├── js/core|tabs/        # 前端模块（API 封装、鉴权、各后台页签）
│   ├── style/               # CSS
│   └── favicon.svg
└── data/                    # 运行时生成：SQLite 库、WAL 文件、.secret-key（已 gitignore）
```

## 开发

常用命令已固定在 `Makefile`（`make help` 查看全部目标）：

```bash
make build        # 静态编译到 ./relay-api
make run          # 本地启动（make run ENV="PORT=8080 LOG_LEVEL=debug"）
make all          # 编译 + 测试，提交前跑这个
make test         # 运行测试（make race 带竞态检测，make cover 出覆盖率报告）
make vet fmt-check # 静态检查与格式检查
make clean        # 删除二进制与覆盖率文件
```

前端为原生 ES Module，无构建步骤；修改 `web/` 后重新 `go build` 即可生效。

### 发布打包

```bash
make release                          # 交叉编译全部平台到 dist/，并生成 SHA256SUMS
make release VERSION=v1.0.0           # 指定版本号（默认取 git describe，无 git 时回退 dev）
make release PLATFORMS="linux/amd64"  # 只构建指定平台
make clean-dist                       # 删除 dist/
```

默认平台为 linux / darwin / windows × amd64 / arm64，产物命名 `relay-api-<version>-<os>-<arch>`：Unix 打包为 `.tar.gz`，Windows 在有 `zip` 命令时打包为 `.zip`（否则回退 `.tar.gz`），并生成 `SHA256SUMS` 校验文件。版本号通过 `-ldflags "-X main.version=..."` 注入，启动日志会打印该版本号。

## 部署注意

- `CGO_ENABLED=0` 静态编译（SQLite 驱动为纯 Go 的 `modernc.org/sqlite`）
- 持久化 `DATABASE_PATH` 指向的目录，并同时保留 `SECRET_KEY_FILE`，二者需一起备份
- 服务器层刻意不设置 `WriteTimeout`/`ReadTimeout`（长连接 SSE 与 `/free` 隧道需要），空闲控制由 `STREAM_IDLE_TIMEOUT_MS` 在处理器内完成
- 反向代理需允许长连接并关闭响应缓冲（Nginx 参考：`proxy_buffering off;`），否则流式响应会被缓冲
