# Relay-Api

OpenAI 兼容的 API 中转网关，后端使用 Go 实现，前端管理界面以 `go:embed` 内嵌进单个可执行文件，运行时无外部依赖（除 SQLite 数据文件外）。

与参考实现（`mini-api-gateway`，Bun + Hono）相比的核心差异：**只监听一个端口**，同一端口同时接受 HTTP/1.1 与明文 HTTP/2（h2c），因此没有 `H2C_PORT` 这个额外端口。

## 功能

- OpenAI 兼容接口：`/v1/models`、`/v1/chat/completions`、`/v1/responses`，支持 SSE 流式响应
- `/v1/responses` 兼容层：把 Responses 请求（Codex CLI 默认使用的 `wire_api = "responses"`）转换为内部 Chat Completions 管线，并回传 Responses 输出项与事件（文本、推理、function/custom 工具调用）
- 多上游管理：provider / model / model-route（优先级 + 故障转移 + 指数退避重试 + 熔断）配置
- Provider 协议适配：`openai` / `compatible`（Chat Completions）、`anthropic`（Messages）、`openai-responses`（Responses API）
- Provider 自定义请求头：`${客户端header}` 引入客户端值，`${_ai_session}` 引入解析出的用户会话标识
- API Key 鉴权与限流，密钥仅存加密值（AES-GCM）
- 非 AI 场景转发：`/open/*`（需 API Key 的 JSON 转发）、`/free/*`（公开的原始流式隧道）
- 内嵌管理后台：用户、模型、密钥、配置、用量统计、审计日志、登录限流
- 全局 IP 黑白名单：支持 IPv4/IPv6 单地址与 CIDR 网段，设置页保存后立即生效，回环地址始终放行
- 审计与用量落库，按 `LOG_RETENTION_DAYS` 每日定时清理；用量日志记录调用方 IP（兼容 `CF-Connecting-IP`），可点开详情查看客户端与上游请求头（敏感头打码）
- 单端口 HTTP/1.1 + h2c，`SIGINT`/`SIGTERM` 优雅退出

### Provider 类型

| 类型 | 上游请求 | 说明 |
| --- | --- | --- |
| `openai` / `compatible` | `<baseUrl>/chat/completions` | 标准 Chat Completions，`compatible` 与 `openai` 完全相同 |
| `anthropic` | `<baseUrl>/messages` | Anthropic Messages，自动转换 OpenAI 请求/响应及 SSE |
| `openai-responses` | `<baseUrl>/responses` | OpenAI Responses API，自动转换文本、图片、工具调用、推理内容和 SSE |
| `responses` | `<baseUrl>/responses` | Responses 原生透传：`/v1/responses` 的请求体、响应体与 SSE 原样转发，不做 Chat 转换，速度最优 |

例如 OpenCode Go 的 GPT / Muse 模型只提供 Responses API：Provider 的 `baseUrl` 填 `https://opencode.ai/inference/go/openai/v1`，类型选 `openai-responses`；DeepSeek 等 Chat Completions 模型继续使用 `openai` / `compatible`。

