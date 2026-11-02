# 安全策略 / Security Policy

ControlHub（企业级跨境电商网络服务运营平台）承载管理员会话、用户账号、订阅凭据与支付回调，安全问题一律按最高优先级处理。我们欢迎负责任的漏洞披露。

ControlHub is an enterprise cross-border network service platform that handles admin sessions, user accounts, subscription credentials and payment callbacks. Security issues are treated as top priority, and responsible disclosure is welcome.

## 支持范围 / Supported Versions

| 版本 / Version | 是否接收报告 / Accepting reports |
|---|---|
| 2.x 最新发布版 / latest 2.x release | 是 / Yes |
| 更早版本 / Older releases | 否，请先升级 / No — please upgrade first |

我们会先确认漏洞在当前最新版上仍可复现，再评估影响。旧版本不做单独修复，修复进入下一个发布版。

Reports are validated against the latest release first. Older releases do not receive individual fixes; the fix ships in the next release.

## 报告漏洞 / Reporting a Vulnerability

**请不要**在公开 issue、讨论区、社交平台或群里披露未修复的安全问题。

**Please do not** disclose unreported security issues in public issues, discussions, social media or chat groups.

报告方式 / How to report:

- 首选 / preferred：GitHub 私密漏洞报告 —— <https://github.com/YouzSpace/3xui-hub/security/advisories/new>
- 报告请包含 / please include：版本号、复现步骤、影响（越权 / 信息泄漏 / 拒绝服务 / 远程执行等）、可选的修复建议、你的署名方式。
- 报告请包含版本、复现步骤、影响面与联系方式；有 PoC 更好，没有也能受理。

我们会在 7 天内回复确认，并在修复发布后于 Release 说明中致谢（可匿名）。

We reply within 7 days and credit you in the release notes once a fix ships (anonymous credit on request).

## 部署自查清单 / Deployment Checklist

装好之后至少做这几件事——大量被报告的问题其实出在部署方式上：

After installation, at minimum:

1. 首次登录后立刻改掉管理员账号与密码（`3hub admin-user` / `3hub admin-pass`），不要保留默认值。
   Change the default admin account and password immediately after first login.
2. 给管理员账号开启 Google 二步验证（2FA）。
   Enable Google two-factor authentication (2FA) for every admin account.
3. 全站走 HTTPS，不要把管理后台直接暴露在公网明文端口上。
   Serve everything over HTTPS; never expose the admin panel on a plaintext public port.
4. 用防火墙或 Nginx 限制管理后台的来源 IP，不要把服务端口直接对公网开放。
   Restrict admin access by source IP (firewall / Nginx allow-list).
5. 定期 `3hub backup` 并异地保存；跟进 `3hub update` 拿到安全修复。
   Run `3hub backup` regularly, keep copies off-host, and stay current with `3hub update`.
6. 不要把 `.env`、数据库备份、证书私钥、`server/storage/` 提交到公开仓库或发给第三方。
   Never commit `.env`, database dumps, TLS private keys or `server/storage/` to a public repo.
7. 订阅链接等同于账号凭据：泄漏后让用户在用户端「重置订阅」（换 token + uuid），不要只改密码。
   Subscription URLs are credentials — after a leak, use the user-side "reset subscription" action.

## 测试边界 / Scope

在范围内 / in scope：认证与会话、越权访问、注入、订阅 token 泄漏与猜测、支付回调签名绕过、节点 agent 通信鉴权、任意文件读写、升级链路被劫持。

不在范围内 / out of scope：

- 未按上面的清单部署导致的问题（无 HTTPS、默认管理员密码、后台直连公网）
- 以管理员身份、在自己的服务器上主动造成的自伤
- 第三方节点内核（Xray 等）自身的未修复漏洞 —— 请报给上游
- 自动化扫描器的告警，除非附可复现的利用路径
- 理论上成立但没有现实利用条件的猜测

## 我们怎么响应 / What We Do

确认有效后，我们会在私密 advisory 里同步修复计划与版本，修复发布后再公开细节；期间请你保密到发布为止。

Once a report is confirmed we share the fix plan and target release in the private advisory, and publish details only after the fix is released.
