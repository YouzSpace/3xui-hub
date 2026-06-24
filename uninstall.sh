#!/bin/bash
# ============================================================
# 3xui-hub 一键卸载脚本
# https://github.com/YouzSpace/3xui-hub
#
# 用法：
#   bash uninstall.sh              # 交互模式（推荐，会弹出菜单让你选）
#   bash uninstall.sh --panel      # 仅卸载面板（保留环境+数据库）
#   bash uninstall.sh --app        # 卸载面板+数据库（保留运行环境）
#   bash uninstall.sh --all        # 完全卸载（面板+数据库+PHP/Nginx/MySQL）
#
# 三种卸载模式说明：
#   --panel : 最轻量。只删面板文件本身，PHP/Nginx/MySQL/数据库都留着，
#             适合想重装面板或临时清理的场景。
#   --app   : 中等。在 --panel 基础上额外删除 controlhub 数据库，
#             运行环境（PHP/Nginx/MySQL）保留，适合换数据/重新初始化。
#   --all   : 最彻底。把安装脚本装的所有东西全部清掉，包括 PHP、Nginx、
#             MySQL/MariaDB、Composer、SSL 证书，服务器恢复到安装前状态。
# ============================================================

# set -u：引用未定义变量时报错（避免 $MODE 之类没赋值就被用导致空判断失误）
# 注意：不用 set -e，因为卸载脚本里很多命令可能失败（如服务已停、包已删），
#       这些都不应该让脚本中断，我们用 || true / 2>/dev/null 来兜底。
set -u

# ------------------------------------------------------------
# 颜色 & 基础输出函数
# 这些 ANSI 转义码用于在终端输出彩色文字，提升可读性
# ------------------------------------------------------------
RED='\033[0;31m'      # 红色：错误/危险操作
GREEN='\033[0;32m'    # 绿色：成功
YELLOW='\033[1;33m'   # 黄色：警告
BLUE='\033[0;34m'     # 蓝色：信息提示
CYAN='\033[0;36m'     # 青色：菜单标题
NC='\033[0m'          # No Color：重置颜色（每个彩色输出后都要接上）

# 四个日志输出函数，统一加前缀标签，方便区分信息级别
info()    { echo -e "${BLUE}[INFO]${NC} $1"; }    # 普通信息
success() { echo -e "${GREEN}[OK]${NC} $1"; }     # 成功提示
warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }  # 警告
error()   { echo -e "${RED}[ERROR]${NC} $1"; }    # 错误

# ------------------------------------------------------------
# 路径常量（与 install.sh 写入的路径完全一致）
# 这些是安装脚本部署时生成的文件/目录，卸载时按图索骥逐一清理
# ------------------------------------------------------------
INSTALL_DIR="/www/wwwroot/3xui-hub"        # 项目根目录（git clone 的位置）
NGINX_CONF="/etc/nginx/conf.d/3xui-hub.conf"  # Nginx 站点配置文件
HUB_BIN="/usr/local/bin/3hub"              # 3hub 管理命令（install.sh 里 cp 过去的）
COMPOSER_BIN="/usr/local/bin/composer"     # Composer 可执行文件
LOG_FILE="/tmp/3xui-hub-uninstall.log"     # 卸载日志文件路径
DB_NAME="controlhub"                       # 面板使用的数据库名

# 运行时动态检测的变量（detect_* 函数会填充）
MODE=""                  # 卸载模式：panel / app / all
PKG_MANAGER=""           # 包管理器：apt 或 yum
PHP_PKGS=()              # 检测到的 PHP 包名数组（用于精确卸载）
MYSQL_SERVICE=""         # MySQL/MariaDB 的 systemd 服务名
NGINX_SSL_DIR="/etc/nginx/ssl"  # SSL 证书目录（install.sh 申请证书时创建）

# ------------------------------------------------------------
# 日志函数
# 所有卸载操作都会追加记录到 LOG_FILE，方便事后排查
# ------------------------------------------------------------
log_init() {
    # 初始化日志文件（覆盖写入），记录本次卸载开始时间
    echo "=== 3xui-hub 卸载日志 $(date) ===" > "$LOG_FILE"
}
log() {
    # 追加一条带时间戳的日志
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1" >> "$LOG_FILE"
}

