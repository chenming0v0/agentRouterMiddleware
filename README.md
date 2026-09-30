# AgentRouterMiddleware

**开源，无限期停更。** 本仓库不再开发，也不会再有功能更新、问题修复或版本发布。代码按最后一次提交保留，供自行阅读和部署。

一个位于 LLM API 供应商（newapi / sub2api 等）前方的**可重试反向代理中间件**。
上游会随机返回 404 / 400 / 429 等“假失败”，而调用方通常不会重试；本中间件会透明地
重试，让客户端只看到最终（理想情况下成功的）响应。触发重试的状态码与响应体正则均可在
WebUI 中配置。

- 零第三方 Go 依赖，仅使用标准库。
- 内嵌管理 WebUI（`web/dist`），单二进制部署。
- 请求日志、请求/响应完整报文持久化到磁盘，并支持在线 SSE 推送。

---

## 1. 运行环境

| 组件 | 版本 |
| --- | --- |
| Go | 1.26.2 |
| Node.js | 24 |
| pnpm | 最新稳定版 |

默认监听地址：`http://127.0.0.1:18851`（仅本机回环，默认无需鉴权）。
**默认使用 18851，不占用 8080 端口。**

---

## 2. 构建与启动

```bash
# 完整构建：先构建前端，再编译 Go 二进制
make build

# 产物
./bin/agentrouter

# 仅 Go 构建（前端资源已在 web/dist 时）
make go
```

启动：

```bash
./bin/agentrouter \
  -config ./config.json \
  -listen 127.0.0.1:18851 \
  -log-dir ./data/logs
```

停止：向进程发送 `SIGINT` / `SIGTERM`，会进行最长 10 秒的优雅关闭，
超时后强制关闭活动连接。

### 命令行参数与优先级

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:18851` | 监听地址。**显式传入时优先级最高**；未显式传入时使用配置文件中的 `listen`。 |
| `-config` | `./config.json` | 配置文件路径。不存在时会写入默认配置。 |
| `-web-dir` | 空 | 从磁盘目录提供 WebUI，覆盖内嵌资源（开发用）。 |
| `-admin-token` | 空 | 管理 API 令牌；未显式传入时回退到环境变量 `AGENTROUTER_ADMIN_TOKEN`。 |
| `-log-dir` | `./data/logs` | 请求日志元数据与报文体文件的持久化目录。 |

优先级规则：**显式命令行 flag > 配置文件 `listen` > flag 默认值**。

### 启动安全校验

- 仅监听回环地址（`127.0.0.1` / `::1` / `localhost`）时，默认允许无令牌访问。
- 若监听**非回环地址**（如 `0.0.0.0:18851`）却**没有配置管理令牌**，启动会被直接拒绝，
  以避免把管理端与代理意外暴露到公网。

公网监听示例：

```bash
export AGENTROUTER_ADMIN_TOKEN='请替换为强随机令牌'
./bin/agentrouter -listen 0.0.0.0:18851
```

---

## 3. 配置模型

配置文件为 JSON，首次启动自动生成，权限为 `0600`。供应商专属入口无需配置 API Key。
如果使用旧入口的附加请求头功能，配置文件可能包含真实凭据，切勿提交到 Git（见 `.gitignore`）。

```json
{
  "listen": "127.0.0.1:18851",
  "log_limit": 50,
  "max_request_body_bytes": 67108864,
  "max_buffer_bytes": 8388608,
  "max_log_body_bytes": 0,
  "body_sniff_timeout_ms": 250,
  "error_body_timeout_ms": 2000,
  "upstreams": [
    {
      "id": "u_sample",
      "name": "newapi-main",
      "base_url": "https://api.example.com",
      "enabled": false,
      "weight": 100,
      "timeout_ms": 300000,
      "path_globs": ["/v1/*"],
      "model_globs": ["gpt-4o*"],
      "headers": {}
    }
  ],
  "retry": {
    "max_attempts": 3,
    "base_delay_ms": 500,
    "max_delay_ms": 8000,
    "jitter": true,
    "retry_on_network_error": false,
    "status_codes": [400, 404, 429, 500, 502, 503, 504],
    "body_regexes": ["(?i)rate limit", "(?i)no available channel", "(?i)无可用渠道"]
  }
}
```

要点：

- **默认样例上游是禁用（`enabled: false`）状态，必须自行配置并启用后才能代理。**
- **`max_attempts` 是“总尝试次数”**：`max_attempts = 3` 表示首次请求 + 最多 2 次重试。
- `status_codes` 内的任一状态码，或任一 `body_regexes` 匹配响应体，都会触发重试（OR 关系）。
- `body_regexes` 使用 **Go 的 RE2 语法**，不是 JavaScript 正则；不支持回溯引用等特性。
- `retry_on_network_error` 默认 `false`，避免对非幂等请求造成重复副作用。
- `max_log_body_bytes = 0` 表示**完整落盘捕获**（见第 6 节磁盘警告）；设为正数则只捕获该字节数。

### upstream base_url 约定

- 若客户端请求路径已包含 `/v1`，`base_url` 不要再重复包含 `/v1`，例如填写 `https://api.example.com`。
  代理会拼接上游前缀和被剥离 provider 段后的请求路径；两边都写 `/v1` 会得到 `/v1/v1/...`。
