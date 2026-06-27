# ControlHub

3x-ui 订阅管理中枢 | 3x-ui Subscription Management Hub

**版本：1.1.0**

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

## 功能特性

### 管理员端

- 仪表盘：用户统计、节点状态
- 用户管理：增删改查、分配套餐、重置流量、续费
- 节点管理：多入站配置、测试连接
- 套餐管理：周期/总量套餐、价格设置
- 订单管理：查看支付订单、搜索
- 安全设置：密码修改、谷歌二步验证（2FA）
- 支付配置：多支付渠道接入
- 数据库备份：导出/导入、差异预览、增量合并/覆盖

### 用户端

- 登录：邮箱密码 / Token 登录
- 注册：邮箱注册（图形验证码）
- 仪表盘：流量统计、订阅地址、节点列表
- 订购订阅：选择套餐、在线支付
- 最近订单：查看已支付订单

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
| 后端 | Laravel 13 + PHP 8.4 + SQLite |
| 前端 | Vue 3 + Vite + Pinia |
| 认证 | Session（管理员）+ Bearer Token（用户） |
| 支付 | MD5 签名对接第三方支付网关 |
| 2FA | Google Authenticator (TOTP) |

## 目录结构

```
3xui-hub/
├── install.sh          # 一键安装脚本
├── 3hub                # 管理命令
├── README.md
├── INSTALL.md
├── backend/
│   ├── app/
│   ├── config/
│   ├── database/
│   ├── public/         # 网站根目录（含前端 dist）
│   ├── routes/
│   ├── storage/
│   └── vendor/
└── frontend/
    └── dist/           # 构建产物
```

## 版本记录

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

**Version: 1.1.0**

## Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

After installation, use `3hub` command to manage the system.

## Features

- User management, node management, plan management
- Payment integration, order management
- Auto traffic sync, auto ban on limit
- Google 2FA, database backup
- 3hub CLI management tool

## Tech Stack

- Backend: Laravel 13 + PHP 8.4 + SQLite
- Frontend: Vue 3 + Vite + Pinia
- Payment: MD5 signed third-party gateway
- 2FA: Google Authenticator (TOTP)

## License

MIT License

<!-- LINKS -->
[version-shield]: https://img.shields.io/badge/version-1.0.0-blue
[version-url]: #
[license-shield]: https://img.shields.io/badge/license-MIT-green
[license-url]: #许可证
[php-shield]: https://img.shields.io/badge/PHP-8.4+-777BB4
[php-url]: https://php.net
[vue-shield]: https://img.shields.io/badge/Vue-3-4FC08D
[vue-url]: https://vuejs.org
