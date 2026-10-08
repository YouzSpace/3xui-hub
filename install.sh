#!/bin/bash
# ============================================================
# 3xui-hub 一键安装脚本
# https://github.com/YouzSpace/3xui-hub
# ============================================================

set -e

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 配置
INSTALL_DIR="/www/wwwroot/3xui-hub"
LOG_FILE="/tmp/3xui-hub-install.log"
REPO_URL="https://github.com/YouzSpace/3xui-hub.git"
VERSION="1.0.0"
# nginx 环境（detect_nginx_env 填充）：bt | standard
NGINX_ENV_TYPE=""
NGINX_CONF_DIR=""

# 日志函数
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" >> "$LOG_FILE"
}

info() {
    echo -e "${BLUE}[INFO]${NC} $1"
    log "INFO: $1"
}

success() {
    echo -e "${GREEN}[OK]${NC} $1"
    log "OK: $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
    log "WARN: $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    log "ERROR: $1"
}

error_exit() {
    error "$1"
    echo ""
    echo -e "${RED}============================================================${NC}"
    echo -e "${RED}  安装失败！${NC}"
    echo -e "${RED}============================================================${NC}"
    echo ""
    # 生成可复制的错误报告
    echo -e "${YELLOW}请复制以下信息发给开发者排查：${NC}"
    echo -e "${BLUE}---------- 复制开始 ----------${NC}"
    echo "【错误】$1"
    echo "【系统】$OS $OS_VERSION ($PKG_MANAGER)"
    echo "【架构】$(uname -m)"
    echo "【时间】$(date '+%Y-%m-%d %H:%M:%S')"
    # 显示日志最后几行关键错误
    if [ -f "$LOG_FILE" ]; then
        LAST_ERROR=$(grep -i 'error\|fatal\|fail\|denied\|not found\|No such' "$LOG_FILE" | tail -5)
        if [ -n "$LAST_ERROR" ]; then
            echo "【日志】$LAST_ERROR"
        fi
    fi
    echo -e "${BLUE}---------- 复制结束 ----------${NC}"
    echo ""
    echo -e "完整日志: ${LOG_FILE}"
    echo -e "查看命令: ${YELLOW}cat ${LOG_FILE}${NC}"
    exit 1
}

# 检测系统
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS=$ID
        OS_VERSION=$VERSION_ID
    elif [ -f /etc/centos-release ]; then
        OS="centos"
        OS_VERSION=$(grep -oE '[0-9]+\.[0-9]+' /etc/centos-release | head -1)
    else
        error_exit "不支持的操作系统"
    fi

    case $OS in
        centos|rhel|almalinux|rocky)
            PKG_MANAGER="yum"
            PHP_PKG="php"
            ;;
        ubuntu|debian)
            PKG_MANAGER="apt"
            PHP_PKG="php8.4"
            ;;
        *)
            error_exit "不支持的发行版: $OS"
            ;;
    esac

    info "检测到系统: $OS $OS_VERSION ($PKG_MANAGER)"
}

# 检测架构
detect_arch() {
    ARCH=$(uname -m)
    info "系统架构: $ARCH"
}

# 检查是否 root
check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        error_exit "请使用 root 用户运行此脚本 (sudo bash install.sh)"
    fi
}

# 检查端口
check_port() {
    if ss -tlnp | grep -q ":$1 "; then
        warn "端口 $1 已被占用"
        return 1
    fi
    return 0
}