- **不要将 base_url 指向本中间件自身**，否则会形成自环并导致请求风暴。中间件带有跳环检测
  （若自己的 hop 标记回到自身会返回 508）。

### provider ID 作为 URL 标识符

每个 provider 的 `id` 同时用作专用 URL 的路径标识符：

- 规则：`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`，即 1–64 个字母/数字/`_`/`-`，必须以字母或数字开头。
- 必须唯一，且**不能**是保留前缀（大小写不敏感）：`api`、`assets`、`v1`、`v1beta`、`v2`。
- `id` 在 `name` / `base_url` 变更后保持不变；默认样例 `u_sample` 与自动生成的 ID 仍然合法。
- 不要在 `id` 中放任何密钥——它会出现在 URL 中。

### `"***"` 是保留字面量

`GET /api/config` 会把所有上游 `headers` 的值替换为字面量 `"***"`，以防止密钥外泄。
`PUT /api/config` 时：

- 值为 `"***"` 表示“保持已存储的旧值不变”；
- 新的普通字符串表示“替换为新值”；
- `headers` 字段整体缺失或为 `null` 表示“保留原 map”；
- 显式给出对象则是**完整替换**，未出现的 key 会被删除；
- 若 `"***"` 找不到对应的已存储值，PUT 会返回 400，且不会改动现有配置。

---

## 4. 客户端接入（推荐：provider 专用 URL）

在 WebUI 中创建一个 provider，填写你自己的上游 `base_url`，并给它一个 `id`（例如 `openai`）。
然后把调用方的 Base URL 指向：

```
http://127.0.0.1:18851/openai/v1
```

- 调用方使用**自己的、该 provider 原有的 API key**（例如 `Authorization: Bearer <你的 key>`）。
  无需在中间件配置该业务 key，专用路由会把调用方的 key **原样透传**给该 provider。
- `.../openai`、`.../openai/v1` 与 `.../openai/v1/chat/completions` 都可用：
  中间件只剥离第一段 `openai`，其余路径、查询串、方法与请求体原样转发。
- 也可使用 provider 根路径（不带版本段）：
  ```
  http://127.0.0.1:18851/openai
  ```

### 专用 URL 的固定路由语义

- `/{id}/...` **只**选择该 `id` 的 provider：无视 `weight`、`path_globs`、`model_globs`。
  即使该 provider 权重为 `0` 或 glob 不匹配，显式选择也生效。
- **重试永远固定在同一 provider**，绝不会在重试时切换到另一个 provider。
  重试耗尽后返回该 provider 的最后一次响应。
- 专用路由**忽略该 provider 配置的 `headers` 覆盖**（纯凭据透传），因此不会出现
  “A 的 key 被换成 B 的 key”。旧式非前缀路由仍保留 `headers` 覆盖行为（兼容用）。
- 已知 `id` 但 provider 被禁用：返回 `503`，不转发。
- 未知 `id` 的 `/{id}/v1[/*]`、`/{id}/v1beta[/*]`、`/{id}/v2[/*]`：返回 `404`，
  **不会**回退到任何 provider。

### 旧式非前缀路由（legacy，兼容保留）

不带 provider 前缀的路径（`/v1/*`、`/api/*`、`/v1beta/*`、`/v2/*` 等）仍按旧的
`path_globs` + `model_globs` + 权重规则自动选择上游。该模式仅用于兼容旧客户端，
**不是推荐的工作流**；新接入请使用上面的 provider 专用 URL。

> 上游 base_url 不要再重复 `/v1`；客户端这里需要带 `/v1`。

---

## 5. 管理 API

除 `GET /api/health` 公开外，其余管理接口与全部报文体下载接口都遵循同一套鉴权：

- 配置了令牌：需要请求头 `Authorization: Bearer <token>`（常量时间比较）。
- 未配置令牌：仅允许来自**回环地址**的直连（忽略 `X-Forwarded-For`，防伪造），
  并校验 `Host` 为 localhost / 回环 IP，防 DNS rebinding。
