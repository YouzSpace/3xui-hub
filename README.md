# ControlHub

3x-ui 订阅管理中枢 | 3x-ui Subscription Management Hub

**版本：1.0.0**

<!-- PROJECT SHIELDS -->
[![Version][version-shield]][version-url]
[![License][license-shield]][license-url]
[![PHP][php-shield]][php-url]
[![Vue][vue-shield]][vue-url]

<!-- PROJECT LOGO -->
<br />

<p align="center">
  <h3 align="center">ControlHub</h3>
  <p align="center">
    3x-ui 订阅管理中枢 — 集中管理节点、用户、套餐、支付的一站式平台
    <br />
    <br />
    <a href="https://github.com/YouzSpace/3xui-hub/issues">报告Bug</a>
    ·
    <a href="https://github.com/YouzSpace/3xui-hub/issues">提出新特性</a>
  </p>
</p>

---

[English](#english) | 中文

## 目录

- [项目简介](#项目简介)
- [功能特性](#功能特性)
- [技术栈](#技术栈)
- [安装部署](#安装部署)
  - [环境要求](#环境要求)
  - [安装步骤](#安装步骤)
- [目录结构](#目录结构)
- [使用说明](#使用说明)
- [版本记录](#版本记录)
- [作者](#作者)
- [许可证](#许可证)

---

## 项目简介

ControlHub 是一个 3x-ui 面板的订阅管理中枢，提供用户管理、节点管理、套餐管理、支付集成等完整功能。支持多节点统一管理，用户自助购买套餐，自动化流量同步与封禁。

## 功能特性

### 管理员端

- 仪表盘：用户统计、节点状态一览
- 用户管理：增删改查、分配套餐、重置流量、续费
- 节点管理：添加 3x-ui 节点、多入站配置、测试连接
- 套餐管理：周期套餐/总量套餐、价格设置
- 订单管理：查看支付订单、搜索
- 安全设置：修改密码、谷歌二步验证（2FA）
- 支付配置：多支付渠道接入

### 用户端

- 登录：邮箱密码登录 / Token 登录
- 注册：邮箱注册（图形验证码）
- 仪表盘：流量统计、订阅地址、在线节点
- 订购订阅：选择套餐、在线支付
- 最近订单：查看已支付订单

---

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Laravel 13 + PHP 8.4 + SQLite |
| 前端 | Vue 3 + Vite + Pinia + TailwindCSS |
| 认证 | Session（管理员）+ Bearer Token（用户） |
| 支付 | MD5 签名对接第三方支付网关 |
| 2FA | Google Authenticator (TOTP) |

---

## 安装部署

详见 [INSTALL.md](./INSTALL.md)

### 环境要求

- PHP 8.4+
- Composer 2.2+
- PHP 扩展：fileinfo、pdo_sqlite、openssl、mbstring、gd
- Nginx

### 安装步骤

```bash
# 1. 克隆或下载项目
git clone https://github.com/YouzSpace/3xui-hub.git

# 2. 后端依赖
cd backend
composer install --no-dev

# 3. 配置环境
cp .env.example .env
php artisan key:generate

# 4. 初始化数据库
touch database/database.sqlite
php artisan migrate --seed

# 5. 复制前端到 public
cp -r ../frontend/dist/* public/

# 6. 配置 Nginx 后访问
```

默认管理员：`admin` / `admin123`

---

## 目录结构

```
controlhub/
├── backend/                    # Laravel 后端
│   ├── app/
│   │   ├── Http/Controllers/   # 控制器
│   │   │   ├── Admin/          # 管理员 API
│   │   │   └── Api/            # 用户 API
│   │   ├── Models/             # 数据模型
│   │   └── Services/           # 业务服务
│   ├── config/                 # 配置
│   ├── database/
│   │   ├── migrations/         # 数据库迁移
│   │   └── seeders/            # 数据填充
│   ├── public/                 # 网站根目录（含前端 dist）
│   ├── routes/                 # 路由定义
│   └── storage/                # 日志、缓存
├── frontend/                   # Vue3 前端
│   └── dist/                   # 构建产物
└── INSTALL.md                  # 安装文档
```

---

## 使用说明

### 管理员

1. 访问 `/admin/login` 登录
2. 在「节点」中添加 3x-ui 节点
3. 在「套餐」中创建套餐并设置价格
4. 在「设置 → 支付」中配置支付接口
5. 在「用户」中管理用户或等待用户注册

### 用户

1. 访问首页注册账号
2. 登录后查看订阅地址
3. 点击「订购订阅」购买套餐
4. 支付完成后自动分配流量

---

## 版本记录

### v1.0.0 (2026-06-23)

- 初始发布
- 用户管理（注册/登录/邮箱/Token）
- 节点管理（多入站/测试连接/健康检查）
- 套餐管理（周期/总量/价格）
- 支付系统（多渠道/下单/回调/订单）
- 订阅系统（Base64/流量同步/封禁）
- 安全功能（密码修改/谷歌二步验证）
- 搜索功能（用户/订单）

---

## 作者

ControlHub Team

---

## 许可证

MIT License

---

<!-- LINKS -->
[version-shield]: https://img.shields.io/badge/version-1.0.0-blue
[version-url]: #
[license-shield]: https://img.shields.io/badge/license-MIT-green
[license-url]: #许可证
[php-shield]: https://img.shields.io/badge/PHP-8.4+-777BB4
[php-url]: https://php.net
[vue-shield]: https://img.shields.io/badge/Vue-3-4FC08D
[vue-url]: https://vuejs.org

---

# English

# ControlHub

3x-ui Subscription Management Hub — A centralized platform for managing nodes, users, plans, and payments.

**Version: 1.0.0**

## Features

### Admin Panel

- Dashboard: User statistics, node status overview
- User Management: CRUD, plan assignment, traffic reset, renewal
- Node Management: Add 3x-ui nodes, multi-inbound configuration, connection testing
- Plan Management: Period/Total plans, pricing
- Order Management: View payment orders, search
- Security: Password change, Google 2FA
- Payment: Multi-channel payment gateway integration

### User Panel

- Login: Email/Password or Token login
- Registration: Email registration with CAPTCHA
- Dashboard: Traffic stats, subscription URL, online nodes
- Purchase: Select plan, online payment
- Orders: View paid orders

## Tech Stack

| Layer | Technology |
|-------|------------|
| Backend | Laravel 13 + PHP 8.4 + SQLite |
| Frontend | Vue 3 + Vite + Pinia + TailwindCSS |
| Auth | Session (Admin) + Bearer Token (User) |
| Payment | MD5 signed third-party gateway |
| 2FA | Google Authenticator (TOTP) |

## Quick Start

```bash
# Clone
git clone https://github.com/YouzSpace/3xui-hub.git

# Backend
cd backend
composer install --no-dev
cp .env.example .env
php artisan key:generate
touch database/database.sqlite
php artisan migrate --seed

# Frontend
cp -r frontend/dist/* backend/public/

# Configure Nginx and visit
```

Default admin: `admin` / `admin123`

See [INSTALL.md](./INSTALL.md) for detailed instructions.

## License

MIT License