# ------------------------------------------------------------
# 检测操作系统和包管理器
# 卸载 PHP/Nginx/MySQL 时需要知道用 apt 还是 yum
# ------------------------------------------------------------
detect_os() {
    if [ -f /etc/os-release ]; then
        # 读取 /etc/os-release，获取发行版 ID（如 debian/ubuntu/centos）
        . /etc/os-release
        case "$ID" in
            centos|rhel|almalinux|rocky) PKG_MANAGER="yum" ;;  # RedHat 系用 yum/dnf
            ubuntu|debian)               PKG_MANAGER="apt" ;;  # Debian 系用 apt
            *) PKG_MANAGER="apt" ;;                            # 未知系统默认 apt
        esac
    else
        # 没有 os-release 的老系统，默认 apt
        PKG_MANAGER="apt"
    fi
    log "系统包管理器: $PKG_MANAGER"
}

# ------------------------------------------------------------
# 收集已安装的 PHP 包名
# install.sh 装了一堆 php8.4-* 包，这里逐一检测哪些真的装了，
# 只卸载实际存在的包，避免 apt/yum 报错。
# 同时兼容两种安装方式：Debian 的 php8.4-* 和 CentOS Remi 的 php84-php-*
# ------------------------------------------------------------
detect_php_pkgs() {
    PHP_PKGS=()  # 清空数组
    case "$PKG_MANAGER" in
        apt)
            # Debian/Ubuntu 安装脚本会装这些包（见 install.sh line 207）
            for p in php8.4 php8.4-fpm php8.4-cli php8.4-mbstring \
                     php8.4-gd php8.4-opcache php8.4-pdo php8.4-mysql \
                     php8.4-xml php8.4-zip php8.4-curl; do
                # dpkg -l 列出已装包，"^ii  $p " 表示该包已正确安装
                if dpkg -l 2>/dev/null | grep -q "^ii  $p "; then
                    PHP_PKGS+=("$p")
                fi
            done
            ;;
        yum)
            # CentOS 可能用 module 方式（php-*）或 Remi php84-* 方式，都查一遍
            for p in php php-fpm php-cli php-mbstring php-gd php-opcache \
                     php-pdo php-mysql php-xml php-pecl-zip php-curl \
                     php84-php php84-php-fpm php84-php-cli php84-php-mbstring \
                     php84-php-gd php84-php-opcache php84-php-pdo php84-php-mysql \
                     php84-php-xml php84-php-pecl-zip php84-php-curl; do
                # rpm -q 查询包是否安装
                if rpm -q "$p" &>/dev/null; then
                    PHP_PKGS+=("$p")
                fi
            done
            ;;
    esac
    log "检测到 PHP 包: ${PHP_PKGS[*]}"
}

# ------------------------------------------------------------
# 检测 MySQL/MariaDB 的 systemd 服务名
# 不同发行版/不同数据库服务名不一样（mysql / mysqld / mariadb），
# 必须先检测才能正确 stop/disable
# ------------------------------------------------------------
detect_mysql_service() {
    # 按优先级查询 systemd 单元文件，找到第一个匹配的
    if systemctl list-unit-files 2>/dev/null | grep -q '^mariadb\.service'; then
        MYSQL_SERVICE="mariadb"        # MariaDB（Debian 默认装这个）
    elif systemctl list-unit-files 2>/dev/null | grep -q '^mysql\.service'; then
        MYSQL_SERVICE="mysql"          # MySQL（部分系统）
    elif systemctl list-unit-files 2>/dev/null | grep -q '^mysqld\.service'; then
        MYSQL_SERVICE="mysqld"         # 老版本 MySQL / CentOS
    else
        MYSQL_SERVICE=""               # 没装数据库
    fi
    log "MySQL/MariaDB 服务: ${MYSQL_SERVICE:-未检测到}"
}

# ------------------------------------------------------------
# 停止 PHP-FPM 服务
# 删除项目文件前先停掉 PHP-FPM，避免文件被进程占用导致 rm 失败
# （尤其是 storage/logs 这种被打开的文件句柄）
# ------------------------------------------------------------
stop_php_fpm() {
    info "停止 PHP-FPM 服务..."
    # 遍历可能的服务名，存在的就停（不同安装方式服务名不同）
    for svc in php8.4-fpm php-fpm php84-php-fpm; do
        if systemctl list-unit-files 2>/dev/null | grep -q "^${svc}\.service"; then
            systemctl stop "$svc" 2>/dev/null && log "已停止 $svc" || true
        fi
    done
}

