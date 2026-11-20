# Nginx 层安全配置说明（真实 IP / 面板登录限流 / WAF 建议）

> **面向**：运维、接手者、以及排查「面板被爆破 / 日志里全是 CDN IP / 限流不生效」的人。
> **由谁生成**：`3hub` 脚本（`3hub update` / `3hub fix-nginx`）。本文件与脚本模板必须同步修改。
> **相关代码**：`3hub` 的 `render_real_ip_conf()` / `render_panel_guard_conf()` /
> `render_nginx_conf()` / `_ensure_aux_nginx_files()` / `cmd_cf_realip()`。

---

## 一、涉及哪些文件（谁管、会不会被覆盖）

| 文件 | 内容 | 谁维护 | `3hub update` 后 |
|---|---|---|---|
| `3xui-hub.conf`（宝塔下为 `/www/server/panel/vhost/nginx/…`） | 主站反代 + 两个登录精确 location | **`3hub` 整份重写**（受管文件） | 按模板重写，不要手工改 |
| `zz-cloudflare-realip.conf` | CF 真实 IP 还原（`set_real_ip_from` 等） | `3hub` 受管（独立文件） | 幂等重建 |
| `zz-panel-login-guard.conf` | 登录限流 zone 定义（`limit_req_zone`） | `3hub` 受管（独立文件） | 幂等重建 |
| `3xui-hub-domains.conf` | 额外自定义域名 | `3hub domain add/remove` | 不碰 |

> ⚠️ **主 conf 顶部有「本文件由 3hub 受管，请勿手工编辑」**。手工加的东西下一次
> `3hub update` / `3hub fix-nginx` 就没了。要改配置请改 `3hub` 脚本的模板并提 PR。

### 为什么 real_ip / 限流 zone 要单独放文件

1. `set_real_ip_from` / `real_ip_header` 是 **http/server 级指令**，放 `conf.d` 让整机所有站点
   （面板 + 节点 `/node-ws`）一起受益。
2. `limit_req_zone` 同理，且**主 conf 只「引用」zone**。集中放一处便于审计「到底限了多少」。

---

## 二、Cloudflare 真实 IP 还原

### 为什么必须有

站点挂在 Cloudflare 后面时，nginx 看到的 `$remote_addr` 是 **CF 边缘节点 IP**（如
`104.23.x` / `172.68.x`），而不是访客真实 IP。后果：

- 后端按 IP 的限流（登录 / 发码 / 试码 / 注册）变成「按 CF 节点计数」，
  攻击者**换一个 CF 节点就换了一份额度** → 慢速爆破完全拦不住；
- 审计日志（`operation_logs.ip`）全是 CF IP，**事后无法追查是谁**。

### 配置内容

```nginx
set_real_ip_from 173.245.48.0/20;   # …共 22 条（CF 官方全部 v4+v6 段）
real_ip_header CF-Connecting-IP;
real_ip_recursive on;
```

网段来源：<https://www.cloudflare.com/ips-v4> 与 <https://www.cloudflare.com/ips-v6>。

### 维护

```bash
3hub cf-realip --show      # 看当前有几条、是否已生成
3hub cf-realip             # 用脚本内置清单重建（不联网）
3hub cf-realip --refresh   # 联网拉 CF 最新网段再重建（推荐定期跑）
```

**会过时吗？** CF 会新增网段。过时的后果**不是安全漏洞**——`set_real_ip_from` 只对列出的段
生效，所以未列出的新段只是「退化成按 CF 边缘 IP 计数」，伪造头也不会被采信。
发现日志里冒出新的 `104.x` / `172.x` 时跑一次 `--refresh` 即可。

### 怎么验证生效

