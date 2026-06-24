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
    echo -e "${RED}安装失败！${NC} 查看日志: ${LOG_FILE}"
    echo -e "常见修复方法:"
    echo -e "  1. 检查网络连接"
    echo -e "  2. 确保有 root 权限"
    echo -e "  3. 查看日志: cat ${LOG_FILE}"
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
            warn "当前 PHP 版本: $PHP_VER，需要 8.4"
        fi
    fi

    info "安装 PHP 8.4..."

    case $PKG_MANAGER in
        yum)
            # 安装 EPEL 和 Remi 仓库
            yum install -y epel-release 2>/dev/null || true
            yum install -y https://rpms.remirepo.net/enterprise/remi-release-${OS_VERSION%%.*}.rpm 2>/dev/null || true
            yum module reset php -y 2>/dev/null || true
            yum module enable php:remi-8.4 -y 2>/dev/null || true
            yum install -y php php-fpm php-cli php-mbstring php-fileinfo php-gd php-opcache php-pdo php-sqlite3 php-xml php-zip php-curl
            ;;
        apt)
            apt-get update -y
            if [ "$OS" = "debian" ]; then
                apt-get install -y apt-transport-https lsb-release ca-certificates curl gnupg
                curl -sSL https://packages.sury.org/php/apt.gpg | gpg --dearmor -o /etc/apt/trusted.gpg.d/php.gpg 2>/dev/null
                echo "deb https://packages.sury.org/php/ $(lsb_release -sc) main" > /etc/apt/sources.list.d/php.list
            else
                apt-get install -y software-properties-common
                add-apt-repository -y ppa:ondrej/php 2>/dev/null || true
            fi
            apt-get update -y
            apt-get install -y php8.4 php8.4-fpm php8.4-cli php8.4-mbstring php8.4-fileinfo php8.4-gd php8.4-opcache php8.4-pdo php8.4-sqlite3 php8.4-xml php8.4-zip php8.4-curl
            ;;
    esac

    # 配置 PHP（禁用 putenv）
    PHP_INI=$(php --ini | grep "Loaded Configuration" | awk '{print $NF}')
    if [ -n "$PHP_INI" ]; then
        sed -i 's/disable_functions = .*/disable_functions =/' "$PHP_INI" 2>/dev/null || true
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

    # 生成 .env（始终使用自定义配置，不用 .env.example）
    cat > .env << EOF
APP_NAME=ControlHub
APP_ENV=production
APP_KEY=
APP_DEBUG=false
APP_URL=http://localhost

DB_CONNECTION=sqlite
DB_DATABASE=${INSTALL_DIR}/backend/database/database.sqlite

SESSION_DRIVER=file
SESSION_LIFETIME=120

CACHE_STORE=file
QUEUE_CONNECTION=sync
EOF

    # 生成 APP_KEY
    php artisan key:generate 2>&1 | tee -a "$LOG_FILE"

    # 创建数据库
    touch database/database.sqlite

    # 运行迁移
    php artisan migrate --force 2>&1 | tee -a "$LOG_FILE"

    # 填充默认管理员
    php artisan db:seed --force 2>&1 | tee -a "$LOG_FILE"

    # 设置权限
    chmod -R 755 storage bootstrap/cache
    chown -R www:www storage database 2>/dev/null || true

    success "环境配置完成"
}

