# ControlHub

3x-ui 订阅管理中枢 | 3x-ui Subscription Management Hub

**版本：1.2.1**

<!-- PROJECT SHIELDS -->
[![Version][version-shield]][version-url]
[![License][license-shield]][license-url]
[![PHP][php-shield]][php-url]
[![Vue][vue-shield]][vue-url]

---

[English](#english) | 中文

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

安装完成后使用 `3hub` 命令管理系统。

## Docker 部署

```bash
git clone https://github.com/YouzSpace/3xui-hub.git
cd 3xui-hub/docker
docker-compose up -d
```

访问 http://localhost:8080

常用命令：
```bash
docker-compose logs -f      # 查看日志
docker-compose down          # 停止
docker-compose restart       # 重启
docker-compose up -d --build # 更新并重启
```

## 功能特性

### 管理员端

- 仪表盘：用户统计、节点状态
- 用户管理：增删改查、分配套餐、重置流量、续费
- 节点管理：多入站配置、测试连接
- 套餐管理：周期/总量套餐、价格设置
- 订单管理：查看支付订单、搜索
- 公告管理：发布/编辑/删除公告
- 教程管理：分类分组、Markdown 内容编辑
- 站点配置：自定义站点名称、Logo、公告等
- 邮箱配置：SMTP 设置、邮件模板
- 安全设置：密码修改、谷歌二步验证（2FA）
- 数据库备份：导出/导入、差异预览、增量合并/覆盖

### 用户端

- 登录：邮箱密码 / Token 登录
- 注册：邮箱注册（图形验证码）
- 仪表盘：流量统计、订阅地址、节点列表
- 订购订阅：选择套餐、在线支付
- 最近订单：查看已支付订单
- 公告查看：系统公告
- 教程中心：分类教程、详情阅读
- 反馈链接：问题反馈入口

### 自动化

- 流量自动同步：每5分钟自动同步所有用户流量
- 超限自动封禁：流量/到期自动禁用用户
- 3hub 命令：一键管理系统

## 3hub 管理命令

```bash
3hub status        # 查看系统状态
3hub check-update  # 检测更新
3hub update        # 更新系统
3hub admin-user    # 修改管理员账号
3hub admin-pass    # 修改管理员密码
3hub sync          # 手动同步流量
3hub sync-status   # 查看自动同步状态
3hub backup        # 导出数据库备份
3hub log           # 查看错误日志
3hub restart       # 重启服务
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Laravel 13 + PHP 8.3+ + MySQL |
| 前端 | Vue 3.5 + Vite 8 + Pinia 3 + Tailwind CSS 3 |
| 动画 | GSAP 3 |
| 认证 | Session（管理员）+ Bearer Token（用户） |
| 支付 | MD5 签名对接第三方支付网关 |
| 2FA | Google Authenticator (TOTP) |

## 目录结构

```
3xui-hub/
├── install.sh          # 一键安装脚本
├── uninstall.sh        # 卸载脚本
├── 3hub                # 管理命令
├── README.md
├── INSTALL.md
├── docker/
│   ├── Dockerfile
│   ├── docker-compose.yml
│   ├── nginx.conf
│   ├── php.ini
│   ├── www.conf
│   ├── supervisord.conf
│   └── entrypoint.sh
├── backend/
│   ├── app/
│   │   ├── Console/        # 定时任务
│   │   ├── Drivers/        # 驱动抽象层
│   │   ├── Http/           # 控制器
│   │   ├── Jobs/           # 队列任务
│   │   ├── Models/         # 数据模型
│   │   ├── Providers/      # 服务提供者
│   │   ├── Services/       # 业务服务
│   │   └── Traits/         # 特性
│   ├── config/
│   ├── database/
│   │   └── migrations/     # 数据库迁移
│   ├── public/             # 网站根目录（部署时复制 frontend/dist）
│   ├── resources/
│   ├── routes/
│   │   ├── api.php         # API 路由
│   │   └── web.php         # Web 路由
│   └── storage/
└── frontend/
    └── dist/               # 前端构建产物
        ├── assets/         # JS/CSS
        ├── icons.svg
        ├── images/
        └── index.html
```

## 版本记录

### v1.2.1 (2026-07-01)

- Logo 背景色统一使用 CSS 变量
- 所有 Logo 添加 :has(img) 处理，SVG 图片背景透明
- 右下角显示版本号

### v1.2.0 (2026-07-01)

- 教程发布系统：分类分组、Markdown 内容编辑
- 公告管理：发布/编辑/删除公告
- 用户后台重构：多页导航 + UI 优化
- 邮箱验证注册 + 注册同步 3x-ui
- 站点配置读取修复
- 反馈链接入口

### v1.1.0 (2026-06-27)

- 流量同步性能优化：批量拉取 + 并行请求 + upsert 快照
- 支持万级用户 + 千级节点
- 快照表改为 upsert 模式（每 user+node 只保留1条）

### v1.0.0 (2026-06-24)

- 初始发布
- 用户管理（注册/登录/邮箱/Token）
- 节点管理（多入站/测试连接）
- 套餐管理（周期/总量/价格）
- 支付系统（多渠道/下单/回调/订单）
- 订阅系统（Base64/流量同步/封禁）
- 安全功能（密码修改/谷歌二步验证）
- 备份管理（导出/导入/差异预览）
- 流量自动同步
- 3hub 管理命令
- 一键安装脚本

## 许可证

MIT License

---

# English

# ControlHub

3x-ui Subscription Management Hub — A centralized platform for managing nodes, users, plans, and payments.

**Version: 1.2.1**

## Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

After installation, use `3hub` command to manage the system.

## Docker Deploy

```bash
git clone https://github.com/YouzSpace/3xui-hub.git
cd 3xui-hub/docker
docker-compose up -d
```

Visit http://localhost:8080

Common commands:
```bash
docker-compose logs -f      # View logs
docker-compose down          # Stop
docker-compose restart       # Restart
docker-compose up -d --build # Update and restart
```

## Features

- User management, node management, plan management
- Payment integration, order management
- Announcement and tutorial system
- Site settings, email configuration
- Auto traffic sync, auto ban on limit
- Google 2FA, database backup
- 3hub CLI management tool

## Tech Stack

- Backend: Laravel 13 + PHP 8.3+ + MySQL
- Frontend: Vue 3.5 + Vite 8 + Pinia 3 + Tailwind CSS 3
- Animation: GSAP 3
- Payment: MD5 signed third-party gateway
- 2FA: Google Authenticator (TOTP)

## License

MIT License

<!-- LINKS -->
[version-shield]: https://img.shields.io/badge/version-1.2.1-blue
[version-url]: #
[license-shield]: https://img.shields.io/badge/license-MIT-green
[license-url]: #许可证
[php-shield]: https://img.shields.io/badge/PHP-8.3+-777BB4
[php-url]: https://php.net
[vue-shield]: https://img.shields.io/badge/Vue-3-4FC08D
[vue-url]: https://vuejs.org
