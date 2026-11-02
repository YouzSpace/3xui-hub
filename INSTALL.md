# 3xui-hub 安装指南

## 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | bash
```

安装过程中：
1. 自动下载 Go 后端**预编译二进制**（免编译，按架构自动选 amd64/arm64，三级镜像回退 + sha256 校验）
2. 输入域名或 IP（直接回车使用 IP）
3. 选择是否开启 SSL（y/N）

安装完成后自动显示访问地址和默认账号。

> 前端页面已打包进后端二进制，不需要单独部署前端；Nginx 只做 80/443 → 8080 反代。

## Docker 部署

```bash
cd docker
docker compose up -d
```

## 3hub 管理命令

安装后可使用 `3hub` 命令管理系统：

```bash
3hub status        # 查看系统状态
3hub check-update  # 检测更新
3hub update        # 更新系统（下载新版本预编译二进制 + sha256 校验）
3hub admin-user    # 修改管理员账号
3hub admin-pass    # 修改管理员密码
3hub sync          # 手动同步节点流量
3hub sync-status   # 查看流量同步状态
3hub backup        # 数据库备份
3hub log           # 查看日志（journalctl -u controlhub）
3hub restart       # 重启后端服务
3hub fix-nginx     # 修复 Nginx 反代配置
3hub ssl           # 申请/续期 SSL 证书
```

## 默认账号

- **管理员**: admin / admin123
- 首次登录后请立即修改密码！

## 从源码编译（专业用户）

```bash
# 加 FROM_SOURCE=1 改为源码编译（需联网下载 Go 工具链）
curl -fsSL https://raw.githubusercontent.com/YouzSpace/3xui-hub/main/install.sh | FROM_SOURCE=1 bash
```

## 常见问题

### 安装失败

查看安装日志：`cat /tmp/3xui-hub-install.log`

### 流量不同步

检查同步状态：`3hub sync-status`，必要时手动同步：`3hub sync`

### 节点任务一直 pending / 不执行

```bash
3hub status        # 查看后端服务状态
3hub log           # 查看后端日志（journalctl -u controlhub）
```

### 500 错误

查看日志：`3hub log` 或 `journalctl -u controlhub --no-pager -n 50`

### 忘记密码

```bash
3hub admin-pass
```