Codex CLI 可以直接使用 Responses 协议：`base_url = "https://<relay>/v1"`、`wire_api = "responses"`，请求会命中本服务的 `/v1/responses`。若该模型路由的 Provider 类型是 `responses`，请求和 SSE 会直接透传到上游；其他类型则自动转换为 Chat Completions 管线。

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
| `MAX_OPEN_FILES` | `65535` | 启动时把软 `RLIMIT_NOFILE` 提到该值（受硬上限约束；Linux/macOS 生效）。每条 `/free` 隧道占用 2 个 fd，默认软上限 1024 约 500 条隧道就会耗尽 |
| `REQUEST_SIZE_LIMIT_MB` | `10` | `/v1/*` 与 `/open/*` 的请求体大小上限（MB）；`/free/*` 不受此限制 |
| `REQUEST_TIMEOUT_MS` | `120000` | 非流式聊天请求与 `/open/*` 转发的上游超时（毫秒） |
| `STREAM_IDLE_TIMEOUT_MS` | `60000` | SSE 流无数据块的最长空闲时间（毫秒），超时主动断开 |
| `UPSTREAM_MAX_RETRIES` | `1` | 单个路由遇到可重试错误（网络错误/超时/408/429/5xx）后的额外重试次数；`0` 关闭重试 |
| `UPSTREAM_RETRY_BASE_MS` | `250` | 首次重试的退避基数（毫秒），之后按 2 倍递增 |
| `UPSTREAM_RETRY_MAX_MS` | `4000` | 单次退避的上限（毫秒） |
| `UPSTREAM_HEADER_TIMEOUT_MS` | `30000` | 流式请求等待上游响应头的上限（毫秒）；超时按上游超时处理并触发重试/熔断，`0` 关闭 |
| `UPSTREAM_BREAKER_THRESHOLD` | `5` | 同一 Provider 连续失败多少次后熔断；`0` 关闭熔断 |
| `UPSTREAM_BREAKER_COOLDOWN_MS` | `30000` | 熔断持续时间（毫秒），到期后放行一个探测请求 |
| `ACCESS_LOG_SKIP_PREFIXES` | `/free` | 不写访问日志的路径前缀（逗号分隔）；请求 ID 仍会生成，设为空可记录所有请求 |
| `TUNNEL_USAGE_SAMPLE` | `10` | `/free` 成功请求写入用量日志的采样率，`N` 表示每 N 条记录 1 条；错误请求始终记录 |
| `TUNNEL_COPY_BUFFER_KB` | `64` | `/free` 转发时每响应占用的拷贝块大小（KiB，会被夹在 32 KiB~1 MiB 之间）。乘以并发隧道数即为常驻内存；高带宽长连接部署可调到 `128`，配合 `GOMEMLIMIT` 使用 |
| `H2C_MAX_CONCURRENT_STREAMS` | `2048` | 每条 h2c/h2 连接向客户端公告的最大并发流数（连接级 SETTINGS，仅对新连接生效，修改需重启）。它不是按 API Config 配置的：一条连接上所有配置共用同一上限；超限的流会被 `REFUSED_STREAM`，由客户端重试，且每条流都可能占用上传缓冲与拷贝块 |
| `USAGE_HEADER_SNAPSHOTS` | `true` | 是否在用量日志中保存请求/上游 header 快照；设为 `false` 可减少 CPU 开销与数据库体积 |
| `LOG_RETENTION_DAYS` | `7` | 用量/审计日志保留天数；`0` 表示永久保留。周/月配额不依赖日志（见“配额与日志保留”），可以放心设短 |
| `LOG_CLEANUP_TIME` | `03:00` | 每日自动清理的本地时间（`HH:MM`）；进程启动时也会补跑一次，避免频繁重启错过清理 |
| `SECRET_ENCRYPTION_KEY` | 空 | 32 字节 base64 密钥，优先级高于密钥文件；`openssl rand -base64 32` 可生成 |
| `SECRET_KEY_FILE` | `data/.secret-key` | 密钥文件路径；文件不存在时自动生成（权限 `0600`） |

示例：

```bash
PORT=8080 LOG_LEVEL=debug DATABASE_PATH=/var/lib/relay-api/gateway.db \
SECRET_KEY_FILE=/var/lib/relay-api/.secret-key ./relay-api
```

## 上游重试与熔断

`/v1/*` 的每次上游调用遇到可重试错误（网络错误、超时、408、429、5xx）时，会按 `UPSTREAM_MAX_RETRIES` 在同一路由上做指数退避重试：`UPSTREAM_RETRY_BASE_MS` 起步、逐次翻倍、`UPSTREAM_RETRY_MAX_MS` 封顶，并带 50%~100% 的随机抖动，避免并发请求在同一时刻集中重试。非可重试状态（400/401/403/404 等）不会重试，直接按上游状态码透传。

同一 Provider 连续失败 `UPSTREAM_BREAKER_THRESHOLD` 次后熔断 `UPSTREAM_BREAKER_COOLDOWN_MS`：期间该 Provider 的路由被直接跳过（有备用路由时自动走备用），到期后只放行一个探测请求，成功即恢复、失败则重新计时。所有路由都处于熔断状态时返回 `503 upstream_unavailable`（而不是 502），客户端可据此退避重试。

重试只发生在响应头返回之前，已经开始向客户端输出的 SSE 流不会被重放；客户端主动断开也不会被计入熔断失败次数。

## 配额与日志保留

日/周/月配额读取的是 `api_key_daily_usage`（按 `API Key + 本地日期` 汇总的小表），而不是 `usage_logs`：

- **日志清理不会影响周/月配额**。`LOG_RETENTION_DAYS` 只作用于用量/审计日志，可以按磁盘情况设成 3 天甚至更短。
- 每天 `LOG_CLEANUP_TIME` 清理日志后，内存配额计数器会被清空；下一次请求按日汇总重新加载“周/月基数 + 当日用量”。当日用量由内存计数器实时累加，热路径不查库。
- 汇总表保留 400 天（远大于 31 天的月窗口），首次升级会从最近 40 天的 `usage_logs` 自动回填一次（受当时的日志保留期限制）。
- `LOG_RETENTION_DAYS=0` 表示永久保留日志；手动清理接口也不会再把它当成“全部删除”。
- 日志按每批 1 万行分批删除，首次清理大表时不会长时间占住 SQLite 写连接。