# ------------------------------------------------------------
# 删除 Nginx 站点配置并重载
# 只删 3xui-hub 自己的 conf，不动其他网站配置
# 删完要 reload 让配置生效，否则 Nginx 还会按旧配置转发到已删除的项目
# ------------------------------------------------------------
remove_nginx_conf() {
    if [ -f "$NGINX_CONF" ]; then
        info "删除 Nginx 站点配置: $NGINX_CONF"
        rm -f "$NGINX_CONF"
        log "已删除 $NGINX_CONF"

        # nginx -t 测试配置是否合法，避免残留坏配置导致 reload 失败
        # 进而让整个 Nginx 挂掉（影响其他网站）
        if nginx -t 2>/dev/null; then
            systemctl reload nginx 2>/dev/null && success "Nginx 已重载" || true
        else
            warn "Nginx 配置测试失败，未重载（请手动检查 nginx -t）"
        fi
    else
        info "未找到 Nginx 站点配置，跳过"
    fi
}

# ------------------------------------------------------------
# 删除 SSL 证书（仅 --all 模式调用）
# 清理 install.sh 申请的证书文件 + acme.sh 工具本身
# ------------------------------------------------------------
remove_ssl_certs() {
    # Nginx 证书目录（install.sh 里 mkdir -p /etc/nginx/ssl 创建的）
    if [ -d "$NGINX_SSL_DIR" ]; then
        info "删除 Nginx SSL 证书目录: $NGINX_SSL_DIR"
        rm -rf "$NGINX_SSL_DIR"
        log "已删除 $NGINX_SSL_DIR"
    fi
    # acme.sh 安装在 root 家目录，连工具带签发记录一起删
    if [ -d "$HOME/.acme.sh" ]; then
        info "删除 acme.sh（$HOME/.acme.sh）"
        rm -rf "$HOME/.acme.sh"
        log "已删除 acme.sh"
    fi
}

# ------------------------------------------------------------
# 删除 3hub 管理命令
# install.sh 把项目根的 3hub 脚本 cp 到 /usr/local/bin/
# ------------------------------------------------------------
remove_hub_bin() {
    if [ -f "$HUB_BIN" ]; then
        info "删除管理命令: $HUB_BIN"
        rm -f "$HUB_BIN"
        log "已删除 $HUB_BIN"
    fi
}

# ------------------------------------------------------------
# 清理 cron 定时任务
# install.sh 配了 `* * * * * cd ... && php artisan schedule:run` 用于流量同步
# 只删包含 "3xui-hub" 的行，保留用户其他 cron 任务
# ------------------------------------------------------------
remove_cron() {
    info "清理定时任务..."
    if crontab -l 2>/dev/null | grep -q "3xui-hub"; then
        # 原理：crontab -l 输出当前任务 → grep -v 过滤掉 3xui-hub 行 → 重新写入 crontab
        crontab -l 2>/dev/null | grep -v "3xui-hub" | crontab - 2>/dev/null
        success "已移除 3xui-hub 相关的 cron 任务"
        log "已清理 cron"
    else
        info "未找到相关 cron 任务，跳过"
    fi
}

# ------------------------------------------------------------
# 删除项目目录
# /www/wwwroot/3xui-hub 整个删掉，包括 backend、frontend、.git 等
# ------------------------------------------------------------
remove_project() {
    if [ -d "$INSTALL_DIR" ]; then
        info "删除项目目录: $INSTALL_DIR"
        rm -rf "$INSTALL_DIR"
        log "已删除 $INSTALL_DIR"
        success "项目目录已删除"
    else
        info "项目目录不存在，跳过"
    fi
}

# ------------------------------------------------------------
# 删除数据库（仅 --app / --all 模式）
# 执行 DROP DATABASE，不可恢复，所以单独再问一次 yes
# ------------------------------------------------------------
remove_database() {
    # 先检查 mysql 命令是否存在（--all 模式下 MySQL 可能已被卸载）
    if ! command -v mysql &>/dev/null; then
        warn "mysql 命令不可用，无法删除数据库（如已卸载 MySQL 则无需处理）"
        return 0
    fi

    # 检查数据库是否存在，不存在就跳过（避免无意义提示）
    # 用 USE 尝试切库，失败说明库不存在
    if ! mysql -u root -e "USE \`$DB_NAME\`" 2>/dev/null; then
        info "数据库 $DB_NAME 不存在，跳过"
        return 0
    fi

    # 二次确认：DROP DATABASE 是高危操作
    warn "即将删除数据库: $DB_NAME（此操作不可恢复！）"
    read -r -p "确认删除数据库 $DB_NAME？输入 yes 继续: " CONFIRM
    if [ "$CONFIRM" = "yes" ]; then
        mysql -u root -e "DROP DATABASE IF EXISTS \`$DB_NAME\`;" 2>/dev/null
        success "数据库 $DB_NAME 已删除"
        log "已删除数据库 $DB_NAME"
    else
        warn "已取消删除数据库，保留 $DB_NAME"
    fi
}