- 跨源请求：若携带 `Origin`，其 host 必须与请求 `Host` 一致；无通配 CORS。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | `{"ok":true}`，公开。 |
| GET | `/api/config` | 获取配置（headers 已脱敏为 `"***"`）。 |
| PUT | `/api/config` | 校验并保存；非法返回 400 且不改动原配置。 |
| GET | `/api/logs?limit=50` | 最近的日志（新的在前）。 |
| DELETE | `/api/logs` | 清空日志。 |
| GET | `/api/logs/stream` | SSE 实时日志；每 15 秒发送 `: ping` 心跳。 |
| GET | `/api/logs/{id}` | 单条日志元数据。 |
| GET | `/api/logs/{id}/body/request` | 原始请求体下载。 |
| GET | `/api/logs/{id}/body/response` | 最终响应体下载（自动解析别名）。 |
| GET | `/api/logs/{id}/body/attempt-N` | 第 N 次上游尝试的响应体（N ≥ 1）。 |
| GET | `/api/stats` | 累计统计。 |
| POST | `/api/regex/test` | 正则测试；编译错误放在 `error`，仍返回 200。 |
| POST | `/api/upstreams/test` | 上游连通性探测（10s 超时，不跟随跳转；任何 HTTP 状态都算可达）。 |

**只有以上明确列出的 `/api/*` 路径属于管理端。** 其他 `/api/...`（如 `/api/chat`）
会被当作业务请求转发给上游，因此兼容以 `/api` 为前缀的 OpenAI 兼容路由。

管理接口与日志响应均带 `Cache-Control: no-store`。

---

## 6. 重试与流式行为

- **先检查状态码，再发送响应**：匹配状态码且还有尝试次数时，可以重试，包括返回 SSE 或大正文的错误响应。失败正文采集受 `error_body_timeout_ms` 限制，超时会标为不完整后继续重试。
- **成功的 SSE 立即透传并逐块刷新**，不对 SSE 帧执行正文正则。压缩响应也不执行正文正则。
- 普通非流式响应只在 `body_sniff_timeout_ms`（默认 250ms）和 `max_buffer_bytes`（默认 8 MiB）内读到完整正文时执行正则；超时或超限则原样透传，并标记跳过正则。
- **一旦向客户端提交最终响应头或任何正文，就不再重试**。后续读取失败会中止响应并记录不完整状态，不会追加 JSON 错误或重复生成内容。
- **网络错误重试默认关闭**，避免非幂等请求产生重复副作用。
- **重试 POST 可能造成重复的生成与计费**：上游若在返回错误前已经执行了生成，
  重试会再次消耗额度。请根据业务谨慎配置 `status_codes` 与 `max_attempts`。

---

## 7. 日志持久化与磁盘占用

- 默认保留最近 **50 条完整日志**（由 `log_limit` 控制），并**跨进程重启持久化**于 `-log-dir`。
- 每条日志的报文体以私有文件存放（权限 `0600`）。
- `max_log_body_bytes = 0` 表示完整捕获。**无上限捕获会持续占用大量磁盘**，
  请按需设置显式上限。
- 元数据中的 `*_body_bytes` / `*_body_captured_bytes` / `*_body_complete` /
  `*_body_available` / `*_body_error` 字段用于说明捕获是否完整；
  即使捕获不完整，原始下载接口仍可读取已捕获的部分。
- 请求体与响应体在元数据 JSON 中只保留**有界预览**，完整内容通过下载接口获取，
  避免列表接口一次性加载所有报文体。

---

## 8. 安全说明

- 上游认证头与日志文件均属敏感数据；运行中的 `config.json` 以 `0600` 权限保存。
- `GET /api/config` 会隐藏附加请求头中的值；不要把密钥放进供应商名称、标识符或上游 URL。
- 业务代理路径**不做任何本地密钥校验**：调用方用哪个 key 就原样转发给被选中的 provider，
  管理令牌的取值在业务路径上不被检查、也不参与替换。管理令牌仅保护 `/api/*` 管理接口。
- 日志中的请求头与 query 凭据会被脱敏为 `***`（`Authorization`、`X-API-Key`、
  `X-Goog-Api-Key`、`Cookie` 以及 `api_key`/`key`/`access_token` 等 query 参数），
  但转发给 provider 的原始值不变。
- 启动日志不会打印任何密钥值。
- 管理令牌只保护管理 API，不是业务代理的访问令牌。专用 provider URL 不要求在网关配置
  业务密钥；公网开放时仍应通过前置网关或网络策略限制访问，尤其不要公开使用固定密钥的旧入口。

---

## 9. 常用命令

```bash
make build     # 构建前端 + Go 二进制
make go        # 仅编译 Go
make test      # go test ./...
make vet       # go vet ./...
make fmt       # gofmt
make run       # 构建并启动
make clean     # 删除 bin/
```