# 安装 PHP 8.4
install_php() {
    if command -v php &>/dev/null; then
        PHP_VER=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;')
        if [ "$PHP_VER" = "8.4" ]; then
            success "PHP 8.4 已安装"
            return 0
        else
            warn "当前 PHP 版本: $PHP_VER，需要 8.4，将重新安装"
        fi
    fi

    info "安装 PHP 8.4..."

    case $PKG_MANAGER in
        yum)
            # CentOS/RHEL: 使用 Remi 仓库安装 PHP 8.4
            OS_MAJOR=${OS_VERSION%%.*}

            # 安装 EPEL（Remi 依赖它）
            info "安装 EPEL 仓库..."
            if ! rpm -q epel-release &>/dev/null; then
                dnf install -y epel-release 2>/dev/null || \
                    dnf install -y "https://dl.fedoraproject.org/pub/epel/epel-release-latest-${OS_MAJOR}.noarch.rpm" 2>/dev/null || true
            fi

            # 安装 Remi 仓库
            if ! rpm -q remi-release &>/dev/null; then
                info "安装 Remi 仓库..."
                dnf install -y "https://rpms.remirepo.net/enterprise/remi-release-${OS_MAJOR}.rpm" || \
                    error_exit "Remi 仓库安装失败"
            fi

            # CentOS 8/9: 用 module 方式
            # CentOS 10+: module 不可用，用 php84-* 包直接装
            if dnf module reset php -y 2>/dev/null && dnf module enable php:remi-8.4 -y 2>/dev/null; then
                info "通过 module 安装 PHP 8.4..."
                dnf install -y php php-fpm php-cli php-mbstring php-gd php-opcache php-pdo php-mysql php-xml php-pecl-zip php-curl
                FPM_SERVICE="php-fpm"
                FPM_CONF="/etc/php-fpm.d/www.conf"
            else
                info "通过 Remi php84 安装 PHP 8.4..."
                dnf install -y php84-php php84-php-fpm php84-php-cli php84-php-mbstring \
                    php84-php-gd php84-php-opcache php84-php-pdo php84-php-mysql \
                    php84-php-xml php84-php-pecl-zip php84-php-curl
                # 创建符号链接
                ln -sf /opt/remi/php84/root/usr/bin/php /usr/bin/php
                ln -sf /opt/remi/php84/root/usr/sbin/php-fpm /usr/sbin/php-fpm
                FPM_SERVICE="php84-php-fpm"
                FPM_CONF="/etc/opt/remi/php84/php-fpm.d/www.conf"
            fi

            # 配置 PHP-FPM
            if [ -f "$FPM_CONF" ]; then
                sed -i 's/^user = .*/user = nginx/' "$FPM_CONF"
                sed -i 's/^group = .*/group = nginx/' "$FPM_CONF"
                sed -i 's|^listen = .*|listen = /run/php-fpm/www.sock|' "$FPM_CONF"
                sed -i 's/^listen\.owner = .*/listen.owner = nginx/' "$FPM_CONF"
                sed -i 's/^listen\.group = .*/listen.group = nginx/' "$FPM_CONF"
                sed -i 's/^listen\.acl_users = .*/listen.acl_users = nginx/' "$FPM_CONF"
                # 取消注释 listen.owner/group
                sed -i 's/^;listen\.owner/listen.owner/' "$FPM_CONF"
                sed -i 's/^;listen\.group/listen.group/' "$FPM_CONF"
            fi

            mkdir -p /run/php-fpm
            systemctl enable "$FPM_SERVICE" 2>/dev/null || true
            systemctl restart "$FPM_SERVICE"
            ;;
        apt)
            apt-get update -y
            mkdir -p /etc/apt/sources.list.d
            if [ "$OS" = "debian" ]; then
                apt-get install -y apt-transport-https lsb-release ca-certificates curl gnupg
                curl -sSL https://packages.sury.org/php/apt.gpg | gpg --dearmor -o /etc/apt/trusted.gpg.d/php.gpg 2>/dev/null
                echo "deb https://packages.sury.org/php/ $(lsb_release -sc) main" > /etc/apt/sources.list.d/php.list
            else
                apt-get install -y software-properties-common
                add-apt-repository -y ppa:ondrej/php 2>/dev/null || true
            fi
            apt-get update -y
            apt-get install -y php8.4 php8.4-fpm php8.4-cli php8.4-mbstring php8.4-gd php8.4-opcache php8.4-pdo php8.4-mysql php8.4-xml php8.4-zip php8.4-curl sudo cron
            ;;
    esac

    # 配置 PHP（禁用 putenv）
    PHP_INI=$(php --ini | grep "Loaded Configuration" | awk '{print $NF}')
    if [ -n "$PHP_INI" ]; then
        sed -i 's/disable_functions = .*/disable_functions =/' "$PHP_INI" 2>/dev/null || true
    fi

    # 验证 PHP 版本
    PHP_VER=$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || echo "unknown")
    if [ "$PHP_VER" != "8.4" ]; then
        error_exit "PHP 8.4 安装失败，当前版本: $PHP_VER"
    fi

    success "PHP 8.4 安装完成"
}

# 安装 Composer
install_composer() {
    if command -v composer &>/dev/null; then
        success "Composer 已安装"
        return 0
    fi

    info "安装 Composer..."
    curl -sS https://getcomposer.org/installer | php -- --install-dir=/usr/local/bin --filename=composer 2>&1 | tee -a "$LOG_FILE"

    if ! command -v composer &>/dev/null; then
        error_exit "Composer 安装失败"
    fi

    success "Composer 安装完成"
}

# 安装 Nginx
install_nginx() {
    if command -v nginx &>/dev/null; then
        success "Nginx 已安装"
        return 0
    fi

    info "安装 Nginx..."
    case $PKG_MANAGER in
        yum)
            yum install -y nginx
            ;;
        apt)
            apt-get install -y nginx
            ;;
    esac

    systemctl enable nginx
    systemctl start nginx

    success "Nginx 安装完成"
}