# ------------------------------------------------------------
# 卸载 PHP（仅 --all 模式）
# 先检测装了哪些包，列出给用户看，确认后 purge
# purge 会连配置文件一起删，比 remove 更干净
# ------------------------------------------------------------
remove_php() {
    detect_php_pkgs
    if [ ${#PHP_PKGS[@]} -eq 0 ]; then
        info "未检测到相关 PHP 包，跳过"
        return 0
    fi

    # 列出将要卸载的包，让用户心里有数
    warn "即将卸载以下 PHP 包："
    printf "  %s\n" "${PHP_PKGS[@]}"
    read -r -p "确认卸载 PHP？输入 yes 继续: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
        warn "已取消卸载 PHP"
        return 0
    fi

    info "卸载 PHP..."
    case "$PKG_MANAGER" in
        apt)
            # purge：删除包+配置；autoremove：清理不再被需要的依赖
            DEBIAN_FRONTEND=noninteractive apt-get purge -y "${PHP_PKGS[@]}" 2>&1 | tee -a "$LOG_FILE"
            apt-get autoremove -y 2>&1 | tee -a "$LOG_FILE" >/dev/null
            ;;
        yum)
            yum remove -y "${PHP_PKGS[@]}" 2>&1 | tee -a "$LOG_FILE"
            ;;
    esac

    # 清理 sury php 源（Debian 安装脚本 line 201 添加的）
    if [ -f /etc/apt/sources.list.d/php.list ]; then
        info "删除 PHP apt 源: /etc/apt/sources.list.d/php.list"
        rm -f /etc/apt/sources.list.d/php.list
    fi
    # sury 的 GPG 密钥
    if [ -f /etc/apt/trusted.gpg.d/php.gpg ]; then
        rm -f /etc/apt/trusted.gpg.d/php.gpg
    fi

    # Remi 安装残留（CentOS，install.sh line 173-174 创建的符号链接和目录）
    if [ -d /opt/remi/php84 ]; then
        rm -rf /opt/remi/php84
    fi
    # install.sh 手动创建的符号链接（ln -sf 那两行）
    rm -f /usr/bin/php /usr/sbin/php-fpm 2>/dev/null

    success "PHP 已卸载"
    log "PHP 卸载完成"
}

# ------------------------------------------------------------
# 卸载 Nginx（仅 --all 模式）
# ⚠️ 警告：会删除整个 Nginx，影响服务器上所有网站
# 如果这台机器还跑着别的站，强烈建议用 --panel 或 --app 模式
# ------------------------------------------------------------
remove_nginx() {
    if ! command -v nginx &>/dev/null; then
        info "Nginx 未安装，跳过"
        return 0
    fi

    # 高危操作，单独确认
    warn "即将卸载 Nginx（会影响所有托管在该服务器上的网站）"
    read -r -p "确认卸载 Nginx？输入 yes 继续: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
        warn "已取消卸载 Nginx"
        return 0
    fi

    # 先停服务再卸载，避免文件占用
    info "停止并禁用 Nginx..."
    systemctl stop nginx 2>/dev/null || true
    systemctl disable nginx 2>/dev/null || true

    info "卸载 Nginx..."
    case "$PKG_MANAGER" in
        apt)
            # purge nginx 全家桶：nginx（元包）、nginx-common（公共文件）、nginx-full（完整版）
            DEBIAN_FRONTEND=noninteractive apt-get purge -y nginx nginx-common nginx-full 2>&1 | tee -a "$LOG_FILE"
            apt-get autoremove -y 2>&1 | tee -a "$LOG_FILE" >/dev/null
            ;;
        yum)
            yum remove -y nginx 2>&1 | tee -a "$LOG_FILE"
            ;;
    esac

    # 清理 /etc/nginx 配置目录（purge 后可能仍有残留）
    rm -rf /etc/nginx 2>/dev/null
    success "Nginx 已卸载"
    log "Nginx 卸载完成"
}

