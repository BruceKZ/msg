# msg

通用的消息接收与转发服务。任何来源通过同一个 HTTP 接口提交消息，服务把消息存进 SQLite，再转发到 Telegram 等渠道，同时提供网页消息中心。
第一种消息类型是短信（备用 iPhone 用快捷指令转发过来的）。

完整设计见 [docs/design.md](docs/design.md)。

```
POST /api/v1/messages ─▶ kind adapter（校验 data，Compose 成 Envelope）
                          ─▶ 存储（messages + deliveries，同一事务；按 kind 去重）
                          ─▶ 调度器 ─▶ sender adapter（Telegram 等，失败退避重试）
GET  /api/v1/messages ─▶ 网页 / 脚本 / Telegram 命令
```

## API 速查

| 方法 | 路径 | 权限 |
|---|---|---|
| POST | `/api/v1/messages` | `messages:write` |
| GET | `/api/v1/messages?kind=&source=&q=&before=&since=&limit=` | `messages:read` |
| GET | `/api/v1/messages/{id}` | `messages:read` |
| DELETE | `/api/v1/messages/{id}` | `messages:delete` |
| GET | `/api/v1/messages/{id}/deliveries` | `messages:read` |
| POST | `/api/v1/messages/{id}/deliveries` | `deliveries:write` |
| GET | `/api/v1/kinds`、`/api/v1/senders`、`/api/v1/routes`、`/api/v1/me` | 任意有效身份 |
| POST/PATCH/DELETE | `/api/v1/senders[/{name}]`、`…/{name}/test`、`…/{name}/chats`、`/api/v1/sender-types`、`PUT /api/v1/routes/{kind}` | `senders:write` |
| GET | `/healthz` | 无 |

提交一条短信：

```bash
curl -X POST https://msg.example.com/api/v1/messages \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"kind":"sms","data":{"from":"95588","body":"……"}}'
```

可选字段 `senders`（只发给这些渠道，`[]` 表示只存不发）、`dedup_key`（自定义去重键）。
新消息返回 `201` 和 `Location` 头；命中去重返回 `200` 和 `X-Duplicate: true`。
错误格式：`{"error":{"code":"validation_failed","message":"…","field":"data.body"}}`。

## 扩展

- **新消息类型**：在 `internal/kinds/<name>` 实现 `core.Kind`（`Info`、`Decode`）和 `core.Message`（`Compose`，可选 `DedupKey`），
  再加进 `main.go` 的 `availableKinds()`。
- **新渠道**：在 `internal/senders/<name>` 实现 `core.Sender`（`Name`、`Send(Envelope)`），并提供一个 `senders.Type`
  （配置字段说明 + `Build`），加进 `main.go` 的 `senderTypes()`。网页设置页会自动出现这个类型的表单。

渠道（Telegram、Discord 等）和路由不写在配置文件里，而是在网页“设置”页里添加、测试、启用/停用，改动立即生效。

两者互不认识，也都不需要改通知核心。

## 目录

```
main.go                     配置、组装、启动；子命令 gentoken / version / check
internal/core               Kind、Message、Envelope、Sender 等接口和类型
internal/hub                提交：解码、Compose、路由、去重
internal/store              SQLite：messages、deliveries
internal/dispatch           投递与重试
internal/auth               token + scope、Cloudflare Access JWT
internal/api                /api/v1 和网页
internal/kinds/sms          短信类型
internal/senders            sender 类型描述与运行时管理（热更新、密钥只写不读、路由）
internal/senders/telegram   Telegram sender、聊天命令、最近会话
internal/senders/discord    Discord Webhook sender
web/static                  网页消息中心
deploy/                     init 脚本、示例配置、部署脚本
```

## 开发与部署

```bash
go test -race ./...
msg -config config.json check   # 只校验配置，不碰真实数据库
msg gentoken                     # 生成一个随机 token
deploy/deploy.sh openwrt         # 测试 → 交叉编译 → 校验配置 → 安装 → 重启
```

路由器上：程序 `/usr/bin/msg`，配置 `/etc/msg/config.json`（示例见 `deploy/config.example.json`），
数据库 `/mnt/ssd/msg/msg.db`，日志 `logread -e "msg\["`。
