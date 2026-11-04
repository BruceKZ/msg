# msg

一个通用的**消息接收与转发服务**。任何程序（手机快捷指令、训练脚本、定时任务……）都通过同一个 HTTP 接口提交消息；
msg 把消息存进 SQLite，再按路由转发到 Telegram、Discord 等渠道，并提供一个网页消息中心。

第一种消息类型是**短信**：备用 iPhone 收到短信后，由快捷指令转发给 msg，再推送到 Telegram。

- 单个 Go 可执行文件（约 11 MB），内置 SQLite 和网页，无其他依赖
- 跑在 OpenWrt 路由器上，经 Cloudflare Tunnel 对外，Cloudflare Access 做登录
- 渠道、路由、API token 都在网页上管理，修改立即生效

完整的设计取舍见 [docs/design.md](docs/design.md)。

---

## 目录

- [功能一览](#功能一览)
- [架构](#架构)
- [Quick start](#quick-start)
  - [1. 本地试跑](#1-本地试跑)
  - [2. 部署到 OpenWrt 路由器](#2-部署到-openwrt-路由器)
  - [3. 公网访问：Cloudflare Tunnel + Access](#3-公网访问cloudflare-tunnel--access)
  - [4. 配置推送渠道](#4-配置推送渠道)
  - [5. 接入 iPhone 快捷指令](#5-接入-iphone-快捷指令)
- [配置文件](#配置文件)
- [鉴权与权限](#鉴权与权限)
- [API 参考](#api-参考)
- [渠道与路由](#渠道与路由)
- [扩展](#扩展)
- [运维](#运维)
- [安全说明](#安全说明)
- [开发](#开发)

---

## 功能一览

| 功能 | 说明 |
|---|---|
| 统一提交接口 | `POST /api/v1/messages`，用 `kind` 区分消息类型，`data` 是该类型自己的字段 |
| 去重 | 同一来源、同一类型的相同消息在时间窗口内只存一次、只推送一次（应对一条短信触发多条自动化） |
| 存储与查询 | SQLite；按类型、来源、关键字搜索；游标翻页和增量拉取 |
| 多渠道推送 | Telegram、Discord（Webhook）；失败按 5 秒 → 1 小时退避重试；可手动重投 |
| 路由 | 每种消息默认发往哪些渠道；提交时也可单独指定 |
| 网页消息中心 | 实时刷新、搜索、删除、重投；“设置”页管理渠道、路由和 API token |
| Telegram 命令 | `/latest`、`/recent`、`/kinds`、`/id`，只对授权会话生效 |
| API token | 网页生成（只显示一次）、按权限和类型限制、随时吊销 |
| 登录 | Cloudflare Access 邮箱验证码；服务端自己校验签名 |

---

## 架构

```
            入口 adapter（每种 kind 一个）       系统核心                出口 adapter（每种 sender 一个）
 iPhone ──┐                                                       ┌─▶ Telegram：HTML 文本
 脚本   ──┼─▶ POST /api/v1/messages ─▶ Kind.Decode ─▶ Compose ─▶ Envelope ─┼─▶ Discord：embed 卡片
 ……     ──┘                                │                      └─▶ ……
                                           ▼
                          SQLite：messages + deliveries（同一事务）
                                           │
                                 调度器：按渠道投递、退避重试
```

**两头各一个 adapter，中间只有一个通用类型：**

- **Kind（消息类型）**：校验自己的 `data` 字段，并把自己拼装（`Compose`）成通用的 **Envelope**（标题、正文、附加字段、时间、优先级、链接）。
- **Sender（渠道）**：只认识 Envelope，把它排成自己平台的格式发出去。
- **核心**：只存储和投递 Envelope，不认识任何具体类型或平台。

所以新增一种消息只写一个 Kind，新增一个渠道只写一个 Sender，代码量是“类型数 + 渠道数”而不是“类型数 × 渠道数”。

| 名词 | 含义 |
|---|---|
| message | 进入系统的一条消息，API 中唯一的核心资源 |
| kind | 消息类型，如 `sms` |
| data | 该类型的原始字段，由 kind 定义和校验 |
| envelope | kind 拼装出的通用信封，所有渠道只认它 |
| source | 提交者，即所用 token 的名字，如 `iphone-backup` |
| sender | 一个渠道实例，如名为 `tg` 的 Telegram bot |
| delivery | 一条消息投递到一个 sender 的记录和状态（`pending` / `sent` / `failed`） |
| route | 某种 kind 默认发往哪些 sender |

---

## Quick start

### 1. 本地试跑

需要 Go 1.26+（见 `go.mod`）。

```bash
git clone <repo> msg && cd msg
go test ./...
go build -o msg .
```

写一个最小配置（token 用 `./msg gentoken` 生成）：

```bash
mkdir -p data
cat > config.json <<EOF
{
  "listen": "127.0.0.1:8787",
  "db_path": "./data/msg.db",
  "timezone": "Europe/Zurich",
  "tokens": [
    { "name": "admin", "token": "$(./msg gentoken)",
      "scopes": ["messages:read", "messages:write", "messages:delete",
                 "deliveries:write", "senders:write", "tokens:write"] }
  ]
}
EOF
./msg -config config.json check     # 只校验配置，不碰数据库
./msg -config config.json           # 启动
```

提交一条消息、再读出来：

```bash
TOKEN=$(sed -n 's/.*"token": "\([^"]*\)".*/\1/p' config.json)

curl -X POST http://127.0.0.1:8787/api/v1/messages \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"kind":"sms","data":{"from":"95588","body":"你好"}}'

curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/api/v1/messages
```

浏览器打开 <http://127.0.0.1:8787>，输入这个 token 即可看到网页；“设置”页里可以添加渠道、生成 token。

> 配置文件里的 token **只用于本地开发**（本地没有 Cloudflare Access，需要它来登录网页）。
> 正式部署时配置文件里不写 token：人通过 Access 登录，给程序用的 token 一律在网页上生成和吊销。

### 2. 部署到 OpenWrt 路由器

部署脚本会：跑测试 → 交叉编译 linux/arm64 → 经 SSH 传到路由器 → 用新程序校验现有配置 → 替换并重启。

```bash
deploy/deploy.sh openwrt          # openwrt 是 ~/.ssh/config 里的主机名
```

第一次部署时，脚本会提示先创建配置（不写 token，见第 3、5 步）：

```bash
ssh openwrt
vi /etc/msg/config.json           # 参考 deploy/config.example.json，填 Access 的 team_domain 和 aud
chmod 600 /etc/msg/config.json
msg -config /etc/msg/config.json check
/etc/init.d/msg enable && /etc/init.d/msg start
```

路由器上的位置：

| 内容 | 路径 |
|---|---|
| 程序 | `/usr/bin/msg` |
| 配置 | `/etc/msg/config.json` |
| 数据库 | `/mnt/ssd/msg/msg.db`（放在 SSD；SSD 未挂载时服务拒绝启动，不会误写闪存） |
| 启动脚本 | `/etc/init.d/msg`（procd，开机自启、崩溃自动重启） |
| 日志 | `logread -e "msg\["` |

这三个路径已加入 `/etc/sysupgrade.conf`，刷固件时保留。

### 3. 公网访问：Cloudflare Tunnel + Access

路由器上的 `cloudflared` 把 `msg.example.com` 转到 `http://localhost:8787`，路由器不需要开任何端口。

**隧道路由**（Zero Trust → Networks → Tunnels → 选隧道 → Published application）：

| 字段 | 值 |
|---|---|
| Subdomain / Domain | `msg` / `example.com` |
| Path | 留空 |
| Service | `http://localhost:8787` |

**Access 应用**（Zero Trust → Access → Applications → Self-hosted），两个：

| 应用 | 域名 / 路径 | 策略 | 作用 |
|---|---|---|---|
| `msg` | `msg.example.com` | Allow，Include → Emails → 你的邮箱 | 网页和管理接口只有你能进 |
| bypass | `msg.example.com/api/v1/messages` | Bypass，Include → Everyone | 手机、脚本等无法交互登录的程序只凭 token 访问 |

然后把第一个应用的 **AUD Tag** 和 team domain（`xxx.cloudflareaccess.com`）写进配置的 `access`，服务端会自己校验 Cloudflare 签发的登录凭证（即使有人绕过 Cloudflare 直连也没用）。

### 4. 配置推送渠道

打开网页 → 右上角 **设置** → **发送渠道** → **+ 添加渠道**。

**Telegram**

1. 在 Telegram 里找官方的 [@BotFather](https://t.me/BotFather)（有蓝色认证勾），发 `/newbot`，按提示起名，得到 bot token。
2. 在设置页选类型 Telegram，填名称（如 `tg`）和 bot token，Chat IDs 先留空，**保存**。状态变成“运行中”。
3. 在 Telegram 里给你的 bot 发 `/id`，它会回复你的 chat id。
4. 回设置页，点“编辑”，在“最近的会话”里点“加入”（或直接粘贴那串数字），**保存**。
5. 点 **测试**，Telegram 应收到一条测试消息。

勾选“启用聊天命令”后，可在授权的会话里用 `/latest [kind]`、`/recent [n] [kind]`、`/kinds`。不在 Chat IDs 里的人给 bot 发消息只会被忽略（`/id` 只告诉对方他自己的 id）。

**Discord**

1. Discord 频道设置 → 整合 → Webhooks → 新建，复制 Webhook URL。
2. 设置页选类型“Discord（Webhook）”，填名称和 URL，保存，点测试。

消息以卡片形式发送；为防止短信内容里的 `@everyone` 真的提醒到所有人，所有 @ 提醒都被关闭。

### 5. 接入 iPhone 快捷指令

1. 设置页 → **API Token** → **+ 生成 token**：名称 `iphone-backup`，权限只勾“提交消息”，类型选“只限 sms”。生成后**立刻复制**，它只显示这一次。
2. 快捷指令 → 自动化 → 新建“信息”自动化，条件“信息包含 0”（再分别建 1–9，共 10 条，用来匹配含数字的短信），选“立即运行”。
3. 动作“获取 URL 内容”：

| 项 | 值 |
|---|---|
| URL | `https://msg.example.com/api/v1/messages` |
| 方法 | POST |
| 头部 | `Authorization` = `Bearer <刚生成的 token>` |
| 请求体 | JSON：`kind` = `sms`；`data`（词典）= `from`: 快捷指令输入的“发件人”，`body`: “内容”，`received_at`: “当前日期”（可选） |

一条短信同时含多个数字时会触发多条自动化，msg 会在 120 秒窗口内去重，只存一条、只推送一次。

token 泄露或换手机时，在设置页**吊销**旧 token、生成新的即可。

---

## 配置文件

配置文件只放服务本身的设置；渠道、路由、网页生成的 token 都存在数据库里。未知字段会被拒绝（防止拼错）。

```json
{
  "listen": "127.0.0.1:8787",
  "db_path": "/mnt/ssd/msg/msg.db",
  "timezone": "Europe/Zurich",
  "access": {
    "team_domain": "<team>.cloudflareaccess.com",
    "aud": "<Application Audience Tag>",
    "scopes": ["messages:read", "messages:delete", "deliveries:write", "senders:write", "tokens:write"]
  },
  "kinds": {
    "sms": { "enabled": true, "dedup_window_seconds": 120 }
  }
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 只监听本机，由 cloudflared 转发 |
| `db_path` | 必填 | 所在目录必须已存在 |
| `timezone` | `Local` | 推送消息里显示时间用（程序内置时区库，OpenWrt 也可用） |
| `tokens` | `[]` | **仅本地开发用**，正式部署留空。格式 `[{name, token, scopes, kinds}]`；名称 1–64 位小写字母数字 `.-_`，token 至少 16 位 |
| `access.team_domain` / `aud` | 空 | 两个都填才启用 Access JWT 校验 |
| `access.scopes` | 全部管理权限 | Access 登录用户获得的权限 |
| `kinds.<name>.enabled` | `true` | 关闭某种类型 |
| `kinds.<name>.dedup_window_seconds` | `120` | 去重窗口，`0` 表示不去重 |

子命令：`msg gentoken`（生成随机 token）、`msg version`、`msg -config <file> check`（只校验配置，用内存数据库）。

---

## 鉴权与权限

**身份来源**

| 方式 | 适用 | 说明 |
|---|---|---|
| `Authorization: Bearer <token>` | 程序 | 配置文件 token 或网页生成的 token；不接受放在 URL 里 |
| Cloudflare Access | 人（浏览器） | 服务端校验 `Cf-Access-Jwt-Assertion` 头；在 Bypass 路径上改读 `CF_Authorization` cookie（同一份签名 JWT） |

用 cookie 认证时，非 GET/HEAD 请求必须同源（`Sec-Fetch-Site: same-origin`，或 `Origin` 与 Host 一致），防止 CSRF。

**权限（scope）**

| scope | 允许 | 可授予网页生成的 token |
|---|---|---|
| `messages:write` | 提交消息 | ✅ |
| `messages:read` | 查询消息、投递记录 | ✅ |
| `messages:delete` | 删除消息 | ✅ |
| `deliveries:write` | 重新投递 | ✅ |
| `senders:write` | 管理渠道（含密钥）和路由 | ❌ |
| `tokens:write` | 生成、吊销 token | ❌ |

管理类权限只给登录的人：即使某个 token 泄露，也不能用它生成新 token 或把通知改发到别处。

每个 token 还可以用 `kinds` 限制只能读写某些类型；读不到的消息在列表里不出现，按 id 访问返回 404。

**网页生成的 token**

- 格式 `msg_` + 43 位随机字符（256 位熵）；数据库只存 SHA-256，原文只在生成时返回一次。
- 列表显示前几位、权限、类型、创建人、最近使用时间（约每分钟更新一次）。
- 吊销立即生效；同名 token 吊销后可以重新生成。
- 正式部署时所有 token 都在网页上管理；配置文件里的 token 只用于本地开发，在列表中标为“配置文件”，只能改配置删除。

---

## API 参考

前缀 `/api/v1`；请求和响应都是 JSON；时间都是 RFC 3339 字符串。

### 消息

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/messages` | `messages:write` | 提交消息 |
| GET | `/messages` | `messages:read` | 列表，新到旧 |
| GET | `/messages/{id}` | `messages:read` | 单条，含投递状态 |
| DELETE | `/messages/{id}` | `messages:delete` | 删除 |
| GET | `/messages/{id}/deliveries` | `messages:read` | 每个渠道的投递记录（次数、错误、下次重试时间） |
| POST | `/messages/{id}/deliveries` | `deliveries:write` | 重投：`{"senders":["tg"]}`；不带则重投所有失败的 |

**提交**

```json
{
  "kind": "sms",
  "data": { "from": "95588", "body": "……", "received_at": "2026-11-04T14:00:00+01:00" },
  "senders": ["tg"],
  "dedup_key": "可选"
}
```

- `senders` 可选：不填按路由；`[]` 表示只存不发；写了不存在或未运行的渠道返回 `unknown_sender`。
- `dedup_key` 可选：覆盖 kind 自己的去重键。
- 新消息返回 `201` + `Location`；命中去重返回 `200` + `X-Duplicate: true`，响应体是之前那条。
- 也接受表单：`kind=sms&from=95588&body=…`（除 `kind`、`senders`、`dedup_key` 外的字段都进 `data`）。
- JSON 顶层出现未定义字段会被拒绝。请求体上限 64 KB。

**查询参数**

| 参数 | 说明 |
|---|---|
| `kind` | 按类型过滤，可重复或逗号分隔 |
| `source` | 按提交者过滤 |
| `q` | 搜索标题和正文 |
| `order` | `desc`（默认，新到旧）或 `asc`（旧到新） |
| `before` / `after` | id 游标：只要比它旧 / 比它新的（`since` 是 `after` 的旧名） |
| `limit` | 每页条数，默认 50，**上限 100**，超过按 100 处理 |

分页用 id 游标而不是页码：新消息不断进来时，页码会让翻页重复或漏掉，游标不会；而且翻到多后面都一样快。

```json
{
  "items": [{
    "id": 12, "kind": "sms", "source": "iphone-backup",
    "data": { "from": "95588", "body": "……", "received_at": "2026-11-04T13:00:00Z" },
    "envelope": { "kind": "sms", "title": "95588", "body": "……",
                  "fields": [{ "name": "来源", "value": "iphone-backup" }],
                  "time": "2026-11-04T13:00:00Z", "priority": "normal" },
    "created_at": "2026-11-04T13:00:01Z",
    "deliveries": { "tg": "sent" }
  }],
  "next_before": 12,
  "next_after": null
}
```

返回满一页时给出下一页的游标：新到旧时是 `next_before`，旧到新时是 `next_after`；为 `null` 表示没有更多。

```bash
# 往下翻页
curl -H "Authorization: Bearer $TOKEN" "$URL/api/v1/messages?limit=50&before=<next_before>"
# 从头导出全部历史（旧到新）
curl -H "Authorization: Bearer $TOKEN" "$URL/api/v1/messages?order=asc&limit=100&after=<next_after>"
```

### 渠道、路由、token

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/senders` | 任意身份 | 渠道列表；有 `senders:write` 时附带配置（永不含密钥，只有 `secrets_set`） |
| POST | `/senders` | `senders:write` | `{name, type, enabled, config}` |
| GET / PATCH / DELETE | `/senders/{name}` | `senders:write` | PATCH 中省略的字段、空的密钥字段保持原值 |
| POST | `/senders/{name}/test` | `senders:write` | 发测试消息，返回 `{ok, error}` |
| GET | `/senders/{name}/chats` | `senders:write` | 最近给 bot 发过消息的会话（Telegram） |
| GET | `/sender-types` | `senders:write` | 可用渠道类型及其字段（网页据此生成表单） |
| GET | `/routes` | 任意身份 | 每种 kind 的路由和实际生效的渠道 |
| PUT | `/routes/{kind}` | `senders:write` | `{"senders":["tg"]}`；`{"senders":null}` 恢复为“全部” |
| GET | `/tokens?include_revoked=1` | `tokens:write` | token 列表（不含原文和哈希） |
| POST | `/tokens` | `tokens:write` | `{name, scopes, kinds}` → `{token, info}`，`token` 只返回这一次 |
| DELETE | `/tokens/{id}` | `tokens:write` | 吊销 |

### 其他

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/kinds` | 类型列表及字段说明、默认渠道 |
| GET | `/me` | 当前身份：`{name, via, scopes, kinds}`（`kinds: null` = 全部） |
| GET | `/healthz` | 健康检查（不在 `/api/v1` 下） |

### 错误

```json
{ "error": { "code": "validation_failed", "message": "is required", "field": "data.body" } }
```

| HTTP | code |
|---|---|
| 400 | `invalid_json`、`validation_failed`、`unknown_kind`、`unknown_sender`、`unsupported` |
| 401 | `unauthorized`（没有或无效的凭证） |
| 403 | `forbidden`（有身份但权限不够，或无权访问该类型） |
| 404 | `not_found` |
| 409 | `conflict`（名称已存在）、`sender_inactive`（渠道停用或启动失败） |
| 413 | `payload_too_large` |
| 500 | `internal` |

---

## 渠道与路由

一条消息发给哪些渠道，按顺序决定：

1. 提交时的 `senders`
2. 该 kind 的路由（设置页 → 路由）；其中停用或出错的渠道会被跳过
3. 所有运行中的渠道

渠道配置的行为：

- 保存时先用新配置构建渠道，失败（如 token 格式不对、Webhook 不是 discord.com）直接报错，不存坏配置。
- 修改立即生效：旧实例停止（Telegram 轮询会等它退出，避免抢更新），新实例启动；服务重启时从数据库恢复。
- 删除渠道会从所有路由中移除它；已排队但发往已删除或停用渠道的投递标记为失败，可在网页上重投。
- Telegram：token 无效时每 5 分钟重试一次，其他网络错误 10 秒；400/401/403/404 视为永久失败不重试。
- Discord：429 和 5xx 会重试，其他 4xx 视为永久失败。

---

## 扩展

### 新增消息类型

在 `internal/kinds/<name>` 实现 `core.Kind` 和 `core.Message`，加进 `main.go` 的 `availableKinds()`：

```go
type Kind struct{}

func (Kind) Info() core.KindInfo {
    return core.KindInfo{Name: "notice", Description: "通用提醒",
        Fields: []core.FieldInfo{{Name: "title", Type: "string", Required: true}, {Name: "body", Type: "string"}}}
}

func (Kind) Decode(data json.RawMessage, meta core.Meta) (core.Message, error) {
    var n Notice
    if err := json.Unmarshal(data, &n); err != nil || n.Title == "" {
        return nil, &core.ValidationError{Field: "data.title", Message: "is required"}
    }
    return n, nil
}

type Notice struct {
    Title string `json:"title"`
    Body  string `json:"body"`
}

func (n Notice) Compose(meta core.Meta) core.Envelope {
    return core.Envelope{Title: n.Title, Body: n.Body, Priority: core.PriorityHigh}
}
// 可选：func (n Notice) DedupKey() string { … } 启用自动去重
```

接口、存储、网页、所有渠道都自动支持新类型。

### 新增渠道

在 `internal/senders/<name>` 实现 `core.Sender`（`Name()`、`Send(ctx, Envelope)`），再提供一个 `senders.Type`
（声明配置字段，字段种类 `string` / `secret` / `int_list` / `bool`；`Build` 从配置构建实例），加进 `main.go` 的 `senderTypes()`。
设置页会自动出现这个类型的表单。可选实现：

| 接口 | 作用 |
|---|---|
| `senders.Runner` | `Run(ctx)`：后台任务（如轮询），渠道停用或修改时自动取消 |
| `senders.ChatLister` / `ChatSeeder` | 提供“最近的会话”，重建时保留 |
| 返回 `core.Permanent{Err}` | 告诉调度器不要重试 |

---

## 运维

| 任务 | 命令 |
|---|---|
| 看日志 | `ssh openwrt 'logread -e "msg\["'` |
| 重启 | `ssh openwrt /etc/init.d/msg restart` |
| 升级 | `deploy/deploy.sh openwrt`（新程序先校验配置，失败则不替换） |
| 校验配置 | `ssh openwrt 'msg -config /etc/msg/config.json check'` |
| 备份数据库 | 停服务后复制 `/mnt/ssd/msg/msg.db*`（路由器上没有 sqlite3 命令） |

**常见问题**

| 现象 | 原因 / 处理 |
|---|---|
| 网页 `ERR_NAME_NOT_RESOLVED` | 新域名的 DNS 缓存，清浏览器 DNS 缓存（Edge：`edge://net-internals/#dns`） |
| 访问返回 530 | 隧道断了：路由器离线或 `cloudflared` 没运行 |
| 返回 502 | 隧道通，但 msg 没在 8787 上运行，看 `logread` |
| Telegram bot 不回复 | 渠道没保存成功或已停用；保存后再给 bot 发消息（保存前发的消息不会被处理） |
| 测试提示 `no chat_ids configured` | 还没把会话加进 Chat IDs |
| 提交返回 403 | token 没有 `messages:write`，或 `kinds` 不包含这个类型 |
| 页面不是新版 | 已设 `Cache-Control: no-cache`，刷新一次即可 |

**cloudflared 注意**：路由器上的 cloudflared 是手动替换的官方版本，不要对它执行 `opkg upgrade`（软件源里是旧版）。

---

## 安全说明

- 服务只监听 `127.0.0.1`，外部访问只能经 Cloudflare Tunnel。
- 网页和管理接口在 Cloudflare Access 后面；服务端还会自己校验 Access JWT（签名、audience、issuer、有效期）。
- Bypass 路径（`/api/v1/messages`）上只有 token 能访问；token 以 SHA-256 比对，配置文件 token 用常量时间比较。
- 渠道密钥（bot token、Webhook URL）存在数据库里，API 永不返回；数据库和配置文件权限 0600。
- 错误信息中会抹掉 bot token 和 Webhook URL，避免写进日志或投递记录。
- 网页设置 `X-Frame-Options: DENY`、`nosniff`、`no-referrer`；所有用户内容用 `textContent` 渲染。

---

## 开发

```bash
go test -race ./...
gofmt -l .
go vet ./...
```

```
main.go                     配置、组装、启动；子命令 gentoken / version / check
internal/core               Kind、Message、Envelope、Sender 等接口和类型
internal/hub                提交：解码、Compose、路由、去重
internal/store              SQLite：messages、deliveries、senders、routes、tokens；迁移
internal/dispatch           投递与重试
internal/auth               token（配置 + 数据库）、scope、Cloudflare Access JWT
internal/api                /api/v1 和网页
internal/kinds/sms          短信类型
internal/senders            渠道类型描述与运行时管理（热更新、密钥只写不读、路由）
internal/senders/telegram   Telegram：推送、聊天命令、最近会话
internal/senders/discord    Discord Webhook
web/static                  网页（单文件，无构建步骤）
deploy/                     init 脚本、示例配置、部署脚本
docs/design.md              设计文档
```

测试覆盖：并发去重、权限和类型隔离、所有错误码、翻页、重试与重投、渠道增删改与热更新、路由、密钥不外泄、
token 生成/使用/吊销、Access JWT（伪造、篡改、过期、错 audience）、cookie 认证的 CSRF 检查、Telegram/Discord 格式与错误处理、配置校验。