# 安装 MySQL
install_mysql() {
    if command -v mysql &>/dev/null; then
        MYSQL_VER=$(mysql --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -1)
        success "MySQL 已安装 (v$MYSQL_VER)"
        # 确保数据目录存在
        if [ ! -d /var/lib/mysql/mysql ]; then
            info "初始化数据库..."
            mysql_install_db --user=mysql 2>/dev/null || mariadb-install-db --user=mysql 2>/dev/null || true
        fi
        # 确保服务运行
        systemctl start mariadb 2>/dev/null || systemctl start mysql 2>/dev/null || true
        return 0
    fi

    info "安装 MySQL..."

    case $PKG_MANAGER in
        yum)
            dnf install -y mysql mysql-server 2>/dev/null || \
                yum install -y mysql mysql-server
            systemctl enable mysqld 2>/dev/null || systemctl enable mysql 2>/dev/null || true
            systemctl start mysqld 2>/dev/null || systemctl start mysql 2>/dev/null || true
            ;;
        apt)
            # 检测是否能直接安装 mysql-server（用 dry-run 检查）
            if apt-get install --dry-run -y mysql-server &>/dev/null; then
                info "从 APT 仓库安装 MySQL..."
                DEBIAN_FRONTEND=noninteractive apt-get install -y mysql-server
            else
                # 默认仓库没有 mysql-server，添加 MySQL 官方仓库
                info "添加 MySQL 官方 APT 仓库..."
                apt-get install -y wget gnupg2 lsb-release ca-certificates 2>/dev/null || true
                
                MYSQL_REPO_OK=false
                # 下载 MySQL APT 配置包
                MYSQL_APT_DEB="mysql-apt-config_0.8.32-1_all.deb"
                if wget -q --timeout=15 "https://dev.mysql.com/get/${MYSQL_APT_DEB}" -O "/tmp/${MYSQL_APT_DEB}" 2>/dev/null || \
                   wget -q --timeout=15 "https://repo.mysql.com/${MYSQL_APT_DEB}" -O "/tmp/${MYSQL_APT_DEB}" 2>/dev/null; then
                    DEBIAN_FRONTEND=noninteractive dpkg -i "/tmp/${MYSQL_APT_DEB}" 2>/dev/null || true
                    apt-get update -qq 2>/dev/null || true
                    if apt-get install --dry-run -y mysql-server &>/dev/null; then
                        DEBIAN_FRONTEND=noninteractive apt-get install -y mysql-server
                        MYSQL_REPO_OK=true
                    fi
                    rm -f "/tmp/${MYSQL_APT_DEB}"
                fi
                
                if [ "$MYSQL_REPO_OK" = false ]; then
                    # MySQL 仓库不可用，使用 MariaDB 作为替代
                    warn "无法安装 MySQL，使用 MariaDB 替代（兼容 MySQL）..."
                    DEBIAN_FRONTEND=noninteractive apt-get install -y mariadb-server
                fi
            fi
            systemctl enable mysql 2>/dev/null || systemctl enable mariadb 2>/dev/null || true
            systemctl start mysql 2>/dev/null || systemctl start mariadb 2>/dev/null || true
            ;;
    esac

    # 安全初始化（设置 root 空密码，允许 TCP 连接）
    info "配置数据库认证..."
    # 兼容 MySQL 和 MariaDB 的认证方式
    mysql -u root -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH mysql_native_password BY ''; FLUSH PRIVILEGES;" 2>/dev/null || \
        mysql -u root -e "ALTER USER 'root'@'localhost' IDENTIFIED VIA mysql_native_password USING ''; FLUSH PRIVILEGES;" 2>/dev/null || \
        mysql -u root -e "SET PASSWORD FOR 'root'@'localhost' = PASSWORD(''); FLUSH PRIVILEGES;" 2>/dev/null || true

    if ! command -v mysql &>/dev/null; then
        error_exit "MySQL 安装失败"
    fi

    success "MySQL 安装完成"
}

# 部署项目
deploy_project() {
    info "部署项目..."

    # 创建目录
    mkdir -p "$INSTALL_DIR"

    # 克隆项目（国内镜像加速 + 浅克隆）
    if [ -d "$INSTALL_DIR/.git" ]; then
        info "更新现有项目..."
        cd "$INSTALL_DIR"
        git pull 2>&1 | tee -a "$LOG_FILE"
    else
        info "下载项目（浅克隆，体积最小化）..."

        # 国内镜像列表，按优先级尝试
        MIRRORS=(
            "https://ghfast.top/https://github.com/YouzSpace/3xui-hub.git"
            "https://ghproxy.net/https://github.com/YouzSpace/3xui-hub.git"
            "https://github.com/YouzSpace/3xui-hub.git"
        )

        CLONED=false
        for MIRROR_URL in "${MIRRORS[@]}"; do
            info "尝试: ${MIRROR_URL%%://*}..."
            if git clone --depth 1 --single-branch --branch main "$MIRROR_URL" "$INSTALL_DIR" 2>&1 | tee -a "$LOG_FILE"; then
                CLONED=true
                break
            fi
            warn "失败，尝试下一个..."
            rm -rf "$INSTALL_DIR" 2>/dev/null
        done

        if [ "$CLONED" = false ]; then
            error_exit "所有下载源均失败，请检查网络"
        fi
    fi

    cd "$INSTALL_DIR"

    # 检查必要文件
    if [ ! -f "backend/artisan" ]; then
        error_exit "项目文件不完整，请检查网络"
    fi

    success "项目部署完成"
}

# 配置环境
setup_env() {
    info "配置环境..."

    cd "$INSTALL_DIR/backend"
    # 安装 PHP 依赖（使用国内镜像加速）
    info "安装 Composer 依赖..."
    composer config -g repos.packagist composer https://mirrors.aliyun.com/composer/ 2>/dev/null || true
    COMPOSER_ALLOW_SUPERUSER=1 composer install --no-dev --optimize-autoloader 2>&1 | tee -a "$LOG_FILE" || error_exit "Composer 依赖安装失败"

    # 生成 .env（始终使用自定义配置，不用 .env.example）
    cat > .env << EOF
APP_NAME=ControlHub
APP_ENV=production
APP_KEY=
APP_DEBUG=false
APP_URL=http://localhost

DB_CONNECTION=mysql
DB_HOST=127.0.0.1
DB_PORT=3306
DB_DATABASE=controlhub
DB_USERNAME=root
DB_PASSWORD=

SESSION_DRIVER=database
SESSION_LIFETIME=120

CACHE_STORE=file
QUEUE_CONNECTION=database
DB_QUEUE_RETRY_AFTER=180

# 节点类 Job（新建节点初始化/扫描/流量同步）派到独立队列 node-ops，
# 避免它们把定时任务（封禁检查、健康检查）堵在 default 队列后面。
# 必须与 setup_node_ops_worker 里起的 node-ops worker 配套：只开一边的任务会丢（见该函数注释）。
PANEL_NODE_OPS_QUEUE=node-ops
EOF

    # 生成 APP_KEY
    info "生成 APP_KEY..."
    php artisan key:generate 2>&1 | tee -a "$LOG_FILE" || error_exit "APP_KEY 生成失败"

    # 设置 .env 权限（运行用户只读）
    NGINX_USER=$(ps -eo user,comm | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}
    chown "$NGINX_USER":"$NGINX_USER" .env 2>/dev/null || true
    chmod 640 .env 2>/dev/null || true

    # 创建 MySQL 数据库
    if command -v mysql &>/dev/null; then
        mysql -u root -e "CREATE DATABASE IF NOT EXISTS controlhub CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" 2>&1 | tee -a "$LOG_FILE" || warn "MySQL 数据库创建失败，请手动创建"
    else
        warn "mysql 命令不可用，请手动创建 MySQL 数据库: controlhub"
    fi

    # 运行迁移
    info "运行数据库迁移..."
    php artisan migrate --force 2>&1 | tee -a "$LOG_FILE" || error_exit "数据库迁移失败"

    # 填充默认管理员
    info "填充默认数据..."
    php artisan db:seed --force 2>&1 | tee -a "$LOG_FILE" || error_exit "数据填充失败"

    # 设置权限（确保 storage 目录存在）
    mkdir -p storage/{app/public,framework/{cache/data,sessions,testing,views},logs}
    chmod -R 775 storage bootstrap/cache
    # 自动检测 Nginx worker 用户
    NGINX_USER=$(ps -eo user,comm | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}
    chown -R "$NGINX_USER":"$NGINX_USER" storage bootstrap/cache 2>/dev/null || true
    # 导入备份需要在 backend/ 目录本身创建 .env.bak（BackupController 的 File::copy）
    chown "$NGINX_USER":"$NGINX_USER" . 2>/dev/null || true

    success "环境配置完成"
}

