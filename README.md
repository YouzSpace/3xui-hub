# ControlHub

3x-ui 订阅管理中枢 | 3x-ui Subscription Management Hub

**版本：1.24.0**

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

- 仪表盘：用户/收入统计、节点状态与实时指标
- 用户管理：增删改查、分配套餐、续费、重置流量、重置密码、切换协议
- 节点管理：3x-ui 面板节点 + xray 裸内核节点（agent 自动部署、自升级、实时指标）
- xray 节点配置：入站（VLESS / VMess / Trojan / Shadowsocks，TCP / WebSocket / gRPC，Reality / TLS）、出站、路由规则、WARP 出口
- Reality 目标检测：在节点上跑真实 TLS 握手测速，挑出可用且最快的伪装目标，支持多选填 serverNames
- 套餐管理：周期/总量套餐、价格设置、节点流量倍率、第三方订阅捆绑
- 订单与支付：多渠道支付网关、订单查询、支付回调
- 优惠码：批量生成、限次限时、邀请返利
- 邮箱配置：SMTP 设置、通知场景开关、批量群发、每月定时发信、发信日志
- 多域名：域名绑定、SSL 申请与自动续期、Nginx 配置助手
- 站点配置：站点名称、Logo、公告、自定义公开首页、后台配色
- 教程管理：分类分组、Markdown 内容编辑
- 系统状态、异步任务日志（失败可重试）
- 数据库备份：导出/导入、差异预览、增量合并/覆盖
- 安全设置：密码修改、谷歌二步验证（2FA）

### 用户端

- 登录：邮箱密码 / Token 登录，找回密码
- 注册：邮箱注册（图形验证码）
- 仪表盘：流量统计、订阅地址与二维码、节点列表、使用文档入口
- 订阅：Clash / Sing-box / Base64 三种格式，节点旗帜与重命名
- 订购订阅：选择套餐、在线支付、流量重置包
- 我的订单：独立页面查看历史订单与支付状态
- 系统公告：铃铛弹窗查看，发布新公告后进入即自动弹出（可「不再提示」）；「我的」页进入全屏公告页
- 使用文档：独立二级页面，按分类内容列表 + 详情阅读
- 邀请好友：邀请码与返利记录
- 反馈链接：问题反馈入口

### 自动化

- 流量自动同步：每5分钟自动同步所有用户流量
- 超量/到期自动关流量：只关 3x-ui 流量、永不封禁账号，用户仍可登录续费
- 自动通知：到期 / 超量 / 即将超量，同一用户同一场景只提醒一次，重新购买或重置流量后恢复提醒
- 队列分流：节点类任务走 `node-ops`、邮件类走 `mail`，都不阻塞封禁/健康检查等定时任务（见 [INSTALL.md 队列与并发](INSTALL.md#队列与并发)）
- 节点 agent 自升级、php-fpm 并发自动调优
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
│   │   ├── Drivers/        # 驱动抽象层（3x-ui / xray）
│   │   ├── Http/           # 控制器
│   │   ├── Jobs/           # 队列任务
│   │   ├── Listeners/      # 事件监听
│   │   ├── Models/         # 数据模型
│   │   ├── Providers/      # 服务提供者
│   │   ├── Services/       # 业务服务
│   │   ├── Traits/         # 特性
│   │   └── WebSocket/      # 节点 WS 长连接服务
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

### v1.24.0 (2026-10-10)

- xray 入站新增 Reality 目标检测：在节点上跑真实 TLS 握手（TLS1.3 / h2 / X25519 / 证书链受信任），
  弹窗表格按「可用 → 延迟」排序，勾选多个填入 serverNames、最快的作为 dest
- 邮件拆独立 `mail` 队列，群发不再堵塞封禁/健康检查等定时任务；发送失败自动释放防重标记并补发
- 邮件通知去重改为「每用户每场景一次」，重新购买 / 重置流量后恢复提醒
- 节点 agent 升级至 1.0.2（面板「节点」页一键升级）

### v1.23.2 (2026-10-10)

- 节点 agent 支持自升级；php-fpm 并发按机器规格自动调优
- 修复节点上报流量超过 2000 条被静默丢弃、订阅生成串行、建号阻塞

### v1.23.1 (2026-10-09)

- 邮箱自动通知改为每自然月一封；无套餐账号不再收信

### v1.23.0 (2026-10-08)

- xray 裸内核节点：agent 自动部署、指令秒级下发、实时指标上报
- 邮箱自动通知修复

### v1.22.0 (2026-10-07)

- 第三方机场订阅；邮箱页改造；管理后台配色

### v1.21.0 (2026-10-05)

- 自定义公开首页；修复套餐有效期超过 2038 年写入失败

### v1.11.0 (2026-09-25)

- 用户端：使用文档独立为全屏二级页面，「我的」页与仪表盘入口均跳该页
- 用户端：弹层改为上下居中，内容超高时弹层内部滚动，去掉拖拽把手
- 用户端：系统公告改为弹窗查看，发布新公告后进入即自动弹出，支持「不再提示」
- 用户端：「我的」页保留全屏公告页入口，公告页去掉重复标题

### v1.7.0 (2026-09-18)

- 新节点接入改为按用户异步初始化，单个用户失败不阻塞其他用户
- 增加初始化任务进度、失败重试和节点启用状态保护
- 修复异步任务超时后的迟到回执，避免任务状态被重新改写
- 修复 3x-ui 客户端更新参数和全量删除语义
- 修复多入站配置比较和空入站校验
- 流量同步继续使用按 email 去重、批量快照和批量流量更新
- 管理员任务列表增加节点接入初始化类型

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

**Version: 1.24.0**

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

- User / node / plan / order management
- Panel nodes (3x-ui) and bare-kernel xray nodes with an auto-upgrading agent
- xray inbounds (VLESS / VMess / Trojan / Shadowsocks, Reality / TLS), outbounds, routing, WARP
- REALITY target scanner: real TLS handshake on the node, pick the fastest usable target
- Multi-channel payment gateway, discount codes, referrals
- Email notifications (expiry / quota), batch sending, monthly scheduled mail, delivery logs
- Multi-domain with SSL issuance & renewal helper
- Clash / Sing-box / Base64 subscriptions
- Auto traffic sync; over-quota users lose traffic only — accounts are never banned
- Queue isolation (`node-ops` / `mail`) so batch work never blocks scheduled jobs
- Google 2FA, database backup & migration
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
[version-shield]: https://img.shields.io/badge/version-1.24.0-blue
[version-url]: #
[license-shield]: https://img.shields.io/badge/license-MIT-green
[license-url]: #许可证
[php-shield]: https://img.shields.io/badge/PHP-8.3+-777BB4
[php-url]: https://php.net
[vue-shield]: https://img.shields.io/badge/Vue-3-4FC08D
[vue-url]: https://vuejs.org