## Provider 自定义 Header 与会话标识

在后台 Provider 的 `headers` 中填写 JSON 对象即可向上游追加请求头，值支持两种占位符：

- `${name}` / `${headers.name}`：取客户端同名请求头（大小写不敏感），例如 `${uuid}`。
- `${_ai_session}`：取本次请求解析出的用户标识。

用户标识按以下顺序解析：`x-opencode-session`、`x-ai-session`、`ai-session`、`x-session-id`、`session-id`、`x-user-id`、`user-id`、`x-client-id`、`client-id`、`x-uuid`、`uuid`、`x-user`、`user` 请求头；其次是请求体中的 `user` 或 `metadata.user_id`；都没有时使用客户端 IP 与 User-Agent 的哈希（32 位十六进制）。

示例：

```json
{
  "x-user-id": "${x-user-id}",
  "x-client-uuid": "${uuid}",
  "x-ai-session": "${_ai_session}"
}
```

## 推理内容回放（thinking / reasoning_content，可选）

DeepSeek V4 等 thinking 模式模型要求多轮对话时把上一轮的推理内容原样传回，否则上游直接 400：

```
The `reasoning_content` in the thinking mode must be passed back to the API.
The `content[].thinking` in the thinking mode must be passed back to the API.
```

OpenAI 格式的客户端（OpenCode、绝大多数 agent）不会把 `reasoning_content` 放进下一轮历史，所以**第一轮正常、第二轮开始报错**。

默认**关闭**，因为它是转发路径上唯一会改动请求体的行为。需要时在 Provider 上打开：编辑 Provider，勾选 `reasoning replay`（或 API `"reasoningReplay": true`）。

打开后：

- 响应返回的 `reasoning_content`（流式或非流式）会按 `Provider + 模型别名 + 会话标识 + 该轮正文/tool_call id` 记在内存里；
- 下一次请求里，客户端漏掉推理的 assistant 消息会被补上 `reasoning_content`；如果客户端用的是 `reasoning` 字段，会同时镜像一份 `reasoning_content`；
- 客户端已经带了推理时不覆盖；不支持推理的上游因为缓存里没有对应条目，永远不会被改写。

限制：

- 缓存是进程内的（2048 条 / 6 小时 TTL），重启或多实例部署后会失效，届时会退回上游原始的 400；
- 只对 `openai` / `compatible` Provider 生效；`anthropic` 类型走自己的请求转换，目前不透传 thinking 块；
- 会话标识来自上面的解析规则，客户端固定发送 `x-opencode-session` 之类的头能让回放更稳定。

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
├── deploy/relay-api.service # systemd 示例（LimitNOFILE / GOMEMLIMIT）
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
│   ├── providers/           # 上游 provider 适配（Chat Completions / Anthropic / Responses）
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

每个发布包内含二进制、`LICENSE` 与 `THIRD_PARTY_LICENSES`（三方依赖许可证原文，由 `make licenses` 从 `go.mod` 与模块缓存自动汇编，也可单独运行生成到 `dist/`）。

## 部署注意

- `CGO_ENABLED=0` 静态编译（SQLite 驱动为纯 Go 的 `modernc.org/sqlite`）
- 持久化 `DATABASE_PATH` 指向的目录，并同时保留 `SECRET_KEY_FILE`，二者需一起备份
- 访问日志经有界队列（8192 条）批量异步写入 stdout：攒到 64 KiB 或 **100 行**（先到者）即刷一批，最多等 200 ms；请求线程只入队，stdout 被阻塞时不会拖慢网站或 `/v1`，队列满的日志会被丢弃并在下一次落盘时输出 `dropped=N` 统计；请确保日志消费者（docker logs、面板、重定向文件）正常读取
- 仪表盘“今日”统计由内存计数器维护：启动时从 SQLite 预热一次，之后随用量写入实时递增，不会随着 `usage_logs` 变大而变慢；`/admin/stats` 默认不返回全量配置统计，脚本需要时加 `?full=1`
- 服务器层刻意不设置 `WriteTimeout`/`ReadTimeout`（长连接 SSE 与 `/free` 隧道需要），空闲控制由 `STREAM_IDLE_TIMEOUT_MS` 在处理器内完成
- 反向代理需允许长连接并关闭响应缓冲（Nginx 参考：`proxy_buffering off;`），否则流式响应会被缓冲

### 容量与资源上限