# 检测 PHP-FPM socket 路径
detect_fpm_sock() {
    # 按优先级查找 socket 文件
    local SOCK_PATHS=(
        "/run/php/php8.4-fpm.sock"
        "/run/php-fpm/www.sock"
        "/var/run/php-fpm/www.sock"
        "/var/opt/remi/php84/run/php-fpm/www.sock"
        "/tmp/php-cgi-84.sock"
    )

    for sock in "${SOCK_PATHS[@]}"; do
        if [ -S "$sock" ]; then
            echo "$sock"
            return 0
        fi
    done

    # 全局搜索
    FOUND=$(find /run /var/run /tmp -name "*.sock" 2>/dev/null | grep -i php | head -1 || true)
    if [ -n "$FOUND" ]; then
        echo "$FOUND"
        return 0
    fi

    # 兜底默认值
    echo "/run/php/php8.4-fpm.sock"
    return 0
}

# 检测 nginx 环境（宝塔 vs 标准），设置 conf 目录变量
# 宝塔：/www/server/panel 或 /www/server/nginx 存在 → vhost 目录
# 标准：否则 → /etc/nginx/conf.d/
detect_nginx_env() {
    if [ -d /www/server/panel ] || [ -d /www/server/nginx ]; then
        NGINX_ENV_TYPE="bt"
        NGINX_CONF_DIR="${NGINX_CONF_DIR:-/www/server/panel/vhost/nginx/}"
    else
        NGINX_ENV_TYPE="standard"
        NGINX_CONF_DIR="${NGINX_CONF_DIR:-/etc/nginx/conf.d/}"
    fi
    info "Nginx 环境: ${NGINX_ENV_TYPE} (conf 目录: ${NGINX_CONF_DIR})"
}