```bash
# 从外网访问一次，然后看日志第一条的 IP 是不是你自己的真实公网 IP
curl -s -o /dev/null https://你的域名/api/ping
tail -1 /var/log/nginx/access.log | awk '{print $1}'
```
- ✅ 显示你的真实公网 IP（如 `189.x.x.x`、`223.104.x.x`）= 生效
- ❌ 显示 `104.x` / `172.6x` / `162.158.x` = 没生效，检查 `zz-cloudflare-realip.conf` 是否在
  `nginx -t` 的 include 路径里、以及 CF 是否真的开了橙云代理（灰云直连不需要还原）

---

## 三、面板登录限流（边缘第一层）

### 设计目标

**只护面板登录口，绝不误伤节点 / App / 订阅。**

### 规则

```nginx
limit_req_zone $binary_remote_addr zone=panel_login:10m rate=20r/m;
limit_req_status 429;
```

主 conf 里**只有两个精确 location** 用它：

```nginx
location = /admin-api/login              { limit_req zone=panel_login burst=10 nodelay; … }
location = /admin-api/google2fa/enable   { limit_req zone=panel_login burst=10 nodelay; … }
```

用 **`=` 精确匹配**（优先级最高），所以：

| 路径 | 是否限流 | 为什么 |
|---|---|---|
| `/admin-api/login` | ✅ 限 | 爆破入口 |
| `/admin-api/google2fa/enable` | ✅ 限 | 2FA 绑定的验证码暴力尝试 |
| `/api/node-api/*` | ❌ 不限 | **节点 agent 轮询，限了节点全掉线** |
| `/api/clients/*` | ❌ 不限 | App 拉配置 |
| `/api/sub/*` | ❌ 不限 | 订阅拉取 |
| 其它所有 | ❌ 不限 | — |

参数含义：`20r/m` = 每 IP 每分钟 20 次（等效 3 秒 1 次）；`burst=10` 允许瞬时突发 10 个，
超出的**直接 429**（`nodelay`，不排队）。人工登录远用不到这个量级。

### 与后端的关系（两层叠加，不重复）

| 层 | 位置 | 阈值 | 作用 |
|---|---|---|---|
| 边缘 | nginx `panel_login` | 20 次/分钟/IP | 挡住高频轰炸，省后端资源 |
| 应用 | Go `login_guard`（`handler/admin/login_guard.go`） | 账号 5 次失败锁 15 分钟；IP 30 次/分钟 | 挡住**慢速换用户名**爆破 |

缺任一层都有缺口：只有后端 → 大量请求照样打到 Go；只有边缘 → 慢速爆破（每 IP 每分钟 5 次以内）拦不住。

### 想关掉 / 改阈值

- **关掉**：`HUB_NO_LOGIN_RATELIMIT=1 3hub fix-nginx`（生成时就不插入那两个 location）
- **改阈值**：直接改 `zz-panel-login-guard.conf` 里的 `rate=`，然后 `nginx -t && systemctl reload nginx`
  （注意：下次 `3hub update` 会按模板重写回 20r/m）

---

## 四、⚠️ 已知陷阱（排查必看）

### 1. guard 文件缺失会让 nginx **完全起不来**

实测报错：

```
[emerg] zero size shared memory zone "panel_login"
nginx: configuration file … test failed
```

因为主 conf 引用了 zone，而 zone 定义在 `zz-panel-login-guard.conf`。该文件被误删
（清理脚本、手工 rm、磁盘恢复不全）后，**nginx 重启直接失败 → 整站 502/挂掉**。

**内置自愈**：`3hub fix-nginx` / `3hub update` 开头会跑 `nginx_guard_selfcheck`，
检测到「主 conf 引用了 zone 但定义文件缺失」就自动补回。

**手工应急**（如果 nginx 已经起不来）：

```bash
# 方案 A：补回定义文件
3hub cf-realip   # 会连带重建 guard
# 方案 B：先临时去掉主 conf 里的 limit_req 行，让 nginx 起来，再跑 3hub fix-nginx
sed -i '/limit_req zone=panel_login/d' /etc/nginx/conf.d/3xui-hub.conf
nginx -t && systemctl reload nginx
```

### 2. 不要给 `/api/*` 加限流或 Bot Fight Mode