`/free` 隧道是公开的、长连接的，而且每条隧道同时占用**客户端 socket + 上游 socket**，所以容量瓶颈通常不是 CPU 而是资源：

- **文件描述符**：进程启动时会把软 `RLIMIT_NOFILE` 提到 `MAX_OPEN_FILES`（默认 65535，受硬上限约束）。硬上限本身来自部署环境，参考 `deploy/relay-api.service`：

  ```ini
  LimitNOFILE=65535
  # 容器则是：docker run --ulimit nofile=65535:65535 ...
  ```

  默认的 1024 软上限大约只能撑 500 条并发隧道，达到上限后 `accept()` 报 `EMFILE`，**包括 `/v1` 在内的所有新请求都会被拒绝**。
- **单条隧道的内存**：`copyAndFlush` 的拷贝块（`TUNNEL_COPY_BUFFER_KB`，默认 64 KiB）在整条响应期间都会占住（包括阻塞在读取、暂时没有数据时），所以内存约等于 `块大小 × 并发隧道数`：64 KiB 时 3000 条并发隧道约 190 MiB，调到 128 KiB 则约 375 MiB，另有每条隧道的 goroutine 栈与 http2 流控缓冲。块大小会与 `H2C_MAX_CONCURRENT_STREAMS` 相乘，两者一起调大前先按并发数估算。
- **HTTP/2 并发流**：`H2C_MAX_CONCURRENT_STREAMS`（默认 2048）是**连接级** SETTINGS，不是按 API Config 配置的；它只影响新连接（改完需重启），超限的流返回 `REFUSED_STREAM` 由客户端重试。另外端到端并发还受上游那一跳公告的流数上限约束，一味调大入口上限可能只是把排队位置前移。
- **堆上限**：建议给 Go 设置软上限 `GOMEMLIMIT`（约为容器内存的 60%~75%），否则运行时可能要等到约 2 倍活跃堆才 GC，突发缓冲很容易把内存顶爆：

  ```bash
  GOMEMLIMIT=1200MiB ./relay-api
  ```
- **`/free` 的默认限额**：新建配置默认 `streamTimeoutMs=300000`（5 分钟）与 `streamMaxBodyMb=8`；其中 `streamTimeoutMs` 是**空闲超时**而不是总时长——等待响应头的时间、以及响应体两次数据之间的间隔受它限制，持续有数据的长连接不会被强制断开。显式填 `0` 表示不限（旧库升级时会把历史配置里的 0 一次性改成这两个默认值，之后手动设 0 不会被覆盖）。没有超时的隧道在客户端/上游异常消失时会永久占住 fd 和缓冲，因此不建议把超时设为 0。
- **按客户端限并发**：代码里没有针对 `/free` 的按 IP 并发限制（该路径完全公开）。需要防风控时在前置 Nginx/HAProxy 上限制，例如：

  ```nginx
  limit_conn_zone $binary_remote_addr zone=free_conn:10m;
  location /free/ { limit_conn free_conn 200; proxy_buffering off; }
  ```
- **隔离**：如果 `/free` 的流量与 AI 中转不允许互相影响，最稳妥的做法是**同一个二进制起两个进程**（改 `PORT` 与 `DATABASE_PATH`），让 fd 与内存风险彻底分开。

## 许可证

本项目以 [MIT](LICENSE) 发布。

三方依赖均为宽松协议，与 MIT 兼容；再分发二进制或源码时需保留各自的版权声明与免责声明。版本以 `go.mod` 为准。

| 模块 | 协议 | 用途 |
| --- | --- | --- |
| `golang.org/x/crypto` | BSD-3-Clause | bcrypt 密码哈希、登录挑战的 nacl/box |
| `golang.org/x/net` | BSD-3-Clause | `http2` 与 `h2c`（单端口明文 HTTP/2） |
| `modernc.org/sqlite` | BSD-3-Clause | 纯 Go SQLite 驱动；内嵌的 SQLite 本体为 Public Domain |
| `modernc.org/libc`、`modernc.org/mathutil`、`modernc.org/memory` | BSD-3-Clause | 间接依赖（`modernc.org/sqlite` 使用） |
| `golang.org/x/sys`、`golang.org/x/text` | BSD-3-Clause | 间接依赖 |
| `github.com/google/uuid`、`github.com/remyoudompheng/bigfft` | BSD-3-Clause | 间接依赖 |
| `github.com/dustin/go-humanize`、`github.com/mattn/go-isatty`、`github.com/ncruces/go-strftime` | MIT | 间接依赖 |

前端为原生 ES Module、无构建步骤，不含第三方运行时依赖；`web/` 下的代码随本项目一并按 MIT 发布。