# 配置 Nginx
setup_nginx() {
    info "配置 Nginx..."

    detect_nginx_env

    # 检查是否已有 Nginx 配置（避免覆盖用户的 SSL 配置）
    NGINX_CONF="${NGINX_CONF_DIR}3xui-hub.conf"
    if [ -f "$NGINX_CONF" ]; then
        # 检查是否已有 SSL 配置
        if grep -q "listen 443 ssl" "$NGINX_CONF" 2>/dev/null; then
            info "检测到已有 SSL 配置，跳过 Nginx 配置覆盖"
            # 只更新 APP_URL（保留域名）
            DOMAIN=$(grep -oP 'server_name \K[^;]+' "$NGINX_CONF" | head -1)
            if [ -n "$DOMAIN" ] && [ "$DOMAIN" != "_" ]; then
                PROTO="https"
                sed -i "s|APP_URL=.*|APP_URL=${PROTO}://${DOMAIN}|" "$INSTALL_DIR/backend/.env"
                info "APP_URL 已更新为: ${PROTO}://${DOMAIN}"
            fi
            # 把现有 nginx server_name 回填 domains 表（多域名：第一个设主域）
            backfill_domains_from_nginx "$NGINX_CONF"
            return 0
        fi
    fi

    # 默认使用 IP
    DOMAIN=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")
    info "使用 IP: $DOMAIN"

    # 设置 APP_URL
    sed -i "s|APP_URL=.*|APP_URL=http://${DOMAIN}|" "$INSTALL_DIR/backend/.env"

    # 确保 PHP-FPM 运行
    systemctl start php8.4-fpm 2>/dev/null || systemctl start php-fpm 2>/dev/null || true

    # 检测 PHP-FPM socket
    FPM_SOCK=$(detect_fpm_sock)
    info "PHP-FPM socket: $FPM_SOCK"

    # 生成 Nginx 配置
    NGINX_CONF="${NGINX_CONF_DIR}3xui-hub.conf"
    mkdir -p "$NGINX_CONF_DIR"

    if [ "$SSL_ENABLED" = true ]; then
        cat > "$NGINX_CONF" << NGINX
server {
    listen 80;
    server_name ${DOMAIN};
    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl http2;
    server_name ${DOMAIN};
    root ${INSTALL_DIR}/backend/public;
    index index.html index.php;

    ssl_certificate /etc/nginx/ssl/${DOMAIN}.pem;
    ssl_certificate_key /etc/nginx/ssl/${DOMAIN}.key;
    ssl_protocols TLSv1.2 TLSv1.3;

    location ~ [^/]\\.php(/|$) {
        fastcgi_pass unix:${FPM_SOCK};
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name;
    }

    # xray 节点 WS 长连接（agent 主通道；连不上时 agent 自动降级 HTTP）
    location /node-ws {
        proxy_pass http://127.0.0.1:8091;
        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    # xray 节点资产与安装脚本（转 PHP；否则被下方 SPA fallback 吞掉，节点机下载拿到 HTML）
    location ~ ^/(node-install\.sh|node-agent\.sh|node-bin/) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location ~ ^/(api|admin-api) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location / {
        try_files \$uri \$uri/ /index.html;
    }

    location ~* \\.(env|git) {
        return 404;
    }
}
NGINX
    else
        cat > "$NGINX_CONF" << NGINX
server {
    listen 80;
    server_name ${DOMAIN};
    root ${INSTALL_DIR}/backend/public;
    index index.html index.php;

    location ~ [^/]\\.php(/|$) {
        fastcgi_pass unix:${FPM_SOCK};
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name;
    }

    # xray 节点 WS 长连接（agent 主通道；连不上时 agent 自动降级 HTTP）
    location /node-ws {
        proxy_pass http://127.0.0.1:8091;
        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    # xray 节点资产与安装脚本（转 PHP；否则被下方 SPA fallback 吞掉，节点机下载拿到 HTML）
    location ~ ^/(node-install\.sh|node-agent\.sh|node-bin/) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location ~ ^/(api|admin-api) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location / {
        try_files \$uri \$uri/ /index.html;
    }

    location ~* \\.(env|git) {
        return 404;
    }
}
NGINX
    fi

    # 复制前端文件到 public
    cp -r "$INSTALL_DIR/frontend/dist/"* "$INSTALL_DIR/backend/public/" 2>/dev/null || true

    # 删除默认站点（避免冲突）
    rm -f /etc/nginx/sites-enabled/default 2>/dev/null || true

    # 测试并重载 Nginx
    nginx -t 2>&1 | tee -a "$LOG_FILE" || error_exit "Nginx 配置错误"
    systemctl reload nginx 2>/dev/null || systemctl start nginx 2>/dev/null || true

    # 确保 PHP-FPM 运行
    systemctl restart php8.4-fpm 2>/dev/null || systemctl restart php-fpm 2>/dev/null || true

    success "Nginx 配置完成"
}

# 配置 cron（调度器：流量同步 / 封禁检查 / 月度重置 / 异步任务超时）
# 真正的实现在 3hub 的 ensure_artisan_cron()，这里调同一个入口，不再自己维护一份，
# 免得安装脚本和 3hub 各写一份后漂移（更新路径那份曾经没人管，线上 root crontab
# 被其它工具覆盖后一直没补回来）。走子进程调用而不是 source：3hub 是可执行命令，
# 里面定义了同名常量（INSTALL_DIR/VERSION），source 进安装脚本会污染后续步骤的环境。
setup_cron() {
    info "配置定时任务（流量自动同步）..."

    if [ ! -f "$INSTALL_DIR/3hub" ]; then
        warn "未找到 $INSTALL_DIR/3hub，跳过定时任务配置（安装完成后运行 3hub sync-status 可自检）"
        return 0
    fi

    if bash "$INSTALL_DIR/3hub" __ensure-cron; then
        success "定时任务已就绪（schedule:run 每分钟，以网站运行用户执行）"
    else
        warn "定时任务写入失败，请运行 3hub sync-status 自检，或手工 crontab -e 添加"
    fi
}

# 配置常驻队列 Worker（后台流量同步）
setup_queue_worker() {
    info "配置后台任务 Worker..."

    NGINX_USER=$(ps -eo user,comm | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}

    cat > /etc/systemd/system/3xui-hub-queue.service << EOF
[Unit]
Description=3xui-hub Queue Worker
After=network.target mysql.service mariadb.service

[Service]
Type=simple
User=${NGINX_USER}
Group=${NGINX_USER}
WorkingDirectory=${INSTALL_DIR}/backend
ExecStart=/usr/bin/php artisan queue:work database --sleep=2 --tries=5 --timeout=120 --max-time=3600
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=130

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable --now 3xui-hub-queue.service
    systemctl is-active --quiet 3xui-hub-queue.service || error_exit "后台任务 Worker 启动失败"
    success "后台任务 Worker 已启动"
}

# 配置节点任务专用队列 Worker（node-ops，2 实例）
#
# 为什么要有第二条队列：
#   一条 `queue:work` 进程同一时刻只跑一个 Job（单线程）。节点任务（如新建节点初始化，
#   一次派 100+ 个 Job）会把定时任务（封禁检查、健康检查）的 Job 堵在同一条队列后面，
#   导致封禁/健康检查延迟几分钟到几十分钟。拆成两条队列后各排各的，互不影响。
#   default 队列仍由上面的 setup_queue_worker 负责，那条不加 --queue（默认就是 default）。
#
# 下面三段的顺序不能调换，理由见各段注释。
setup_node_ops_worker() {
    info "配置节点任务 Worker (node-ops)..."

    NGINX_USER=$(ps -eo user,comm | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}

    # ---------- 1. 先写 .env：让节点类 Job 派到 node-ops ----------
    # 必须排在启动 worker 之前：反过来的话会先有一段「worker 在监听 node-ops、
    # 但 Job 还往 default 派」的空转窗口，人工排查时很难判断到底生效没有。
    # 幂等：已有该行就改写成 node-ops，没有才追加，绝不重复追加
    #（dotenv 同名变量以最后一行为准，重复追加会让旧值留在文件里，看着像没生效）。
    if grep -q '^PANEL_NODE_OPS_QUEUE=' "${INSTALL_DIR}/backend/.env"; then
        sed -i 's/^PANEL_NODE_OPS_QUEUE=.*/PANEL_NODE_OPS_QUEUE=node-ops/' "${INSTALL_DIR}/backend/.env"
    else
        echo 'PANEL_NODE_OPS_QUEUE=node-ops' >> "${INSTALL_DIR}/backend/.env"
    fi

    # ---------- 2. 安装并启动 node-ops worker ----------
    # 用模板单元 + 实例编号（@1/@2）而不是两份独立的服务文件：
    # 命令行只写一份，实例 1 和 2 共用，杜绝「两份文件只改了一份」导致某个实例漏掉
    # --queue=node-ops 而去和 default worker 抢同一条队列（那样等于白开一个进程）。
    # 加减实例也只需 systemctl enable/disable，不用重新生成文件。
    cat > /etc/systemd/system/3xui-hub-queue-node@.service << EOF
[Unit]
Description=3xui-hub Node Ops Queue Worker (instance %i)
After=network.target mysql.service mariadb.service

[Service]
Type=simple
User=${NGINX_USER}
Group=${NGINX_USER}
WorkingDirectory=${INSTALL_DIR}/backend
ExecStart=/usr/bin/php artisan queue:work database --queue=node-ops --sleep=2 --tries=5 --timeout=120 --max-time=3600
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=130
# 小机器（1 核/473MB 这类）上让队列任务给 Web 请求让路：Nice 降优先级、
# CPUWeight 压低 cgroup 权重。这里只用优先级手段，不设内存硬上限 ——
# 硬上限会在内存吃紧时把 worker 杀掉，一个跑到一半的节点初始化任务会中断重跑。
Nice=10
CPUWeight=20

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    # enable --now 失败不在这里中断：交给下面第 3 步自检统一报错。
    # 本脚本是 set -e，让 enable 自己失败会变成一句没有上下文的退出，
    # 而 error_exit 会带上系统信息、日志尾巴和可复制给开发者的报告。
    for i in 1 2; do
        systemctl enable --now "3xui-hub-queue-node@${i}.service" || true
    done

    # ---------- 3. 启动后自检 ----------
    # 为什么必须自检：.env 里开了 PANEL_NODE_OPS_QUEUE=node-ops、但没有 worker 监听
    # node-ops 时，节点类 Job 会被派进一条没人消费的队列 —— 任务永远不执行，而且不报错，
    # 面板上一直停在 pending 直到被判超时失败。这比「两类任务抢一条队列」严重得多，
    # 所以「两边都到位」才算装好，这里检查不过就直接中断安装，而不是留下一个静默坏掉的面板。
    # active 和 enabled 都要查：active 只证明「现在在跑」，enabled 才保证重启后还在
    #（enable 失败但被 --now 拉起来的边界情况，只查 active 会漏过去）。
    for i in 1 2; do
        systemctl is-active --quiet "3xui-hub-queue-node@${i}.service" || \
            error_exit "节点任务 Worker (实例 ${i}) 未在运行，请查看: systemctl status 3xui-hub-queue-node@${i}.service"
        systemctl is-enabled --quiet "3xui-hub-queue-node@${i}.service" || \
            error_exit "节点任务 Worker (实例 ${i}) 未设置开机自启，请执行: systemctl enable 3xui-hub-queue-node@${i}.service"
    done
    success "节点任务 Worker 已启动（node-ops 队列，2 实例）"
}

# 配置 xray 节点 WS 长连接服务（agent 主通道；nginx 反代 /node-ws → 127.0.0.1:8091）
#
# 为什么单开一个服务：xray 节点 agent 与面板之间用 WebSocket 长连接做实时通道
# （指令秒级下发 / 配置变更即时通知），Workerman 常驻进程由 systemd 看护。
# 连不上 WS 时 agent 会自动降级到 HTTP 六端点通道，面板核心功能不依赖本服务，
# 因此启动失败只告警不阻断安装。
setup_node_ws() {
    info "配置节点 WS 长连接服务..."

    NGINX_USER=$(ps -eo user,comm | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}

    cat > /etc/systemd/system/3xui-hub-node-ws.service << EOF
[Unit]
Description=3xui-hub Node WebSocket Server (xray nodes)
After=network.target mysql.service mariadb.service

[Service]
Type=simple
User=${NGINX_USER}
Group=${NGINX_USER}
WorkingDirectory=${INSTALL_DIR}/backend
ExecStart=/usr/bin/php artisan node:ws-serve
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable --now 3xui-hub-node-ws.service 2>/dev/null || true

    if systemctl is-active --quiet 3xui-hub-node-ws.service; then
        success "节点 WS 服务已启动"
    else
        warn "节点 WS 服务未启动（节点将使用 HTTP 降级通道，不影响功能；排查: systemctl status 3xui-hub-node-ws.service）"
    fi
}

# 修复 storage / bootstrap/cache 属主（幂等自愈）
#
# 为什么安装末尾要单独修一遍：任何以 root 身份跑过 artisan / composer 的动作，都会在这两个
# 目录里留下 root 属主文件 —— 本脚本 setup_env 里的 composer install（会以 root 触发
# package:discover，往 bootstrap/cache 写 packages.php / services.php）、key:generate、
# migrate、db:seed 全都是 root 跑的。装完之后以网站运行用户跑的调度器 / 队列 worker 再往
# storage/framework/cache 写缓存就 Permission denied，schedule:run 静默失败 → 定时任务全停
# + 面板显示 worker 离线（BanCheckJob 不再写心跳缓存）。所以装完必须把属主收回来。
#
# 安静且幂等：先扫一遍有没有非目标属主的文件，没有就一声不响地返回，不做多余动作。
# bootstrap/cache 不能漏 —— composer 的 root 属主文件正写在那里。
fix_storage_ownership() {
    local NGINX_USER BAD SCAN_RC=0 d
    local -a TARGETS=()
    local BACKEND_DIR="${INSTALL_DIR}/backend"

    # 与 setup_env / setup_queue_worker 同一套判定：nginx worker 用户，兜底 www-data
    NGINX_USER=$(ps -eo user,comm 2>/dev/null | grep nginx | awk '{print $1}' | grep -v root | head -1)
    NGINX_USER=${NGINX_USER:-www-data}

    for d in "$BACKEND_DIR/storage" "$BACKEND_DIR/bootstrap/cache"; do
        if [ -d "$d" ]; then
            TARGETS+=("$d")
        fi
    done
    if [ ${#TARGETS[@]} -eq 0 ]; then
        return 0
    fi

    # -print -quit：撞见第一个属主不对的就停，只有干净时才会走完整棵树
    BAD=$(find "${TARGETS[@]}" ! -user "$NGINX_USER" -print -quit 2>/dev/null) || SCAN_RC=$?

    # 干净：不动、不吭声。扫不出结果（权限不足 / 该用户不存在）时按「脏」处理 ——
    # 宁可多做一次幂等的 chown，也不要静默放过一次真实的污染。
    if [ "$SCAN_RC" -eq 0 ] && [ -z "$BAD" ]; then
        return 0
    fi

    # 非 root 改不了属主，chown 只会失败刷屏；说明原因后直接返回，不动任何文件
    if [ "$(id -u)" -ne 0 ]; then
        warn "存在非 ${NGINX_USER} 属主的缓存文件（如 ${BAD:-扫描失败}），但当前不是 root，跳过属主修复；请以 root 执行: chown -R ${NGINX_USER}:${NGINX_USER} ${TARGETS[*]}"
        return 0
    fi

    if chown -R "$NGINX_USER":"$NGINX_USER" "${TARGETS[@]}" 2>/dev/null; then
        info "已修复 storage/bootstrap/cache 属主（此前存在非 ${NGINX_USER} 属主文件，如 ${BAD:-扫描失败}）"
    else
        # 修不好也不中断安装：属主问题比起前面报过的错要轻，留提示让人工收尾即可
        warn "修复 storage/bootstrap/cache 属主失败，请以 root 手工执行: chown -R ${NGINX_USER}:${NGINX_USER} ${TARGETS[*]}"
    fi
    return 0
}

# 安装 3hub 命令
install_3hub() {
    info "安装 3hub 管理命令..."

    cp "$INSTALL_DIR/3hub" /usr/local/bin/3hub
    chmod +x /usr/local/bin/3hub
    # 安装目录里那份也补 x 位：仓库里 3hub 存的是 644（git ls-files -s 3hub 可查），
    # clone 下来不可执行，用户直接敲 ./3hub 会 Permission denied
    chmod +x "$INSTALL_DIR/3hub"

    success "3hub 命令安装完成"
}

# 解析 nginx conf 的 server_name → 逐个 INSERT IGNORE 进 domains 表（第一个设主域）
# 与本脚本其它 DB 访问一致：mysql -u root controlhub
# conf 来源按环境检测：宝塔 vhost 目录 / 标准 conf.d，不再写死单一文件路径
# 可选参数 $1：指定 conf 文件；缺省扫描 ${NGINX_CONF_DIR} 下所有 *.conf
backfill_domains_from_nginx() {
    local SRC_FILE="${1:-}"
    local CONF_DOMAINS FIRST IS_PRIMARY d SCAN_DESC

    if ! command -v mysql &>/dev/null; then
        warn "mysql 命令不可用，跳过 domains 表回填"
        return 0
    fi

    if [ -z "$NGINX_CONF_DIR" ]; then
        detect_nginx_env
    fi

    # server_name 可能一行多个；去掉空值与默认占位 "_"，统一小写去重
    if [ -n "$SRC_FILE" ] && [ -f "$SRC_FILE" ]; then
        SCAN_DESC="$SRC_FILE"
        CONF_DOMAINS=$(grep -oP 'server_name \K[^;]+' "$SRC_FILE" 2>/dev/null \
            | tr ' ,\t' '\n' \
            | sed 's/;$//' \
            | tr '[:upper:]' '[:lower:]' \
            | grep -Ev '^$|^_$' \
            | awk '!seen[$0]++' || true)
    else
        SCAN_DESC="${NGINX_CONF_DIR}*.conf"
        CONF_DOMAINS=$(grep -h -oP 'server_name \K[^;]+' "${NGINX_CONF_DIR}"*.conf 2>/dev/null \
            | tr ' ,\t' '\n' \
            | sed 's/;$//' \
            | tr '[:upper:]' '[:lower:]' \
            | grep -Ev '^$|^_$' \
            | awk '!seen[$0]++' || true)
    fi

    if [ -z "$CONF_DOMAINS" ]; then
        info "nginx conf 中未解析到 server_name，跳过 domains 表回填"
        return 0
    fi

    info "回填 domains 表（来源: ${SCAN_DESC} 的 server_name）..."
    FIRST=1
    for d in $CONF_DOMAINS; do
        if [ "$FIRST" = "1" ]; then
            IS_PRIMARY=1
            FIRST=0
        else
            IS_PRIMARY=0
        fi
        mysql -u root controlhub -e \
            "INSERT IGNORE INTO domains (domain, is_primary, enabled, ssl_status, created_at, updated_at) VALUES ('${d}', ${IS_PRIMARY}, 1, 'ok', NOW(), NOW());" \
            2>/dev/null || warn "domains 回填失败: ${d}"
    done
    success "domains 表回填完成"
}

# 安装多域名助手（3hub-domain + 受限 sudoers）
# 幂等：已存在则覆盖更新。装 sudoers 前必须 visudo 校验，失败则不装（防止锁死服务器）
install_domain_support() {
    info "安装多域名助手 (3hub-domain)..."

    if [ ! -f "$INSTALL_DIR/3hub-domain" ] || [ ! -f "$INSTALL_DIR/sudoers-3xui-hub" ]; then
        warn "未找到 3hub-domain / sudoers-3xui-hub，跳过多域名助手安装"
        return 0
    fi

    cp "$INSTALL_DIR/3hub-domain" /usr/local/bin/3hub-domain
    chmod +x /usr/local/bin/3hub-domain
    chown root:root /usr/local/bin/3hub-domain

    # 装 sudoers 前先校验语法，失败则不装并报错
    if ! visudo -cf "$INSTALL_DIR/sudoers-3xui-hub" >/dev/null 2>&1; then
        error "sudoers-3xui-hub 校验失败，已跳过安装 /etc/sudoers.d/3xui-hub（防止写坏 sudoers 锁死服务器）"
        error "请人工检查: $INSTALL_DIR/sudoers-3xui-hub"
        return 0
    fi

    cp "$INSTALL_DIR/sudoers-3xui-hub" /etc/sudoers.d/3xui-hub
    chmod 0440 /etc/sudoers.d/3xui-hub
    chown root:root /etc/sudoers.d/3xui-hub

    success "多域名助手安装完成 (3hub-domain + sudoers)"
}

# 输出安装结果
show_result() {
    PROTOCOL="http"
    if [ "$SSL_ENABLED" = true ]; then
        PROTOCOL="https"
    fi

    echo ""
    echo -e "${GREEN}============================================================${NC}"
    echo -e "${GREEN}  安装完成！${NC}"
    echo -e "${GREEN}============================================================${NC}"
    echo ""
    echo -e "  访问地址:  ${BLUE}${PROTOCOL}://${DOMAIN}/${NC}"
    echo -e "  管理后台:  ${BLUE}${PROTOCOL}://${DOMAIN}/admin/login${NC}"
    echo ""
    echo -e "  管理员账号:  ${YELLOW}admin${NC}"
    echo -e "  管理员密码:  ${YELLOW}admin123${NC}"
    echo ""
    echo -e "  ${RED}首次登录后请立即修改密码！${NC}"
    echo ""
    echo -e "  绑定域名/SSL:  ${BLUE}3hub ssl${NC}"
    echo -e "  管理命令:  ${BLUE}3hub${NC} 查看所有可用命令"
    echo ""
    echo -e "${GREEN}============================================================${NC}"
}

# 主流程
main() {
    echo -e "${BLUE}"
    echo "  ____  _____ _   _ ____  _   _ _   _ __  __ _____   ____  _   _ "
    echo " / ___|| ____| \\ | |  _ \\| | | | \\ | |  \\/  | ____| | __ )| | | |"
    echo " \\___ \\|  _| |  \\| | |_) | | | |  \\| | |\\/| |  _|   |  _ \\| | | |"
    echo "  ___) | |___| |\\  |  __/| |_| | |\\  | |  | | |___  | |_) | |_| |"
    echo " |____/|_____|_| \\_|_|    \\___/|_| \\_|_|  |_|_____| |____/ \\___/ "
    echo -e "${NC}"
    echo "  3x-ui 订阅管理中枢 · 一键安装脚本 v${VERSION}"
    echo ""

    # 初始化日志
    echo "=== 3xui-hub 安装日志 $(date) ===" > "$LOG_FILE"

    # 检查 root
    check_root

    # 检测系统
    detect_os
    detect_arch

    # 安装依赖
    install_php
    install_composer
    install_nginx

    # 确保 git 可用
    if ! command -v git &>/dev/null; then
        info "安装 Git..."
        case $PKG_MANAGER in
            yum) yum install -y git ;;
            apt) apt-get install -y git ;;
        esac
    fi

    # 安装 MySQL
    install_mysql

    # 部署项目
    deploy_project

    # 配置环境
    setup_env

    # 配置 Nginx
    setup_nginx

    # 配置 cron
    setup_cron

    # 配置后台任务 Worker（default 队列：定时任务类 Job）
    setup_queue_worker

    # 配置节点任务 Worker（node-ops 队列）
    # 必须排在 setup_env（写 .env）之后：它要先确认 .env 里的 PANEL_NODE_OPS_QUEUE
    # 已生效再起 worker，顺序反了就成了「worker 监听一条没人投递的队列」
    setup_node_ops_worker

    # 配置节点 WS 长连接服务（xray 节点 agent 实时通道）
    setup_node_ws

    # 安装 3hub 命令
    install_3hub

    # 安装多域名助手（3hub-domain + sudoers）
    install_domain_support

    # 属主自愈：必须排在所有以 root 身份跑过 artisan / composer 的步骤之后
    #（composer install 的 package:discover、key:generate / migrate / db:seed 都会往
    # storage 和 bootstrap/cache 写 root 属主文件），否则网站运行用户一写缓存就
    # Permission denied，queue / schedule 静默失效 —— 详见函数注释
    fix_storage_ownership

    # 输出结果
    show_result
}

main "$@"
