# msg 设计（v2）

一个通用的消息接收与转发服务。短信是第一种消息类型；以后的实验结束提醒、定时任务、服务器告警等，
都是新增一种类型，不改接口、不改通知核心、不改发送渠道。

## 1. 核心模型：两头各一个 adapter，中间只有一个通用类型

```
  入口 adapter（每种 kind 一个）          系统核心                出口 adapter（每个 sender 一个）
  sms.Decode → SMS.Compose()        ─┐                        ┌─▶ telegram：排成 HTML 文本
  notice.Decode → Notice.Compose()  ─┼─▶ Message + Envelope ──┼─▶ discord：排成 embed 卡片
  ……                                ─┘  存储 / 投递 / 重试    └─▶ ……
```

- **Kind（消息类型）**：负责校验 `data` 的字段，并把自己拼装成 `Envelope`。
- **Sender（发送渠道）**：只认识 `Envelope`，负责排成本平台的格式并发出去。
- **核心**：存储消息、生成投递任务、重试。它不认识任何具体类型，也不认识任何平台的格式。

新增类型只需要写一个 Kind，新增渠道只需要写一个 Sender。

## 2. 名词

| 名词 | 含义 |
|---|---|
| message | 一条进入系统的消息，是 API 中唯一的核心资源 |
| kind | 消息类型，如 `sms`；决定 `data` 的结构和拼装方式 |
| data | 该类型的原始字段，由 kind 定义 |
| envelope | 由 kind 拼装出的通用信封，所有 sender 都只认它 |
| source | 提交者，即上报所用 token 的名字，如 `iphone-backup`、`gpu-server` |
| sender | 发送渠道实例，如 `tg` |
| delivery | 一条消息投递到一个 sender 的记录及其状态 |

`source` 是通用概念，替代了之前短信专用的"设备"字段。

## 3. 资源与接口