# ------------------------------------------------------------
# 卸载 MySQL/MariaDB（仅 --all 模式）
# ⚠️ 警告：会删除所有数据库文件，不可恢复
# 自动识别是 MySQL 还是 MariaDB，分别 purge
# ------------------------------------------------------------
remove_mysql() {
    detect_mysql_service
    if [ -z "$MYSQL_SERVICE" ]; then
        info "未检测到 MySQL/MariaDB 服务，跳过"
        return 0
    fi

    # 最高危操作，双重警告
    warn "即将卸载 $MYSQL_SERVICE（所有数据库将丢失！）"
    warn "如有重要数据，请先用面板导出备份再执行此操作"
    read -r -p "确认卸载 $MYSQL_SERVICE？输入 yes 继续: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
        warn "已取消卸载 $MYSQL_SERVICE"
        return 0
    fi

    info "停止并禁用 $MYSQL_SERVICE..."
    systemctl stop "$MYSQL_SERVICE" 2>/dev/null || true
    systemctl disable "$MYSQL_SERVICE" 2>/dev/null || true

    info "卸载 $MYSQL_SERVICE..."
    case "$PKG_MANAGER" in
        apt)
            # 区分 MySQL 和 MariaDB，包名不同
            # install.sh 现在会 fallback 到 mariadb-server，两种都要处理
            if dpkg -l 2>/dev/null | grep -qE '^ii  mysql-server'; then
                # MySQL 路线：purge server + client + common + core
                DEBIAN_FRONTEND=noninteractive apt-get purge -y \
                    mysql-server mysql-server-* mysql-client mysql-client-* \
                    mysql-common mysql-server-core-* 2>&1 | tee -a "$LOG_FILE"
            elif dpkg -l 2>/dev/null | grep -qE '^ii  mariadb-server'; then
                # MariaDB 路线：包名前缀换成 mariadb
                DEBIAN_FRONTEND=noninteractive apt-get purge -y \
                    mariadb-server mariadb-server-* mariadb-client mariadb-client-* \
                    mariadb-common 2>&1 | tee -a "$LOG_FILE"
            fi
            apt-get autoremove -y 2>&1 | tee -a "$LOG_FILE" >/dev/null
            ;;
        yum)
            # CentOS 先试 MySQL，失败再试 MariaDB
            yum remove -y mysql-server mysql mysql-server mysql-devel 2>/dev/null || \
            yum remove -y mariadb-server mariadb mariadb-devel 2>/dev/null || true
            ;;
    esac

    # 清理数据目录和配置（purge 不会删 /var/lib 下的实际数据文件）
    rm -rf /var/lib/mysql 2>/dev/null          # 数据库数据文件
    rm -rf /etc/mysql 2>/dev/null              # 配置目录
    rm -rf /etc/my.cnf 2>/dev/null             # 老版配置文件
    rm -rf /var/log/mysql 2>/dev/null          # 日志
    rm -rf /var/log/mariadb 2>/dev/null        # MariaDB 日志

    # install.sh 可能装过 mysql-apt-config 包（添加 MySQL 官方源用的），一并清掉
    case "$PKG_MANAGER" in
        apt)
            if dpkg -l 2>/dev/null | grep -q 'mysql-apt-config'; then
                DEBIAN_FRONTEND=noninteractive apt-get purge -y mysql-apt-config 2>&1 | tee -a "$LOG_FILE"
            fi
            ;;
    esac

    success "$MYSQL_SERVICE 已卸载"
    log "MySQL/MariaDB 卸载完成"
}

# ------------------------------------------------------------
# 删除 Composer（仅 --all 模式）
# install.sh 把 composer 装到 /usr/local/bin/composer
# Composer 是单文件可执行程序，直接 rm 即可
# ------------------------------------------------------------
remove_composer() {
    if [ -f "$COMPOSER_BIN" ]; then
        info "删除 Composer: $COMPOSER_BIN"
        rm -f "$COMPOSER_BIN"
        log "已删除 Composer"
    fi
}

# ------------------------------------------------------------
# 清理临时文件
# 删掉安装/调试过程中产生的临时文件，保持系统整洁
# ------------------------------------------------------------
cleanup_tmp() {
    info "清理临时文件..."
    rm -f /tmp/3xui-hub-install.log 2>/dev/null   # 安装日志
    rm -f /tmp/test1.sql /tmp/test2.sql 2>/dev/null  # 调试时手动跑 mysqldump 留下的
    rm -f /tmp/mysql-apt-config*.deb 2>/dev/null   # MySQL APT 配置包残留

    # 清理 mysqldump 残留的 nul 文件
    # 历史版本 BackupController.php 用了 Windows 语法 `2>nul`，在 Linux 上会创建名为 "nul" 的文件
    # 已在新版本修复，这里清理历史残留
    find /www/wwwroot -maxdepth 4 -name 'nul' -type f -delete 2>/dev/null || true
    log "已清理临时文件"
}

# ------------------------------------------------------------
# 交互模式：选择卸载模式
# 当用户不带参数运行脚本时调用，弹出菜单让用户选择
# ------------------------------------------------------------
choose_mode() {
    echo ""
    echo -e "${CYAN}请选择卸载模式：${NC}"
    # 用不同颜色区分危险等级：绿=安全，黄=中等，红=危险
    echo -e "  ${GREEN}1)${NC} 仅卸载面板         （保留 PHP/Nginx/MySQL + 数据库）"
    echo -e "  ${YELLOW}2)${NC} 卸载面板 + 数据库   （保留 PHP/Nginx/MySQL 运行环境）"
    echo -e "  ${RED}3)${NC} 完全卸载             （面板 + 数据库 + PHP/Nginx/MySQL 全删）"
    echo -e "  ${BLUE}0)${NC} 退出"
    echo ""
    read -r -p "请输入选项 [1/2/3/0]: " CHOICE

    case "$CHOICE" in
        1) MODE="panel" ;;
        2) MODE="app"   ;;
        3) MODE="all"   ;;
        0) echo "已取消"; exit 0 ;;
        *) error "无效选项"; exit 1 ;;
    esac
}

# ------------------------------------------------------------
# 执行卸载主流程
# 按模式调用对应的清理函数，顺序经过设计：
#   1. 先停服务（释放文件占用）
#   2. 删配置（Nginx conf、3hub 命令、cron）
#   3. 删项目文件
#   4. 按模式决定是否删数据库
#   5. 按模式决定是否删运行环境
#   6. 最后清理临时文件
# ------------------------------------------------------------
do_uninstall() {
    echo ""
    echo -e "${BLUE}============================================================${NC}"
    echo -e "${BLUE}  开始卸载（模式: $MODE）${NC}"
    echo -e "${BLUE}============================================================${NC}"
    echo ""

    # === 所有模式都执行的基础清理 ===
    stop_php_fpm         # 1. 停 PHP-FPM，释放项目文件占用
    remove_nginx_conf    # 2. 删 Nginx 站点配置 + reload
    remove_hub_bin       # 3. 删 /usr/local/bin/3hub 命令
    remove_cron          # 4. 清理 cron 定时任务
    remove_project       # 5. 删 /www/wwwroot/3xui-hub 项目目录

    # === --app / --all：额外删数据库 ===
    if [ "$MODE" = "app" ] || [ "$MODE" = "all" ]; then
        remove_database
    else
        info "保留数据库 $DB_NAME（模式: $MODE）"
    fi

    # === --all：额外删运行环境 ===
    if [ "$MODE" = "all" ]; then
        remove_ssl_certs   # SSL 证书 + acme.sh
        remove_php         # PHP + 扩展 + sury/Remi 源
        remove_nginx       # Nginx 本体 + 配置目录
        remove_mysql       # MySQL/MariaDB + 数据目录
        remove_composer    # Composer 可执行文件
    else
        info "保留运行环境（模式: $MODE）"
    fi

    # === 清理临时文件 ===
    cleanup_tmp
}

# ------------------------------------------------------------
# 显示最终卸载结果
# 告诉用户删了什么、留了什么、日志在哪
# ------------------------------------------------------------
show_result() {
    echo ""
    echo -e "${GREEN}============================================================${NC}"
    echo -e "${GREEN}  卸载完成！${NC}"
    echo -e "${GREEN}============================================================${NC}"
    echo ""
    echo -e "  卸载模式: ${BLUE}$MODE${NC}"
    echo ""
    # 按模式分别列出已删除/已保留的内容，让用户一目了然
    case "$MODE" in
        panel)
            echo -e "  ${GREEN}已删除：${NC}项目文件、Nginx 配置、3hub 命令、cron 任务"
            echo -e "  ${YELLOW}已保留：${NC}PHP、Nginx、MySQL/MariaDB、数据库 $DB_NAME"
            ;;
        app)
            echo -e "  ${GREEN}已删除：${NC}项目文件、Nginx 配置、3hub 命令、cron 任务、数据库 $DB_NAME"
            echo -e "  ${YELLOW}已保留：${NC}PHP、Nginx、MySQL/MariaDB（运行环境）"
            ;;
        all)
            echo -e "  ${GREEN}已删除：${NC}项目、数据库、PHP、Nginx、MySQL/MariaDB、Composer、SSL 证书"
            echo -e "  ${RED}服务器已恢复到安装前状态${NC}"
            ;;
    esac
    echo ""
    echo -e "  卸载日志: ${BLUE}$LOG_FILE${NC}"
    echo -e "  查看命令: ${YELLOW}cat $LOG_FILE${NC}"
    echo ""
    echo -e "${GREEN}============================================================${NC}"
}

# ------------------------------------------------------------
# 命令行参数解析
# 支持 --panel / --app / --all / -h / --help
# 不带参数则返回，由主流程进入交互模式
# ------------------------------------------------------------
parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --panel) MODE="panel" ;;   # 仅卸载面板
            --app)   MODE="app"   ;;   # 卸载面板+数据库
            --all)   MODE="all"   ;;   # 完全卸载
            -h|--help)
                # 帮助信息
                echo "用法: bash uninstall.sh [--panel|--app|--all]"
                echo ""
                echo "  --panel  仅卸载面板（保留环境+数据库）"
                echo "  --app    卸载面板+数据库（保留运行环境）"
                echo "  --all    完全卸载（面板+数据库+PHP/Nginx/MySQL）"
                echo "  无参数    进入交互模式"
                exit 0
                ;;
            *)
                error "未知参数: $1"
                exit 1
                ;;
        esac
        shift  # 处理下一个参数
    done
}

# ------------------------------------------------------------
# 主流程入口
# 顺序：检查root → 初始化日志 → 检测系统 → 解析参数/交互选择
#       → 二次确认 → 执行卸载 → 显示结果
# ------------------------------------------------------------
main() {
    # ASCII Art 横幅（UNINSTALL 字样）
    echo -e "${BLUE}"
    echo "  _   _   _   ___ _   _   ____   ___  _   _ ___ ____  "
    echo " | | | | | | | __| | | | / ___| / _ \\| | | |_ _|  _ \\ "
    echo " | |_| | | | | _|| |_| | \\___ \\| | | | | | || || |_) |"
    echo "  \\___/  |_| |___|\\___/  |___/ |_| |_| |_| |___|  __/ "
    echo "                                                |_|    "
    echo -e "${NC}"
    echo "  3x-ui 订阅管理中枢 · 一键卸载脚本"
    echo ""

    # 卸载操作需要 root 权限（rm 系统目录、systemctl、apt/yum 都要 root）
    if [ "$(id -u)" -ne 0 ]; then
        error "请使用 root 用户运行此脚本 (sudo bash uninstall.sh)"
        exit 1
    fi

    log_init       # 初始化日志文件
    detect_os      # 检测系统包管理器

    # 解析命令行参数；无参数则进入交互模式
    parse_args "$@"
    if [ -z "$MODE" ]; then
        choose_mode
    fi

    # 执行前的最终二次确认，列出将要执行的操作让用户确认
    echo ""
    warn "卸载模式: $MODE"
    case "$MODE" in
        panel) echo "将删除项目文件、Nginx 配置、3hub 命令、cron 任务（保留环境和数据库）" ;;
        app)   echo "将删除项目文件、Nginx 配置、3hub 命令、cron 任务、数据库 $DB_NAME（保留运行环境）" ;;
        all)   echo -e "${RED}将删除项目、数据库、PHP、Nginx、MySQL/MariaDB、Composer、SSL 证书（完全清理）${NC}" ;;
    esac
    echo ""
    read -r -p "确认执行？输入 yes 继续，其他取消: " FINAL_CONFIRM
    if [ "$FINAL_CONFIRM" != "yes" ]; then
        echo "已取消卸载"
        exit 0
    fi

    # 正式执行卸载
    do_uninstall
    show_result

    log "卸载完成，模式: $MODE"
}

# 调用 main，把所有命令行参数透传过去
main "$@"
