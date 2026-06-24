# 3xui-hub 安装指南

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

安装过程中：
1. 自动安装 PHP 8.4、Composer、Nginx
2. 输入域名或 IP（直接回车使用 IP）
3. 选择是否开启 SSL（y/N）

安装完成后自动显示访问地址和默认账号。

## 3hub 管理命令

安装后可使用 `3hub` 命令管理系统：

```bash
3hub status        # 查看系统状态
3hub check-update  # 检测更新
3hub update        # 更新系统
3hub admin-user    # 修改管理员账号
3hub admin-pass    # 修改管理员密码
3hub sync          # 手动同步流量
3hub sync-status   # 查看自动同步状态
3hub backup        # 导出数据库备份
3hub log           # 查看最近错误日志
3hub restart       # 重启 PHP-FPM 和 Nginx
3hub help          # 显示帮助
```

## 默认账号

- **管理员**: admin / admin123
- 首次登录后请立即修改密码！

## 手动安装

如需手动安装，参考以下步骤：

### 环境要求

- PHP 8.4+（扩展：fileinfo、pdo_sqlite、mbstring、gd）
- Composer 2.2+
- Nginx

### 步骤

```bash
# 1. 克隆项目
git clone https://github.com/YouzSpace/3xui-hub.git /www/wwwroot/3xui-hub

# 2. 配置后端
cd /www/wwwroot/3xui-hub/backend
cp .env.example .env
php artisan key:generate
touch database/database.sqlite
php artisan migrate --seed

# 3. 复制前端到 public
cp -r ../frontend/dist/* public/

# 4. 设置权限
chmod -R 755 storage bootstrap/cache
chown -R www:www storage database

# 5. 配置 Nginx（参考下方配置）

# 6. 配置 cron（流量自动同步）
crontab -e
# 添加: * * * * * cd /www/wwwroot/3xui-hub/backend && php artisan schedule:run >> /dev/null 2>&1
```

### Nginx 配置示例

```nginx
server {
    listen 80;
    server_name your-domain.com;
    root /www/wwwroot/3xui-hub/backend/public;
    index index.php index.html;

    location ~ [^/]\.php(/|$) {
        fastcgi_pass unix:/run/php/php8.4-fpm.sock;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    }

    location ~ ^/(api|admin-api) {
        try_files $uri $uri/ /index.php?$query_string;
    }

    location / {
        try_files $uri $uri/ /index.html;
    }

    location ~* \.(env|git|sqlite) {
        return 404;
    }
}
```

## 常见问题

### 安装失败
查看安装日志：`cat /tmp/3xui-hub-install.log`

### 流量不同步
检查 cron 是否配置：`3hub sync-status`

### 500 错误
查看日志：`3hub log` 或 `tail -20 /www/wwwroot/3xui-hub/backend/storage/logs/laravel.log`

### 忘记密码
```bash
3hub admin-pass
```
