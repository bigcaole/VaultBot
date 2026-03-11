# VaultBot

高安全性云端密码管理机器人（Telegram），使用 Go + Gin + PostgreSQL + Redis，密码采用 AES-256-GCM 加密。

## 主要特性
- AES-256-GCM 加密，随机 Nonce，避免相同密码产生相同密文
- MASTER_KEY + SECRET_PEPPER 通过 Argon2id 派生密钥
- Telegram 机器人交互
- Redis 用于限流与会话状态管理
- REST API（API Key 认证）
- 阅后即焚：发送包含密码的消息后 60 秒自动删除

## 环境变量
说明包含用途与获取方式，所有值以环境变量注入。

必需：
| 变量 | 说明 | 获取方式 |
| --- | --- | --- |
| `DB_URL` | PostgreSQL 连接串，例如 `postgres://user:pass@localhost:5432/vaultbot?sslmode=disable` | 本地/容器中创建数据库与账号后拼接连接串 |
| `REDIS_URL` | Redis 连接串，例如 `redis://localhost:6379/0` | 本地/容器中启动 Redis 后拼接连接串 |
| `MASTER_KEY` | 32 字节主密钥（原始字符串或 base64 编码），绝不落库 | 使用 `openssl rand -base64 32` 生成后保存 |
| `SECRET_PEPPER` | 额外胡椒粉字符串，用于 KDF 派生 | 使用 `openssl rand -base64 32` 生成后保存 |
| `UNLOCK_PIN` | 密码查询会话 PIN（/unlock 使用） | 自行生成高强度随机串并安全保存 |
| `BACKUP_PASSWORD` | 备份加密口令 | 自行生成高强度随机串并安全保存 |
| `API_KEY` | REST API 的访问密钥 | 自行生成高强度随机串并安全保存 |

可选：
| 变量 | 说明 | 获取方式 |
| --- | --- | --- |
| `HTTP_ADDR` | HTTP 监听地址，默认 `:8080` | 按部署端口设置 |
| `TELEGRAM_BOT_TOKEN` | Telegram 机器人 Token | 通过 BotFather 创建机器人获取 |
| `ALLOWED_USER_IDS` | 白名单用户 ID，逗号分隔（Telegram 数字 ID） | Telegram 可通过 `/id` 机器人或用户信息获取 |
| `DELETE_AFTER_SECONDS` | 阅后即焚延迟秒数，默认 60 | 按策略设置 |
| `DB_CONNECT_RETRIES` | 数据库连接重试次数，默认 10 | 按部署稳定性设置 |
| `DB_CONNECT_DELAY_SECONDS` | 数据库连接重试间隔秒数，默认 3 | 按部署稳定性设置 |
| `ALLOW_GROUP_CHAT` | 是否允许群聊使用机器人，默认 `false` | 需要群聊时设为 `true` |
| `PASSWORD_TOKEN_TTL_SECONDS` | 明文密码一次性令牌有效期（秒），默认 60 | 按安全策略设置 |
| `BACKUP_RECEIVER_IDS` | 备份接收人 ID 列表，逗号分隔 | Telegram 数字 ID |

示例生成 MASTER_KEY：
```bash
# 生成 32 字节随机 key（base64）
openssl rand -base64 32
```

## 本地启动
```bash
go run ./cmd/server
```

## Docker 启动
```bash
docker compose up --build
```

## 生产部署说明
见 `DEPLOYMENT.md`，包含 OpenResty 反向代理与 HTTPS 配置建议。

## 备份
内置定时任务每天 22:00 生成加密备份，并发送到 `BACKUP_RECEIVER_IDS`。主菜单支持手动触发备份，`/backup_test` 可验证备份流程。
如遇到 `pg_dump` 版本不匹配错误，请确保运行环境的 `pg_dump` 版本大于等于数据库版本（默认镜像已使用 Postgres 18 客户端）。

## Telegram 指令
- `/menu`：打开主菜单
- `/start`：显示功能入口（菜单按钮）
- `/unlock <PIN>`：解锁密码查询（15 分钟有效）
- `/add`：引导式输入平台、用户名、密码等信息
- `/find <platform>`：按平台关键词查询（无参数时进入分类浏览）
- `/search`：按字段搜索
- `/list`：按分类查看所有记录（不包含密码）
- `/ttl`：设置自动删除时间（3/5/10 分钟）
- `/backup`：手动触发备份（发送到备份接收人）
- `/cancel`：取消当前引导流程

主菜单包含“手动备份”按钮，用于立即触发备份并发送到 `BACKUP_RECEIVER_IDS`。

## 备份接收人指令
- `/menu`：打开备份菜单
- `/start`：显示备份菜单
- `/ping`：连接测试
- `/backup`：手动触发备份
- `/backup_test`：备份流程测试（不发送文件）
- `/help`：帮助说明

## REST API
所有接口需携带 `X-API-Key` 请求头。

- `GET /api/accounts?platform=xx&category=yy`
- `POST /api/accounts`
- `GET /api/accounts/:id?include_password=1`
- `PUT /api/accounts/:id`
- `DELETE /api/accounts/:id`
- `POST /api/password-token`

获取明文密码需要：
1) 使用 `POST /api/password-token` 获取一次性令牌（默认 60 秒有效）
2) 在 `GET /api/accounts/:id?include_password=1` 请求头中携带 `X-Password-Token`
3) 必须使用 HTTPS（或由反向代理设置 `X-Forwarded-Proto: https`）

创建/更新请求体示例：
```json
{
  "platform": "github",
  "category": "work",
  "username": "alice",
  "password": "secret",
  "email": "alice@example.com",
  "phone": "",
  "notes": "2FA enabled"
}
```

## 安全说明
- 密码仅在内存中出现，入库前加密
- 日志中不输出明文密码
- 支持白名单与限流