统一前缀 `/api/v1`。路径只出现通用名词，不出现任何具体类型名。

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/v1/messages` | `messages:write` | 提交一条消息 |
| GET | `/api/v1/messages` | `messages:read` | 查询消息列表 |
| GET | `/api/v1/messages/{id}` | `messages:read` | 单条消息，含投递状态 |
| DELETE | `/api/v1/messages/{id}` | `messages:delete` | 删除一条消息及其投递记录 |
| GET | `/api/v1/messages/{id}/deliveries` | `messages:read` | 该消息在各 sender 的投递记录 |
| POST | `/api/v1/messages/{id}/deliveries` | `deliveries:write` | 重新投递，可指定 sender |
| GET | `/api/v1/kinds` | 任意有效身份 | 已注册的类型及其 data 字段说明 |
| GET | `/api/v1/senders` | 任意有效身份 | 已配置的 sender；有 `senders:write` 时附带配置（永远不含密钥） |
| POST | `/api/v1/senders` | `senders:write` | 新建 sender：`{name, type, enabled, config}` |
| GET / PATCH / DELETE | `/api/v1/senders/{name}` | `senders:write` | 查看 / 修改（省略的字段和空密钥保持不变）/ 删除 |
| POST | `/api/v1/senders/{name}/test` | `senders:write` | 发一条测试消息，返回 `{ok, error}` |
| GET | `/api/v1/senders/{name}/chats` | `senders:write` | 最近给 bot 发过消息的会话（Telegram），用于挑 chat id |
| GET | `/api/v1/sender-types` | `senders:write` | 可用的 sender 类型及其配置字段说明 |
| GET | `/api/v1/routes` | 任意有效身份 | 每种 kind 的默认 sender 列表与当前实际生效的列表 |
| PUT | `/api/v1/routes/{kind}` | `senders:write` | `{"senders": [...]}` 设置；`{"senders": null}` 恢复为“全部” |
| GET | `/api/v1/me` | 任意有效身份 | 当前身份：`{name, via, scopes, kinds}`，`kinds` 为 `null` 表示全部 |
| GET | `/healthz` | 无 | 健康检查，不加版本前缀 |
| GET | `/` | 页面公开，数据需鉴权 | 网页消息中心 |

### 3.1 消息资源的表示

```json
{
  "id": 12,
  "kind": "sms",
  "source": "iphone-backup",
  "data": { "from": "95588", "body": "……", "received_at": "2026-11-04T13:00:00Z" },
  "envelope": {
    "title": "95588",
    "body": "……",
    "fields": [ { "name": "来源", "value": "iphone-backup" } ],
    "time": "2026-11-04T13:00:00Z",
    "priority": "normal",
    "link": ""
  },
  "created_at": "2026-11-04T13:00:01Z",
  "deliveries": { "tg": "sent" }
}
```

列表接口里的 `deliveries` 只给状态摘要；要看每次尝试的错误信息，查 `/deliveries` 子资源。

### 3.2 POST `/api/v1/messages`

请求：

```json
{
  "kind": "sms",
  "data": { "from": "95588", "body": "……", "received_at": "2026-11-04T14:00:00+01:00" },
  "senders": ["tg"],
  "dedup_key": "可选"
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `kind` | 是 | 必须是已注册的类型，且在 token 允许的类型范围内 |
| `data` | 是 | 由该 kind 校验 |
| `senders` | 否 | 本条消息只发给这些 sender；不填则按路由规则（第 6 节） |
| `dedup_key` | 否 | 调用方自定义的去重键；不填则由 kind 计算 |

响应：

- `201 Created`，带 `Location: /api/v1/messages/12`，响应体为完整的消息资源。
- 命中去重时返回 `200 OK`，响应体是之前那条消息，响应头带 `X-Duplicate: true`。

除 JSON 外也接受表单，方便简单客户端：除 `kind`、`senders`（逗号分隔）、`dedup_key` 外的表单字段都当作 `data` 的字符串字段，例如 `kind=sms&from=95588&body=…`。
JSON 请求体里出现未定义的顶层字段会被拒绝（`invalid_json`），避免拼错字段名后被悄悄忽略。

### 3.3 GET `/api/v1/messages`

| 参数 | 说明 |
|---|---|
| `kind` | 按类型过滤，可重复：`?kind=sms&kind=notice` |
| `source` | 按提交者过滤 |
| `q` | 在 envelope 的标题和正文里搜索子串 |
| `before` / `since` | 游标：id 小于 / 大于该值；用于翻页 / 增量拉取 |
| `limit` | 默认 50，上限 500 |

响应：

```json
{ "items": [ …消息资源… ], "next_before": 12 }
```

`next_before` 为 `null` 表示没有更多。只返回 token 有权读取的类型。

### 3.4 重新投递 POST `/api/v1/messages/{id}/deliveries`

```json
{ "senders": ["tg"] }
```

不填 `senders` 时，重投该消息所有失败的投递。返回 `202 Accepted`。

### 3.5 GET `/api/v1/kinds`

```json
{ "items": [ { "name": "sms", "description": "手机收到的短信",
               "fields": [ { "name": "from", "type": "string", "required": true },
                           { "name": "body", "type": "string", "required": true },
                           { "name": "received_at", "type": "time", "required": false } ],
               "default_senders": ["tg"] } ] }
```

调用方据此知道每种类型要传哪些字段，网页也据此生成筛选项。

## 4. 统一约定

- **格式**：请求和响应都是 JSON，UTF-8。时间一律用 RFC 3339 字符串。
- **错误**：

  ```json
  { "error": { "code": "validation_failed", "message": "data.body is required", "field": "data.body" } }
  ```

  | HTTP | code | 场景 |
  |---|---|---|
  | 400 | `invalid_json` | 请求体不是合法 JSON |
  | 400 | `validation_failed` | data 校验失败，`field` 指出哪个字段 |
  | 400 | `unknown_kind` / `unknown_sender` | 类型或 sender 不存在或已禁用 |
  | 401 | `unauthorized` | 没有身份或 token 无效 |
  | 403 | `forbidden` | 有身份但权限不够，或无权访问该类型 |
  | 404 | `not_found` | 资源不存在 |
  | 413 | `payload_too_large` | 请求体超过 64 KB |
  | 500 | `internal` | 服务端错误 |

- **缓存**：所有 `/api` 响应带 `Cache-Control: no-store`。
- **版本**：不兼容的改动走 `/api/v2`，v1 保留到所有客户端迁移完。

## 5. 鉴权与权限

token 统一一种写法：

```json
"tokens": [
  { "name": "iphone-backup", "token": "…", "scopes": ["messages:write"], "kinds": ["sms"] },
  { "name": "gpu-server",    "token": "…", "scopes": ["messages:write"], "kinds": ["notice"] },
  { "name": "laptop",        "token": "…", "scopes": ["messages:read"] }
]
```

| scope | 允许 |
|---|---|
| `messages:write` | 提交消息 |
| `messages:read` | 查询消息、查看投递记录 |
| `messages:delete` | 删除消息 |
| `deliveries:write` | 重新投递 |
| `senders:write` | 管理 sender（含密钥）和路由 |

- `kinds` 限制这个 token 能读写哪些类型；不写表示全部。
- token 的 `name` 就是写入消息时的 `source`。
- 网页登录走 Cloudflare Access，服务端校验 JWT。登录用户的权限在 `access.scopes` 里配置，默认 `["messages:read", "messages:delete", "deliveries:write", "senders:write"]`。
- Cloudflare 侧有两个 Access 应用：`msg.example.com` 只允许指定邮箱用验证码登录；`msg.example.com/api/v1/messages` 设为 Bypass，
  让手机等无法交互登录的客户端只凭 token 访问。
- Bypass 路径上 Cloudflare 不附加 `Cf-Access-Jwt-Assertion` 头，所以服务端也接受浏览器自带的 `CF_Authorization` cookie（同一份签名 JWT）。
  用 cookie 认证时，非 GET/HEAD 请求必须同源（`Sec-Fetch-Site: same-origin`，或 `Origin` 与 Host 一致），防止 CSRF。
- token 只通过 `Authorization: Bearer` 传递，不接受放在 URL 里。

## 6. 投递与路由

一条消息发给哪些 sender，按下面的顺序确定：

1. 请求里的 `senders`
2. 该 kind 的路由（网页“设置 → 路由”或 `PUT /api/v1/routes/{kind}`）；其中已停用或出错的 sender 会被跳过
3. 所有运行中的 sender

列表中有不存在或已禁用的 sender 时，提交失败并返回 `unknown_sender`，不会悄悄丢掉。

消息写入和投递任务生成在同一个事务里完成。调度器按 5 秒、30 秒、2 分钟、10 分钟、30 分钟、1 小时退避重试；
配置错误这类重试也没用的错误，直接标记为 `failed`。投递状态：`pending`、`sent`、`failed`。

## 7. 去重

- 去重范围：同一 `source`、同一 `kind`，在该 kind 配置的时间窗口内（默认 120 秒，配置为 0 表示不去重）。
- 去重键优先用请求里的 `dedup_key`；没有时由 kind 计算，比如 sms 用"号码 + 正文"。kind 也可以不提供去重键，那就不去重。
- sms 默认窗口 120 秒，用来应对一条短信同时触发多条快捷指令自动化。

## 8. 代码结构与接口

```go
// internal/core
type Kind interface {
    Name() string
    Describe() KindInfo                           // 供 /kinds 使用
    Decode(data json.RawMessage) (Message, error) // 入口 adapter：校验并构造
}

type Message interface {
    Compose(meta Meta) Envelope // meta 里有 source、时间等通用信息
}

type Deduper interface { // Message 可选实现
    DedupKey() string
}

type Sender interface { // 出口 adapter
    Name() string
    Send(ctx context.Context, e Envelope) error
}
```

```
main.go                       读配置，注册 kind 和 sender 类型，启动 HTTP
internal/core                 Kind、Message、Envelope、Sender 接口；Hub（提交、路由、去重）
internal/store                SQLite：messages、deliveries 两张表，迁移
internal/dispatch             投递调度与重试
internal/api                  /api/v1 路由、鉴权、错误格式
internal/kinds/sms            短信类型
internal/senders              sender 类型描述、运行时管理器（配置校验、密钥隐藏、热更新、路由）
internal/senders/telegram     Telegram sender、聊天命令、最近会话
internal/senders/discord      Discord Webhook sender
web/                          网页消息中心
```

### 存储

所有类型共用一张 `messages` 表：`id, kind, source, data(JSON), envelope(JSON), dedup_key, created_at`；
另有 `deliveries` 表：`id, message_id, sender, status, attempts, last_error, next_attempt_at, updated_at`。
各类型不需要单独建表；以后某个类型确实需要（比如通讯录），再由它自己加。
`senders` 表（name, type, enabled, config JSON 含密钥）和 `routes` 表（kind → sender 名列表）保存运行时配置。
数据库里有密钥，文件权限设为 0600。

### Sender 的运行时管理

- 每种 sender 类型声明自己的配置字段（`string`、`secret`、`int_list`、`bool`），网页据此生成表单，新增类型不用改网页。
- `secret` 字段只写不读：API 只返回 `secrets_set` 列出哪些已设置；修改时留空表示保持原值。
- 保存时先用新配置构建 sender，构建失败（如 token 格式不对）直接返回 `validation_failed`，不会存入坏配置。
- 修改立即生效：旧实例停止（Telegram 的轮询会等它退出，避免两个实例抢更新），新实例启动。服务重启时从数据库恢复。
- 删除 sender 会把它从所有路由里去掉；已排队但发往已删除/停用 sender 的投递标记为失败，可在网页上重投。

### 聊天命令（Telegram）

命令同样面向通用的消息，不绑定类型：

| 命令 | 说明 |
|---|---|
| `/latest [kind]` | 最新一条消息 |
| `/recent [n] [kind]` | 最近 n 条，默认 5 |
| `/kinds` | 列出类型 |
| `/id` | 显示当前 chat id |
| `/help` | 帮助 |

只有配置在 `chat_ids` 里的会话能查看消息；回复内容同样由 Telegram sender 从 envelope 排版。

## 9. 配置

```json
{
  "listen": "127.0.0.1:8787",
  "db_path": "/mnt/ssd/msg/msg.db",
  "timezone": "Europe/Zurich",
  "tokens": [ … ],
  "access": { "team_domain": "", "aud": "", "scopes": ["messages:read", "messages:delete", "deliveries:write", "senders:write"] },
  "kinds": {
    "sms": { "enabled": true, "dedup_window_seconds": 120 }
  }
}
```

配置文件只放服务本身的设置；sender 和路由在网页“设置”里管理，存在数据库中。

## 10. 第一个类型：sms

| data 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `from` | string | 是 | 发件人号码或通讯录名字 |
| `body` | string | 是 | 正文 |
| `received_at` | time | 否 | 手机收到的时间；缺失或与服务器时间相差超过一天时，用服务器时间 |

- Compose 结果：标题为 `from`，正文为 `body`，附加字段为"来源"，时间为 `received_at`。
- 去重键为"号码 + 正文"。

iPhone 快捷指令的请求：

```
POST https://msg.example.com/api/v1/messages
Authorization: Bearer <iphone-backup 的 token>
Content-Type: application/json

{ "kind": "sms", "data": { "from": <发件人>, "body": <内容>, "received_at": <当前日期> } }
```

## 11. 本期范围

- **做**：第 3 节全部接口；kind 和 sender 两种 adapter；sms 类型；Telegram sender 和通用命令；
  路由、去重、重试；token 权限和 Access 校验；网页消息中心；测试。
- **不做**：sms 以外的类型（如通用提醒 `notice`）、平台专用排版、通讯录。
- **已追加**：sender 改为网页配置（Telegram、Discord Webhook），见第 8 节。
- **数据**：路由器上的旧库只有一条部署测试消息，直接建新库，不写迁移。
- **部署**：设计定稿、代码完成并通过测试后再编译部署，替换路由器上的旧服务。

## 12. 命名与部署位置

服务名为 `msg`：

| 项 | 值 |
|---|---|
| 项目目录 | `~/Projects/msg`，Go module `msg` |
| 程序 | `/usr/bin/msg` |
| 配置 | `/etc/msg/config.json` |
| 数据库 | `/mnt/ssd/msg/msg.db` |
| init 脚本 | `/etc/init.d/msg` |
| 对外地址 | `https://msg.example.com` → 隧道 → `127.0.0.1:8787` |

旧的 `smsrelay` 服务、`/etc/smsrelay/`、`/mnt/ssd/smsrelay/` 在新服务部署后删除。
