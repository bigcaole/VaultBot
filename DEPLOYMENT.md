# 部署说明（生产）

本说明基于 OpenResty 反向代理 + HTTPS。目标是：应用仅监听本地回环地址，公网只开放 443。

## 1. 服务监听与安全建议
- 建议 `HTTP_ADDR=127.0.0.1:8080`，避免应用直接暴露公网端口。
- 反向代理终止 TLS，并转发到本地 8080。
- 需要允许 `X-Forwarded-Proto: https` 透传，明文密码接口会校验 HTTPS。

## 2. 服务器防火墙
- 仅开放 22/80/443（或 22/443）。
- 不开放 8080（因为应用只监听本地）。

## 3. 运行应用
### 方式 A：直接二进制
```bash
# 示例环境变量
export HTTP_ADDR=127.0.0.1:8080
export DB_URL="postgres://user:pass@127.0.0.1:5432/vaultbot?sslmode=disable"
export REDIS_URL="redis://127.0.0.1:6379/0"
export MASTER_KEY="<32字节或base64>"
export API_KEY="<高强度随机串>"
export TELEGRAM_BOT_TOKEN="<可选>"
export FEISHU_APP_ID="<可选>"
export FEISHU_APP_SECRET="<可选>"
export FEISHU_VERIFICATION_TOKEN="<可选>"
export FEISHU_ENCRYPT_KEY="<可选>"
export ALLOWED_USER_IDS="<逗号分隔>"

./vaultbot
```

### 方式 B：Docker
```bash
docker compose up -d
```
确保 `docker-compose.yml` 中 `HTTP_ADDR` 设置为 `127.0.0.1:8080`。

## 4. OpenResty 反向代理（HTTPS）

### 4.1 获取证书（示例：acme.sh）
```bash
# 示例，按你的证书方案调整
acme.sh --issue -d your-domain.com --webroot /var/www/html
acme.sh --install-cert -d your-domain.com \
  --key-file /etc/ssl/private/your-domain.com.key \
  --fullchain-file /etc/ssl/certs/your-domain.com.crt
```

### 4.2 OpenResty 配置示例
```nginx
server {
    listen 80;
    server_name your-domain.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name your-domain.com;

    ssl_certificate     /etc/ssl/certs/your-domain.com.crt;
    ssl_certificate_key /etc/ssl/private/your-domain.com.key;

    # 强烈建议启用 HSTS（确认 HTTPS 正常后再启用）
    # add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_read_timeout 60s;
    }
}
```

### 4.3 测试与重载
```bash
openresty -t
openresty -s reload
```

## 5. DNS 配置
- 为域名添加 A 记录指向服务器公网 IP。
- 不需要将域名写入 `HTTP_ADDR`。

## 6. 常见问题
- 访问 443 正常但 `include_password=1` 报错：确认代理是否设置了 `X-Forwarded-Proto: https`。
- 机器人回调：Telegram/飞书回调地址请配置为 `https://your-domain.com/...`，并确保 443 可达。
- 如果需要临时对外暴露 8080：将 `HTTP_ADDR=:8080` 并放行防火墙，但不推荐生产环境使用。
