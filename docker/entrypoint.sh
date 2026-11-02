#!/bin/bash
set -e

echo "=========================================="
echo " ControlHub - Docker 启动（Go 后端）"
echo "=========================================="

mkdir -p /var/log/supervisor /var/log/nginx

# ── 初始化 MySQL ──
if [ ! -d "/var/lib/mysql/mysql" ]; then
    echo "[1/5] 初始化 MySQL 数据库..."
    mysqld --initialize-insecure --user=mysql
fi

echo "[2/5] 启动 MySQL..."
mysqld --user=mysql &
sleep 5
for i in $(seq 1 30); do
    if mysqladmin ping -h 127.0.0.1 --silent 2>/dev/null; then break; fi
    echo "  等待 MySQL 启动... ($i/30)"
    sleep 1
done

echo "[3/5] 创建数据库..."
mysql -u root -e "CREATE DATABASE IF NOT EXISTS controlhub CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# ── 生成 .env（Go 后端复用；APP_KEY base64:32字节，与 Laravel 同格式）──
# 路径与 Go 的默认一致：工作目录 /var/www/html/server，配置读 ../.env
ENV_FILE="/var/www/html/.env"
if [ -d "$ENV_FILE" ]; then
    # 挂载源文件不存在时 Docker 会把挂载点建成空目录，这里给出可操作的提示，别让它继续跑
    echo "错误: $ENV_FILE 是目录（宿主 .env 不存在时 docker 会这样创建挂载点）"
    echo "      请在仓库根目录执行: cp .env.example .env   然后重新 docker-compose up -d"
    exit 1
fi
if [ ! -f "$ENV_FILE" ]; then
    echo "[4/5] 生成 .env..."
    APP_KEY_GEN="base64:$(openssl rand -base64 32)"
    cat > "$ENV_FILE" << EOF
APP_NAME=ControlHub
APP_ENV=production
APP_KEY=${APP_KEY:-${APP_KEY_GEN}}
APP_DEBUG=false
APP_URL=${APP_URL:-http://localhost:8080}

DB_CONNECTION=mysql
DB_HOST=127.0.0.1
DB_PORT=3306
DB_DATABASE=controlhub
DB_USERNAME=root
DB_PASSWORD=

SESSION_DRIVER=database
SESSION_LIFETIME=120
EOF
fi

# ── 建表 + 默认管理员（Go 直连 MySQL，不跑 Laravel migrate）──
TABLE_COUNT=$(mysql -u root -N -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='controlhub'" 2>/dev/null || echo 0)
if [ "$TABLE_COUNT" -gt 0 ]; then
    echo "  数据库已有 ${TABLE_COUNT} 张表，跳过建表"
else
    echo "[5/5] 初始化数据库表结构..."
    mysql -u root controlhub < /var/www/html/server/schema/schema.sql
    mysql -u root controlhub < /var/www/html/server/schema/seed.sql
fi

echo "=========================================="
echo " 启动完成！"
echo " 访问: ${APP_URL:-http://localhost:8080}"
echo "=========================================="

exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf
