# VaultBot

高安全性云端密码管理机器人（Telegram & 飞书），使用 Go + Gin + PostgreSQL + Redis，密码采用 AES-256-GCM 加密。

## 主要特性
- AES-256-GCM 加密，随机 Nonce，避免相同密码产生相同密文
- Telegram / 飞书机器人交互
- Redis 用于限流与会话状态管理
- REST API（API Key 认证）
- 阅后即焚：发送包含密码的消息后 60 秒自动删除

## 环境变量
必需：
- `DB_URL`：PostgreSQL 连接串，例如 `postgres://user:pass@localhost:5432/vaultbot?sslmode=disable`
- `REDIS_URL`：Redis 连接串，例如 `redis://localhost:6379/0`
- `MASTER_KEY`：32 字节主密钥（原始字符串或 base64 编码），绝不落库
- `API_KEY`：REST API 的访问密钥

可选：
- `HTTP_ADDR`：HTTP 监听地址，默认 `:8080`
- `TELEGRAM_BOT_TOKEN`：Telegram Bot Token
- `FEISHU_APP_ID` / `FEISHU_APP_SECRET`：飞书应用凭证
- `FEISHU_VERIFICATION_TOKEN` / `FEISHU_ENCRYPT_KEY`：飞书事件验证（如需）
- `ALLOWED_USER_IDS`：白名单用户 ID，逗号分隔
  - Telegram：填写用户数字 ID
  - 飞书：填写 `user_id` 或 `open_id`
- `DELETE_AFTER_SECONDS`：阅后即焚延迟秒数，默认 60
- `DB_CONNECT_RETRIES`：数据库连接重试次数，默认 10
- `DB_CONNECT_DELAY_SECONDS`：数据库连接重试间隔秒数，默认 3
- `ALLOW_GROUP_CHAT`：是否允许群聊使用机器人，默认 `false`
- `PASSWORD_TOKEN_TTL_SECONDS`：获取明文密码的一次性令牌有效期（秒），默认 60

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

## Telegram 指令
- `/add`：引导式输入平台、用户名、密码等信息
- `/find <platform>`：模糊搜索并返回卡片（包含密码）
- `/list`：按分类查看所有记录（不包含密码）
- `/cancel`：取消当前引导流程

## 飞书交互
- `/add`：引导式输入
- `/find <platform>`：返回交互式卡片，提供“点击复制”按钮
- `/list`：按分类查看所有记录

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
