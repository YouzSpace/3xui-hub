# ControlHub 部署指南

3x-ui 订阅管理中枢。Laravel 后端 + Vue3 前端 + SQLite。

## 环境要求

- PHP 8.4+
- Composer 2.2+
- PHP 扩展：fileinfo、pdo_sqlite、openssl、mbstring、gd（图形验证码）
- Nginx

## PHP 配置

宝塔 → PHP 8.4 → 设置 → 配置修改：

1. `disable_functions` 中删掉 `putenv`（否则 Composer 报错）
2. 开启 `fileinfo` 扩展
3. 开启 `gd` 扩展（图形验证码需要）

## 安装步骤

### 1. 上传文件

把 `backend/` 和 `frontend/` 上传到服务器，建议结构：

```
/www/wwwroot/your-domain.com/
├── backend/
│   ├── app/
│   ├── bootstrap/
│   ├── config/
│   ├── database/
│   ├── public/        ← 网站根目录
│   ├── resources/
│   ├── routes/
│   ├── storage/
│   ├── vendor/
│   ├── artisan
│   ├── composer.json
│   └── .env
└── frontend/
    └── dist/          ← 前端构建产物
```

### 2. 配置 .env

```bash
cd /path/to/backend
cp .env.example .env   # 或手动创建
```

编辑 `.env`：

```env
APP_NAME=ControlHub
APP_ENV=production
APP_KEY=                # 运行 php artisan key:generate 自动生成
APP_DEBUG=false
APP_URL=https://your-domain.com

DB_CONNECTION=sqlite
DB_DATABASE=/path/to/backend/database/database.sqlite

SESSION_DRIVER=file
SESSION_LIFETIME=120

CACHE_STORE=file
QUEUE_CONNECTION=sync
```

### 3. 初始化

```bash
cd /path/to/backend

# 创建数据库
touch database/database.sqlite

# 生成 APP_KEY
php artisan key:generate

# 运行迁移 + 填充默认管理员
php artisan migrate --seed

# 设置权限
chmod -R 755 storage bootstrap/cache
chown -R www:www storage database
```

默认管理员：`admin` / `admin123`（首次登录后务必修改）

### 4. 复制前端文件

```bash
# 把前端 dist 内容复制到 backend/public
cp -r ../frontend/dist/* public/
cp ../frontend/dist/index.html public/
```

### 5. Nginx 配置

```nginx
server {
    listen 443 ssl;
    server_name your-domain.com;
    root /path/to/backend/public;
    index index.php index.html;

    # SSL 证书配置（略）

    # PHP-FPM
    location ~ [^/]\.php(/|$) {
        fastcgi_pass unix:/tmp/php-cgi-84.sock;
        fastcgi_index index.php;
        include fastcgi.conf;
        include pathinfo.conf;
    }

    # API 路由 → Laravel
    location ~ ^/(api|admin-api) {
        try_files $uri $uri/ /index.php?$query_string;
    }

    # Vue Router history 模式
    location / {
        try_files $uri $uri/ /index.html;
    }

    # 禁止访问敏感文件
    location ~* \.(env|git|sqlite) {
        return 404;
    }
}
```

验证并重载：

```bash
nginx -t && nginx -s reload
```

### 6. 验证

- 访问 `https://your-domain.com/` — 用户登录页
- 访问 `https://your-domain.com/admin/login` — 管理员登录（admin / admin123）
- 测试 API：`curl https://your-domain.com/api/ping`

## 功能说明

### 管理员端（/admin）

- **仪表盘**：用户统计、节点状态
- **用户管理**：增删改查、分配套餐、重置流量、续费
- **节点管理**：添加 3x-ui 节点、配置入站、测试连接
- **套餐管理**：创建套餐（周期/总量）、设置价格
- **订单管理**：查看支付订单
- **设置 → 安全**：修改密码、谷歌二步验证
- **设置 → 支付**：配置支付接口

### 用户端（/）

- **登录**：邮箱密码登录 / Token 登录
- **注册**：邮箱注册（需图形验证码）
- **仪表盘**：流量统计、订阅地址、节点列表、订购订阅、最近订单

## 支付配置

1. 管理后台 → 设置 → 支付 → 新建配置
2. 填写支付平台提供的参数（商户号、网关地址、银行编码、API密钥）
3. 回调地址留空则自动生成：`https://your-domain.com/api/payment/notify`

## 更新部署

```bash
# 前端重新构建
cd frontend && npm run build

# 上传修改的文件到服务器
scp -r dist/* server:/path/backend/public/
scp backend/app/... server:/path/backend/app/...

# 清除缓存
ssh server "cd /path/backend && php artisan config:clear && php artisan route:clear"
```

## 常见问题

### 500 错误

检查 Laravel 日志：`tail -20 storage/logs/laravel.log`

### 登录后提示 "The MAC is invalid"

APP_KEY 不一致，重新运行 `php artisan key:generate` 或从本地复制 `.env` 中的 APP_KEY。

### 支付下单失败 "不存在的银行编码"

在支付平台后台查看正确的银行编码，填入支付配置。

### 图形验证码不显示

确保 PHP 安装了 `gd` 扩展。