# 配置 Nginx
setup_nginx() {
    info "配置 Nginx..."

    echo ""
    echo -e "${BLUE}请输入域名或 IP（直接回车使用 IP）:${NC}"
    read -r DOMAIN

    if [ -z "$DOMAIN" ]; then
        DOMAIN=$(curl -s ifconfig.me 2>/dev/null || echo "localhost")
        info "使用 IP: $DOMAIN"
    fi

    # 更新 .env APP_URL（在 SSL 判断之后设置）
    # 询问 SSL
    SSL_ENABLED=false
    echo ""
    echo -e "${BLUE}是否开启 SSL？(y/N):${NC}"
    read -r SSL_CHOICE

    if [ "$SSL_CHOICE" = "y" ] || [ "$SSL_CHOICE" = "Y" ]; then
        SSL_ENABLED=true
        info "正在申请 SSL 证书..."

        # 安装 acme.sh
        if [ ! -f "$HOME/.acme.sh/acme.sh" ]; then
            curl https://get.acme.sh | sh 2>&1 | tee -a "$LOG_FILE"
        fi

        # 申请证书
        $HOME/.acme.sh/acme.sh --issue -d "$DOMAIN" --webroot /var/www/html 2>&1 | tee -a "$LOG_FILE" || {
            warn "SSL 申请失败，使用 HTTP"
            SSL_ENABLED=false
        }

        if [ "$SSL_ENABLED" = true ]; then
            mkdir -p /etc/nginx/ssl
            $HOME/.acme.sh/acme.sh --install-cert -d "$DOMAIN" \
                --key-file /etc/nginx/ssl/${DOMAIN}.key \
                --fullchain-file /etc/nginx/ssl/${DOMAIN}.pem 2>&1 | tee -a "$LOG_FILE"
        fi
    fi

    # 根据 SSL 设置 APP_URL
    if [ "$SSL_ENABLED" = true ]; then
        sed -i "s|APP_URL=.*|APP_URL=https://${DOMAIN}|" "$INSTALL_DIR/backend/.env"
    else
        sed -i "s|APP_URL=.*|APP_URL=http://${DOMAIN}|" "$INSTALL_DIR/backend/.env"
    fi

    # 生成 Nginx 配置
    NGINX_CONF="/etc/nginx/conf.d/3xui-hub.conf"

    # 检测 PHP-FPM socket 路径
    if [ -S "/run/php/php8.4-fpm.sock" ]; then
        FPM_SOCK="/run/php/php8.4-fpm.sock"
    elif [ -S "/tmp/php-cgi-84.sock" ]; then
        FPM_SOCK="/tmp/php-cgi-84.sock"
    else
        FPM_SOCK="/run/php/php8.4-fpm.sock"
        warn "未检测到 PHP-FPM socket，使用默认路径"
    fi
    info "PHP-FPM socket: $FPM_SOCK"

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

    location ~ [^/]\.php(/|$) {
        fastcgi_pass unix:${FPM_SOCK};
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name;
    }

    location ~ ^/(api|admin-api) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location / {
        try_files \$uri \$uri/ /index.html;
    }

    location ~* \.(env|git|sqlite) {
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

    location ~ [^/]\.php(/|$) {
        fastcgi_pass unix:${FPM_SOCK};
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name;
    }

    location ~ ^/(api|admin-api) {
        try_files \$uri \$uri/ /index.php?\$query_string;
    }

    location / {
        try_files \$uri \$uri/ /index.html;
    }

    location ~* \.(env|git|sqlite) {
        return 404;
    }
}
NGINX
    fi

    # 复制前端文件到 public
    cp -r "$INSTALL_DIR/frontend/dist/"* "$INSTALL_DIR/backend/public/" 2>/dev/null || true

    # 测试并重载 Nginx
    nginx -t 2>&1 | tee -a "$LOG_FILE" || error_exit "Nginx 配置错误"
    systemctl reload nginx

    success "Nginx 配置完成"
}

# 配置 cron（流量自动同步）
setup_cron() {
    info "配置定时任务（流量自动同步）..."

    CRON_CMD="* * * * * cd ${INSTALL_DIR}/backend && php artisan schedule:run >> /dev/null 2>&1"

    # 检查是否已存在
    if crontab -l 2>/dev/null | grep -q "artisan schedule:run"; then
        success "定时任务已存在"
    else
        (crontab -l 2>/dev/null; echo "$CRON_CMD") | crontab -
        success "定时任务已配置（每分钟检查，每5分钟同步流量）"
    fi
}

# 安装 3hub 命令
install_3hub() {
    info "安装 3hub 管理命令..."

    cp "$INSTALL_DIR/3hub" /usr/local/bin/3hub
    chmod +x /usr/local/bin/3hub

    success "3hub 命令安装完成"
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
    echo -e "  管理命令:  ${BLUE}3hub${NC} 查看所有可用命令"
    echo ""
    echo -e "${GREEN}============================================================${NC}"
}

# 主流程
main() {
    echo -e "${BLUE}"
    echo "  ____  _____ _   _ ____  _   _ _   _ __  __ _____   ____  _   _ "
    echo " / ___|| ____| \ | |  _ \| | | | \ | |  \/  | ____| | __ )| | | |"
    echo " \___ \|  _| |  \| | |_) | | | |  \| | |\/| |  _|   |  _ \| | | |"
    echo "  ___) | |___| |\  |  __/| |_| | |\  | |  | | |___  | |_) | |_| |"
    echo " |____/|_____|_| \_|_|    \___/|_| \_|_|  |_|_____| |____/ \___/ "
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

    # 部署项目
    deploy_project

    # 配置环境
    setup_env

    # 配置 Nginx
    setup_nginx

    # 配置 cron
    setup_cron

    # 安装 3hub 命令
    install_3hub

    # 输出结果
    show_result
}

main "$@"