节点 agent 与 App 的流量是 `Go-http-client/2.0` 等**非浏览器 UA**（实测占面板流量七成）。
一旦开 Cloudflare 的 **Bot Fight Mode** 或给 `/api/*` 加 Challenge/限流，**所有节点和 App 立刻掉线**。
这是本项目最容易踩的坑，切勿尝试。

### 3. `matches`（正则）在 Cloudflare 免费版不可用

写 CF WAF 规则时用 `matches` 会报
`not entitled: the use of operator Matches is not allowed, a Business plan or a WAF Advanced plan is required`。
免费版只能用 `contains` / `eq` / `starts_with` 等，逐项列举。

### 4. Cloudflare 免费版限速的三条硬限制

| 参数 | 免费版限制 |
|---|---|
| `period` | **只能是 10 秒**（不能设 60） |
| `mitigation_timeout` | **只能是 10 秒** |
| `characteristics` | 必须含 `cf.colo.id`（按机房计数） |

所以 CF 边缘限速建议配「每 10 秒 N 次」，而不是「每分钟 N 次」。

---

## 五、Cloudflare 侧建议（本仓库不自动配置，需在 CF 面板/API 操作）

这些是**边缘防护**，与 nginx 无关，但同样影响面板安全。用免费版即可：

| 项 | 建议值 | 说明 |
|---|---|---|
| SSL 模式 | `strict` | 回源也校验证书；源站是 acme 正式证书，可放心 |
| Always Use HTTPS | on | 强制跳转 |
| 最低 TLS | 1.2 | 1.0/1.1 有已知漏洞 |
| HSTS | on（max-age 6 个月，先不加 preload） | 防降级 |
| WAF 自定义规则 | 拦 `.env`/`.git`/`wp-*`/`phpmyadmin`/备份后缀；拦扫描器 UA | 免费 5 条/zone |
| 限速 | 只对 `/admin-api/login`，如 10 秒 5 次 | 免费 1 条/zone |
| **Bot Fight Mode** | **禁开** | 会打死节点与 App（见四.2） |
| Under Attack | 只在真被打时临时开 | 常开会误伤正常用户 |

---

## 六、验收清单（改完 nginx 后逐条过）

```bash
# 1. 配置语法
nginx -t

# 2. 面板正常
curl -s -o /dev/null -w '%{http_code}\n' https://你的域名/            # 期望 200
curl -s -o /dev/null -w '%{http_code}\n' https://你的域名/admin       # 期望 200
curl -s https://你的域名/api/ping                                     # 期望 {"code":0,...}

# 3. ⭐节点不受影响（最关键）
curl -s -o /dev/null -w '%{http_code}\n' -A "Go-http-client/2.0" \
  https://你的域名/api/node-api/config                                # 期望 401（鉴权要求，不是 429/403）

# 4. 登录限流生效（连打 12 次，应出现 429）
for i in $(seq 1 12); do
  curl -s -o /dev/null -w '%{http_code} ' -X POST https://你的域名/admin-api/login \
    -H 'Content-Type: application/json' -d '{"username":"x","password":"y"}'
done; echo

# 5. 真实 IP 还原
curl -s -o /dev/null https://你的域名/api/ping
tail -1 /var/log/nginx/access.log | awk '{print $1}'                  # 期望你的真实公网 IP

# 6. 节点心跳（源站）
journalctl -u controlhub --since "10 min ago" | grep -c api/node-api  # 期望 > 0
```

任意一项不符，先看 `nginx -t` 与 `error.log`，再用 `3hub fix-nginx` 恢复受管模板。

---

## 七、变更记录

| 日期 | 内容 |
|---|---|
| 2026-11 | 把 `install.sh` 已有的 CF real_ip 逻辑同步进 `3hub` 的受管模板（此前 `3hub update` 会丢）；新增面板登录限流（zone + 两个精确 location）；新增 `3hub cf-realip` 子命令；新增 guard 文件缺失自愈 |
