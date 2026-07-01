#!/bin/bash
set -e

echo "=========================================="
echo " ControlHub - Docker 启动"
echo "=========================================="

# 创建日志目录
mkdir -p /var/log/supervisor /var/log/php /var/log/nginx
chown -R www-data:www-data /var/log/php /var/log/nginx

# ============================================
# 初始化 MySQL
# ============================================
if [ ! -d "/var/lib/mysql/mysql" ]; then
    echo "[1/5] 初始化 MySQL 数据库..."
    mysqld --initialize-insecure --user=mysql
fi

# 启动 MySQL
echo "[2/5] 启动 MySQL..."
mysqld --user=mysql &
sleep 5

# 等待 MySQL 就绪
for i in $(seq 1 30); do
    if mysqladmin ping -h 127.0.0.1 --silent 2>/dev/null; then
        break
    fi
    echo "  等待 MySQL 启动... ($i/30)"
    sleep 1
done

# 创建数据库
echo "[3/5] 创建数据库..."
mysql -u root -e "CREATE DATABASE IF NOT EXISTS controlhub CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

# ============================================
# 配置 Laravel
# ============================================
cd /var/www/html/backend

# 生成 .env（如果不存在）
if [ ! -f ".env" ]; then
    echo "  创建 .env 文件..."
    cat > .env << EOF
APP_NAME=ControlHub
APP_ENV=production
APP_KEY=
APP_DEBUG=false
APP_URL=${APP_URL:-http://localhost:8080}

APP_LOCALE=zh_CN
APP_FALLBACK_LOCALE=en
APP_FAKER_LOCALE=zh_CN

BCRYPT_ROUNDS=12

LOG_CHANNEL=daily
LOG_STACK=single
LOG_LEVEL=error

DB_CONNECTION=mysql
DB_HOST=127.0.0.1
DB_PORT=3306
DB_DATABASE=controlhub
DB_USERNAME=root
DB_PASSWORD=

SESSION_DRIVER=file
SESSION_LIFETIME=120
SESSION_ENCRYPT=false
SESSION_PATH=/
SESSION_DOMAIN=null

BROADCAST_CONNECTION=log
FILESYSTEM_DISK=local
QUEUE_CONNECTION=sync

CACHE_STORE=file

MAIL_MAILER=log
MAIL_FROM_ADDRESS="noreply@example.com"
MAIL_FROM_NAME="\${APP_NAME}"

VITE_APP_NAME="\${APP_NAME}"
EOF
fi

# 生成 APP_KEY
if [ -z "$APP_KEY" ] || [ "$APP_KEY" = "" ]; then
    echo "[4/5] 生成 APP_KEY..."
    php artisan key:generate --force
else
    echo "[4/5] 使用自定义 APP_KEY..."
    sed -i "s|APP_KEY=|APP_KEY=${APP_KEY}|" .env
fi

# 设置权限
chown -R www-data:www-data storage bootstrap/cache
chmod -R 775 storage bootstrap/cache

# 运行迁移
echo "[5/5] 运行数据库迁移..."
php artisan migrate --force

# 清除缓存
php artisan config:clear
php artisan cache:clear
php artisan view:clear

# 配置 cron 任务
echo "  配置定时任务..."
echo "* * * * * cd /var/www/html/backend && php artisan schedule:run >> /dev/null 2>&1" | crontab -

echo "=========================================="
echo " 启动完成！"
echo " 访问: ${APP_URL:-http://localhost:8080}"
echo "=========================================="

# 启动 Supervisor（管理 Nginx + PHP-FPM）
exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf
