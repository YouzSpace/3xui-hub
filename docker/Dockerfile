# ============================================
# ControlHub - 单容器部署
# PHP 8.3 + Nginx + MySQL 8.0 + Supervisor
# ============================================

FROM php:8.3-fpm-bookworm

# 系统依赖
RUN apt-get update && apt-get install -y \
    nginx \
    mysql-server \
    supervisor \
    cron \
    curl \
    unzip \
    libpng-dev \
    libjpeg-dev \
    libfreetype6-dev \
    libonig-dev \
    libxml2-dev \
    libzip-dev \
    && rm -rf /var/lib/apt/lists/*

# PHP 扩展
RUN docker-php-ext-configure gd --with-freetype --with-jpeg \
    && docker-php-ext-install -j$(nproc) \
    pdo_mysql \
    mbstring \
    exif \
    pcntl \
    bcmath \
    gd \
    zip \
    xml \
    opcache

# Composer
COPY --from=composer:latest /usr/bin/composer /usr/bin/composer

# 工作目录
WORKDIR /var/www/html

# 复制项目文件
COPY backend/ /var/www/html/backend/
COPY frontend/dist/ /var/www/html/frontend/dist/
COPY docker/ /var/www/docker/
COPY 3hub /usr/local/bin/3hub

# 安装 PHP 依赖
RUN cd /var/www/html/backend && composer install --no-dev --optimize-autoloader --no-interaction

# 复制前端到 public
RUN cp -r /var/www/html/frontend/dist/* /var/www/html/backend/public/

# 配置文件
RUN cp /var/www/docker/php.ini /usr/local/etc/php/conf.d/custom.ini \
    && cp /var/www/docker/www.conf /usr/local/etc/php-fpm.d/www.conf \
    && cp /var/www/docker/nginx.conf /etc/nginx/sites-available/default \
    && cp /var/www/docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf

# 数据目录
RUN mkdir -p /var/lib/mysql /var/run/mysqld /var/run/nginx \
    && chown -R mysql:mysql /var/lib/mysql /var/run/mysqld \
    && chown -R www-data:www-data /var/www/html/backend/storage \
    && chown -R www-data:www-data /var/www/html/backend/bootstrap/cache \
    && chmod 755 /usr/local/bin/3hub

# 环境变量
ENV DB_CONNECTION=mysql \
    DB_HOST=127.0.0.1 \
    DB_PORT=3306 \
    DB_DATABASE=controlhub \
    DB_USERNAME=root \
    DB_PASSWORD= \
    APP_KEY= \
    APP_URL=http://localhost

# 端口
EXPOSE 80

# 入口点
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
