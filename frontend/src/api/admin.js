import client from './client'

/**
 * Admin API（/admin/*，session cookie 鉴权）。
 */
export const adminApi = {
  login: (username, password, otp) => client.post('/admin-api/login', { username, password, otp }),
  logout: () => client.post('/admin-api/logout'),

  // 用户
  // 列表接口（纯展示层分页，每页 50 条）：params 传 { page } 翻页；config 可带 { signal } 取消上一请求。
  // 响应 data 是当前页数组，分页元信息在数组的 .pagination（{current_page,last_page,total,per_page}）。
  listUsers: (params, config) => client.get('/admin-api/users', { params, ...config }),
  getUser: (id) => client.get(`/admin-api/users/${id}`),
  createUser: (data) => client.post('/admin-api/users', data),
  updateUser: (id, data) => client.put(`/admin-api/users/${id}`, data),
  deleteUser: (id) => client.delete(`/admin-api/users/${id}`),
  resetUserTraffic: (id) => client.post(`/admin-api/users/${id}/reset-traffic`),
  resetUserPassword: (id, password) => client.post(`/admin-api/users/${id}/reset-password`, { password }),
  // 重置订阅：换 token + uuid，旧订阅链接与旧节点凭据立即作废；账号/套餐/流量不动。
  // 响应 data:{id, token, uuid, sub_path}
  resetUserSubscription: (id) => client.post(`/admin-api/users/${id}/reset-subscription`),
  renewUser: (id) => client.post(`/admin-api/users/${id}/renew`),

  // 仪表盘统计：成交额（今日 / 累计，只算已支付订单）
  getRevenueStats: () => client.get('/admin-api/stats/revenue'),

  // 节点
  // 分页同 listUsers；params 传 { all: 1 } 时后端显式返回全量（倍率页/仪表盘用）。
  listNodes: (params, config) => client.get('/admin-api/nodes', { params, ...config }),
  getNode: (id) => client.get(`/admin-api/nodes/${id}`),
  createNode: (data) => client.post('/admin-api/nodes', data),
  updateNode: (id, data) => client.put(`/admin-api/nodes/${id}`, data),
  deleteNode: (id) => client.delete(`/admin-api/nodes/${id}`),
  // 以下两个接口直连节点 3x-ui（同步等待）。账密模式下后端最坏 ≈ 登录 5s + CSRF 2s + 业务 15s ≈ 22s，
  // 超过 client.js 的全局 15s 会误报 timeout，故单独放宽到 30s（同 :84 备份导入的写法，只放宽这两个）。
  testNode: (id) => client.post(`/admin-api/nodes/${id}/test`, {}, { timeout: 30000 }),
  // agent 自升级：只入队指令，节点取走后自己下载/校验/替换/重启（秒级返回，不等节点）
  upgradeNodeAgent: (id) => client.post(`/admin-api/nodes/${id}/upgrade-agent`),
  probeInbounds: (data) => client.post('/admin-api/nodes/probe-inbounds', data, { timeout: 30000 }),
  // 流量倍率：改原始倍率（只影响继承中的节点）/ 开关用户端是否展示倍率。
  // 单节点改倍率走上面的 updateNode，不另开端点。
  setOriginalNodeMultiplier: (multiplier) => client.put('/admin-api/nodes/multiplier/original', { multiplier }),
  setNodeMultiplierDisplay: (showMultiplier) => client.put('/admin-api/nodes/multiplier/display', { show_multiplier: showMultiplier }),

  // 第三方订阅（外部机场线路；链接含机场 token，仅管理端）
  // 列表返回 { items, enabled(总开关) }；新增/编辑会触发一次拉取（失败不阻塞保存），放宽超时
  listThirdParty: () => client.get('/admin-api/nodes/third-party'),
  createThirdParty: (data) => client.post('/admin-api/nodes/third-party', data, { timeout: 60000 }),
  updateThirdParty: (id, data) => client.put(`/admin-api/nodes/third-party/${id}`, data, { timeout: 60000 }),
  deleteThirdParty: (id) => client.delete(`/admin-api/nodes/third-party/${id}`),
  refreshThirdParty: (id) => client.post(`/admin-api/nodes/third-party/${id}/refresh`, {}, { timeout: 60000 }),
  setThirdPartyEnabled: (enabled) => client.put('/admin-api/nodes/third-party/switch', { enabled }),

  // 上游 · Webshare 代理（站长账号 → 子用户凭据）
  listWebshareProviders: () => client.get('/admin-api/upstream/webshare/providers'),
  createWebshareProvider: (data) => client.post('/admin-api/upstream/webshare/providers', data),
  updateWebshareProvider: (id, data) => client.put(`/admin-api/upstream/webshare/providers/${id}`, data),
  deleteWebshareProvider: (id) => client.delete(`/admin-api/upstream/webshare/providers/${id}`),
  // 连上游，慢（15s 超时），单独放宽
  testWebshareProvider: (id) => client.post(`/admin-api/upstream/webshare/providers/${id}/test`, {}, { timeout: 60000 }),
  listWebsharePlans: (id) => client.get(`/admin-api/upstream/webshare/providers/${id}/plans`, { timeout: 60000 }),
  listWebshareAccounts: (id) => client.get(`/admin-api/upstream/webshare/providers/${id}/accounts`),
  // 代理 IP 列表（只回 IP/端口/国家，不回主账号凭据）
  listWebshareProxies: (id, params) => client.get(`/admin-api/upstream/webshare/providers/${id}/proxies`, { params, timeout: 60000 }),
  createWebshareAccount: (id, data) => client.post(`/admin-api/upstream/webshare/providers/${id}/accounts`, data, { timeout: 60000 }),
  // 把上游已有子用户拉回本地（后台直接建过号 / 换机器时用）
  syncWebshareAccounts: (id) => client.post(`/admin-api/upstream/webshare/providers/${id}/accounts/sync`, {}, { timeout: 90000 }),
  disableWebshareAccount: (id) => client.post(`/admin-api/upstream/webshare/accounts/${id}/disable`, {}, { timeout: 60000 }),
  deleteWebshareAccount: (id) => client.delete(`/admin-api/upstream/webshare/accounts/${id}`, { timeout: 60000 }),
  pullWebshareUsage: (id) => client.post(`/admin-api/upstream/webshare/accounts/${id}/usage`, {}, { timeout: 60000 }),
  // 读取本地保存的子用户凭据（上游没有读密码接口；同步来的记录会 404）
  getWebshareCredential: (id) => client.post(`/admin-api/upstream/webshare/accounts/${id}/credential`),
  pullWebshareUsageBatch: (id) => client.post(`/admin-api/upstream/webshare/providers/${id}/usage`, {}, { timeout: 120000 }),

  // 代理运营（谁买了、还剩多少、到期没、待补开）
  proxyOpsOverview: () => client.get('/admin-api/proxy-ops/overview'),
  proxyOpsAccounts: (params) => client.get('/admin-api/proxy-ops/accounts', { params }),
  proxyOpsSocksGroups: (params) => client.get('/admin-api/proxy-ops/socks-groups', { params }),
  proxyOpsPending: (params) => client.get('/admin-api/proxy-ops/pending', { params }),
  proxyOpsPull: (accountId) => client.post(`/admin-api/proxy-ops/accounts/${accountId}/pull`, {}, { timeout: 60000 }),
  proxyOpsDisable: (accountId) => client.post(`/admin-api/proxy-ops/accounts/${accountId}/disable`, {}, { timeout: 60000 }),
  proxyOpsResetPassword: (accountId) => client.post(`/admin-api/proxy-ops/accounts/${accountId}/reset-password`, {}, { timeout: 60000 }),
  proxyOpsAccountProxies: (accountId) => client.get(`/admin-api/proxy-ops/accounts/${accountId}/proxies`, { timeout: 60000 }),
  proxyOpsDeleteAccount: (accountId) => client.delete(`/admin-api/proxy-ops/accounts/${accountId}`, { timeout: 60000 }),
  proxyOpsExtend: (entitlementId, days) => client.post(`/admin-api/proxy-ops/entitlements/${entitlementId}/extend`, { days }),
  proxyOpsMoveSocks: (entitlementId, socksId) => client.post(`/admin-api/proxy-ops/entitlements/${entitlementId}/move-socks`, { socks_id: socksId }),
  proxyOpsRetryOrder: (orderId) => client.post(`/admin-api/proxy-ops/orders/${orderId}/retry`, {}, { timeout: 60000 }),

  // 代理商城 · 商品 / 规格 / 手工 socks 货源（与节点套餐完全独立）
  proxyStoreSettings: () => client.get('/admin-api/proxy-store/settings'),
  updateProxyStoreSettings: (enabled) => client.post('/admin-api/proxy-store/settings', { enabled }),
  listProxyProducts: () => client.get('/admin-api/proxy-store/products'),
  createProxyProduct: (data) => client.post('/admin-api/proxy-store/products', data),
  updateProxyProduct: (id, data) => client.put(`/admin-api/proxy-store/products/${id}`, data),
  deleteProxyProduct: (id) => client.delete(`/admin-api/proxy-store/products/${id}`),
  createProxySku: (productId, data) => client.post(`/admin-api/proxy-store/products/${productId}/skus`, data),
  updateProxySku: (id, data) => client.put(`/admin-api/proxy-store/skus/${id}`, data),
  deleteProxySku: (id) => client.delete(`/admin-api/proxy-store/skus/${id}`),
  listProxySocks: () => client.get('/admin-api/proxy-store/socks'),
  importProxySocks: (text) => client.post('/admin-api/proxy-store/socks/import', { text }),
  updateProxySock: (id, data) => client.put(`/admin-api/proxy-store/socks/${id}`, data),
  deleteProxySock: (id) => client.delete(`/admin-api/proxy-store/socks/${id}`),

  // 套餐
  // 分页同 listUsers；params 传 { all: 1 } 时后端显式返回全量（套餐选择器用）。
  listPlans: (params, config) => client.get('/admin-api/plans', { params, ...config }),
  getPlan: (id) => client.get(`/admin-api/plans/${id}`),
  createPlan: (data) => client.post('/admin-api/plans', data),
  updatePlan: (id, data) => client.put(`/admin-api/plans/${id}`, data),
  deactivatePlan: (id) => client.post(`/admin-api/plans/${id}/deactivate`),
  activatePlan: (id) => client.post(`/admin-api/plans/${id}/activate`),

  // 设置
  getSettings: () => client.get('/admin-api/settings'),
  changePassword: (data) => client.post('/admin-api/change-password', data),
  updateUsername: (data) => client.post('/admin-api/update-username', data),

  // Google 2FA
  google2faGenerate: () => client.post('/admin-api/google2fa/generate'),
  google2faEnable: (otp) => client.post('/admin-api/google2fa/enable', { otp }),
  google2faDisable: (password) => client.post('/admin-api/google2fa/disable', { password }),

  // 支付配置
  listPayments: () => client.get('/admin-api/payments'),
  listPaymentProtocols: () => client.get('/admin-api/payments/protocols'),
  getPayment: (id) => client.get(`/admin-api/payments/${id}`),
  createPayment: (data) => client.post('/admin-api/payments', data),
  updatePayment: (id, data) => client.put(`/admin-api/payments/${id}`, data),
  deletePayment: (id) => client.delete(`/admin-api/payments/${id}`),

  // 订单
  listOrders: (params, config) => client.get('/admin-api/orders', { params, ...config }),

  // 流量同步
  syncTraffic: () => client.post('/admin-api/sync-traffic'),

  // 异步任务
  listAsyncTasks: () => client.get('/admin-api/async-tasks'),
  retryAsyncTask: (id) => client.post(`/admin-api/async-tasks/${id}/retry`),

  // 站点信息
  getSiteSettings: () => client.get('/admin-api/site-settings'),
  updateSiteSettings: (data) => client.put('/admin-api/site-settings', data),

  // 订阅格式配置
  getSubscriptionSettings: () => client.get('/admin-api/subscription-settings'),
  updateSubscriptionSettings: (data) => client.put('/admin-api/subscription-settings', data),

  // App 版本分发（升级规则 + 版本分布）
  getAppVersionSettings: () => client.get('/admin-api/app-version-settings'),
  updateAppVersionSettings: (data) => client.put('/admin-api/app-version-settings', data),
  getAppVersions: () => client.get('/admin-api/app-versions'),

  // 备份
  backupExport: () => client.get('/admin-api/backup/export', { responseType: 'blob' }),
  backupPreview: (formData) => client.post('/admin-api/backup/preview', formData),
  // 导入 = 导库 + 补跑迁移 + 重启队列 worker，几十秒到几分钟都正常，单独放宽到 10 分钟
  // （只放宽这一个请求，client.js 的全局 15 秒是给普通接口的，别动）。
  //
  // siteUrl：留空 = 本机 APP_URL 一个字都不动。只有用户真填了才提交该字段 ——
  // 备份里的旧域名永远不会被后端自动写入，改域名必须走这个显式入口。
  backupImport: (mode, restoreEnv = true, siteUrl = '') => {
    const payload = { mode, restore_env: restoreEnv }
    if (siteUrl) payload.site_url = siteUrl
    return client.post('/admin-api/backup/import', payload, { timeout: 600000 })
  },

  // 教程
  listTutorials: () => client.get('/admin-api/tutorials'),
  createTutorial: (data) => client.post('/admin-api/tutorials', data),
  updateTutorial: (id, data) => client.put(`/admin-api/tutorials/${id}`, data),
  deleteTutorial: (id) => client.delete(`/admin-api/tutorials/${id}`),

  // 优惠码 / 邀请机制
  // 列表：不传 folder 只出未归档码（folder_id=null），folder=id 只出该文件夹内的码；
  // 响应 data.folders 始终是全部文件夹 [{id,name,code_count,matched}]。
  listDiscountCodes: (params) => client.get('/admin-api/discount-codes', { params }),
  createDiscountCodes: (data) => client.post('/admin-api/discount-codes', data),
  updateDiscountCode: (id, data) => client.put(`/admin-api/discount-codes/${id}`, data),
  deleteDiscountCode: (id) => client.delete(`/admin-api/discount-codes/${id}`),
  // 批量删除优惠码：一次请求删多个，按 id 回报成功/失败
  batchDeleteDiscountCodes: (ids) => client.post('/admin-api/discount-codes/batch-delete', { ids }),
  // 文件夹（分组展示用；删除只把码退回未归档，不删码）
  createDiscountCodeFolder: (data) => client.post('/admin-api/discount-code-folders', data),
  updateDiscountCodeFolder: (id, data) => client.put(`/admin-api/discount-code-folders/${id}`, data),
  deleteDiscountCodeFolder: (id, withCodes = false) =>
    client.delete(`/admin-api/discount-code-folders/${id}`, {
      params: withCodes ? { with_codes: 1 } : {},
    }),
  getInviteSettings: () => client.get('/admin-api/invite-settings'),
  updateInviteSettings: (data) => client.put('/admin-api/invite-settings', data),
  // 邀请返佣流水（只读，返佣域）：params { page, per_page }（默认 20/页，上限 100）
  // data = { list:[{ id, inviter_id, inviter_email, invitee_id, invitee_email, order_id, order_no,
  //                 amount(分), rate(0~1), created_at }], total, page, per_page }
  listAffiliateRebates: (params, config) => client.get('/admin-api/affiliate/rebates', { params, ...config }),

  // 余额兑换码（余额域，金额一律「分」）
  // 列表：params { status?, folder?, value_order?, page?, per_page? }
  //   status ∈ active|used|disabled；不传 folder 只出未归档码，folder=id 只出该文件夹的码；
  //   value_order ∈ asc|desc（面值排序，不传=按 id 倒序）
  //   data = { list:[{id,code,name,value,status,used_by,used_at,expires_at,created_by,note,folder_id,created_at}],
  //            total, page, per_page, folders:[{id,name,code_count}] }
  listGiftcards: (params) => client.get('/admin-api/giftcards', { params }),
  // 批量生成：{ count(1-100), value(分), name?, note?, expires_at?, folder_id? | folder_name? }
  // 响应 data = { count, codes:[…] }（codes 是本次真正落库的码）
  createGiftcards: (data) => client.post('/admin-api/giftcards', data),
  // 删除：物理删除（任意状态，含已使用/已作废的码）
  deleteGiftcard: (id) => client.delete(`/admin-api/giftcards/${id}/delete`),
  // 批量删除：{ ids:[…] } → data = { deleted:[…], failed:[{id,reason}] }
  batchDeleteGiftcards: (ids) => client.post('/admin-api/giftcards/batch-delete', { ids }),
  // 兑换码文件夹（分组展示用；删除默认只把码退回未归档，不删码）
  createGiftcardFolder: (data) => client.post('/admin-api/giftcard-folders', data),
  updateGiftcardFolder: (id, data) => client.put(`/admin-api/giftcard-folders/${id}`, data),
  deleteGiftcardFolder: (id, withCodes = false) =>
    client.delete(`/admin-api/giftcard-folders/${id}`, {
      params: withCodes ? { with_codes: 1 } : {},
    }),

  // 用户余额（管理端）：金额一律「分」
  // GET  data = { user_id, balance, logs:[…], total, page, per_page }
  // POST { amount(分，正=加/负=扣，不能为 0), note? } → data = { user_id, amount, balance, log }
  getUserBalance: (id, params) => client.get(`/admin-api/users/${id}/balance`, { params }),
  adjustUserBalance: (id, data) => client.post(`/admin-api/users/${id}/balance`, data),

  // 机器流量监控（采样表只读查询；window 非法后端回退 24h）
  // summary → data:{totals:{up,down,total}, window:{key,start,end}, nodes:[{id,name,up,down,total,cpu_avg,cpu_max,mem_avg,mem_max}]}
  // series  → data:[{node_id,name,points:[{t,up,down}]}, …, {node_id:null,name:'合计',aggregate:true,points:[{t,up,down}]}]
  //           末尾一条是**全机汇总**（同一桶内所有启用节点增量之和，后端算，见 MonitorService::series）
  // 无采样数据的字段是 null（不是 0），前端一律显示占位符。
  monitorSummary: (window) => client.get('/admin-api/monitor/summary', { params: { window } }),
  monitorSeries: (window) => client.get('/admin-api/monitor/series', { params: { window } }),

  // 用户×机器 流量（只读；window 非法后端回退 24h，24h 查小时表 / 7d·30d 查天表）
  // 每个返回 data:{window,start,end,data_start,rows:[…]}
  //   用户榜 rows:[{user_id,email,plan_type,plan_name,plan_months,weighted,up,down}]
  //     weighted = 乘倍率后的字节（套餐口径，与 users.traffic_used 同源；默认排序）
  //     up/down  = 面板侧原始字节（未乘倍率）
  //     plan_type= period / total / null
  //   机器榜 rows:[{node_id,name,weighted,up,down,user_raw,multiplier,nic_up,nic_down,nic_total}]
  //     user_raw = up + down（用户腿原始字节，不乘倍率）
  //     nic_*    = MonitorService::summary 的网卡口径（仅 enabled 节点；窗口内无采样是 null）
  //                —— 与用户腿不同源，只并排对照，不互相换算
  //     multiplier = TrafficSyncService::multiplierFor()（节点倍率，唯一入口）
  //   明细 rows:[{node_id,name,weighted,up,down,multiplier,share}]
  //     share = 占该用户窗口内加权合计的百分比（0–100；分母为 0 时 null）
  //   data_start = 新表最早有数据的那天（业务时区 Y-m-d），没数据时 null
  // 没有数据的用户不会出现在 rows 里；机器榜会列出「只有网卡流量」的机器（用户腿列为 null，缺数据 ≠ 0）。
  userTrafficRanking: (window, limit) =>
    client.get('/admin-api/traffic/user-ranking', { params: { window, limit } }),
  nodeTrafficRanking: (window) => client.get('/admin-api/traffic/node-ranking', { params: { window } }),
  userTrafficNodes: (userId, window) =>
    client.get(`/admin-api/traffic/users/${userId}/nodes`, { params: { window } }),

  // 系统状态（操作日志 + 定时任务 + worker）
  getSystemStatus: () => client.get('/admin-api/system/status'),
  // 操作审计（独立分页，不跟监控轮询一起刷）
  auditLogs: (params) => client.get('/admin-api/audit-logs', { params }),

  // 邮箱
  getEmail: () => client.get('/admin-api/email'),
  saveEmail: (data) => client.put('/admin-api/email', data),
  // account 可选：传池号 id 就用该号发（不传 = 站点默认账号）
  testEmail: (to, account) =>
    client.post('/admin-api/email/test', account ? { to, account } : { to }),
  // SMTP 账号池（多号轮换发信；密码只进不出，列表只回 has_password）
  listSmtpAccounts: () => client.get('/admin-api/email/smtp-accounts'),
  createSmtpAccount: (data) => client.post('/admin-api/email/smtp-accounts', data),
  updateSmtpAccount: (id, data) => client.put(`/admin-api/email/smtp-accounts/${id}`, data),
  deleteSmtpAccount: (id) => client.delete(`/admin-api/email/smtp-accounts/${id}`),
  setSmtpAccountStatus: (id, status) =>
    client.put(`/admin-api/email/smtp-accounts/${id}/status`, { status }),
  resetSmtpAccountToday: (id) => client.put(`/admin-api/email/smtp-accounts/${id}/reset-today`),
  // 自动通知（开关全部默认关闭，管理员自行开启）
  getEmailNotify: () => client.get('/admin-api/email/notify'),
  saveEmailNotify: (data) => client.put('/admin-api/email/notify', data),
  // 批量发信：群发/单发，限速默认关闭
  batchSendEmail: (data) => client.post('/admin-api/email/batch-send', data),
  // 批次进度（前端每 5 秒轮询一次；queued/sending 只统计本批，不含别的批次和定时通知邮件）
  mailBatchProgress: (batchId) =>
    client.get('/admin-api/email/batch-progress', { params: { batch_id: batchId } }),
  getEmailSchedule: () => client.get('/admin-api/email/schedule-mail'),
  saveEmailSchedule: (data) => client.put('/admin-api/email/schedule-mail', data),
  // 发信日志
  getEmailLogs: (params) => client.get('/admin-api/email/logs', { params }),

  // Telegram 机器人（通知渠道）
  getTelegram: () => client.get('/admin-api/telegram'),
  saveTelegram: (data) => client.put('/admin-api/telegram', data),
  testTelegram: () => client.post('/admin-api/telegram/test'),

  // 自定义公开首页（HTML + CSS）
  getHomeCustom: () => client.get('/admin-api/home-custom'),
  saveHomeCustom: (data) => client.put('/admin-api/home-custom', data),
  resetHomeCustom: () => client.post('/admin-api/home-custom/reset'),

  // 管理后台配色（自定义管理端核心颜色，亮/暗两套）
  getPanelColors: () => client.get('/admin-api/panel-colors'),
  savePanelColors: (data) => client.put('/admin-api/panel-colors', data),
  resetPanelColors: () => client.post('/admin-api/panel-colors/reset'),

  // 域名管理
  listDomains: () => client.get('/admin-api/domains'),
  addDomain: (domain) => client.post('/admin-api/domains', { domain }),
  setDomainPrimary: (id) => client.post(`/admin-api/domains/${id}/primary`),
  applyDomain: (id) => client.post(`/admin-api/domains/${id}/apply`),
  renewDomain: (id) => client.post(`/admin-api/domains/${id}/renew`),
  deleteDomain: (id) => client.delete(`/admin-api/domains/${id}`),

  // xray 节点配置：入站
  listXrayInbounds: (nodeId) => client.get(`/admin-api/nodes/${nodeId}/xray-inbounds`),
  createXrayInbound: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-inbounds`, data),
  updateXrayInbound: (nodeId, id, data) => client.put(`/admin-api/nodes/${nodeId}/xray-inbounds/${id}`, data),
  deleteXrayInbound: (nodeId, id) => client.delete(`/admin-api/nodes/${nodeId}/xray-inbounds/${id}`),
  // 批量删除入站：一次请求删多个，按 id 回报成功/失败（对齐 3x-ui 入站列表的多选删除）
  batchDeleteXrayInbounds: (nodeId, ids) => client.post(`/admin-api/nodes/${nodeId}/xray-inbounds/batch-delete`, { ids }),
  generateRealityKeypair: (nodeId) => client.post(`/admin-api/nodes/${nodeId}/xray-inbounds/reality-keypair`),
  // Reality 目标探测：在节点侧跑真实 TLS 握手，同步等回执（≤18s）
  scanRealityTargets: (nodeId, targets) => client.post(
    `/admin-api/nodes/${nodeId}/xray-inbounds/reality-scan`,
    { targets },
    { timeout: 30000 },
  ),
  // HY2 端口跳跃（issue #94）：都是转发给节点 agent 的指令，同步等回执。
  // apply 首次可能要先装 iptables，等待上限比探测长。
  hopProbe: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/hop/probe`, data, { timeout: 60000 }),
  hopApply: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/hop/apply`, data, { timeout: 60000 }),
  hopRemove: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/hop/remove`, data, { timeout: 60000 }),
  hopStatus: (nodeId) => client.post(`/admin-api/nodes/${nodeId}/hop/status`, {}, { timeout: 60000 }),
  // 系统调优 / BBR（issue #94 R4）：转发成 sysctl_apply 指令并同步等回执。
  // 回执里的 applied 是 agent 读 /proc/sys 的实际生效值。
  sysctlApply: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/sysctl`, data, { timeout: 40000 }),
  // TLS 证书库（hysteria 的 TLS 证书引用源）：入站只存证书 id，私钥留库里
  listXrayCertificates: () => client.get('/admin-api/xray-certificates'),
  createXrayCertificate: (data) => client.post('/admin-api/xray-certificates', data),
  deleteXrayCertificate: (id) => client.delete(`/admin-api/xray-certificates/${id}`),
  // 证书库回填 TLS 入站 PEM：单张取回解密后的 cert_pem + key_pem（admin 鉴权）。
  // 列表接口永不回 PEM/私钥，所以回填必须单独请求，且只在用户点按钮时发生。
  // 响应 data 形如 { cert_pem, key_pem }。
  getXrayCertificatePem: (id) => client.get(`/admin-api/xray-certificates/${id}/pem`),
  // 远程签发：面板给裸内核节点下发 cert_issue（域名取节点 host），成功后写入证书库。
  // 入参 { node_id, domain, method: 'http-01' }；响应 data 形如
  // { certificate: { id, name, domain, not_after } }（缺 certificate 时前端按域名从刷新后的库里兜底挑）。
  // 节点侧要真跑 acme.sh（HTTP-01），秒级等不到，放宽到 3 分钟。
  issueXrayCertificate: (data) => client.post('/admin-api/xray-certificates/issue', data, { timeout: 180000 }),

  // xray 节点配置：出站
  listXrayOutbounds: (nodeId) => client.get(`/admin-api/nodes/${nodeId}/xray-outbounds`),
  createXrayOutbound: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-outbounds`, data),
  updateXrayOutbound: (nodeId, id, data) => client.put(`/admin-api/nodes/${nodeId}/xray-outbounds/${id}`, data),
  deleteXrayOutbound: (nodeId, id) => client.delete(`/admin-api/nodes/${nodeId}/xray-outbounds/${id}`),
  importXrayOutbound: (nodeId, link) => client.post(`/admin-api/nodes/${nodeId}/xray-outbounds/import`, { link }),
  // 出站 → 客户端分享链接（只读；wireguard 等无标准格式时后端返回 code!=0）
  shareXrayOutboundLink: (nodeId, id) => client.get(`/admin-api/nodes/${nodeId}/xray-outbounds/${id}/link`),
  testXrayOutbound: (nodeId, id) => client.post(`/admin-api/nodes/${nodeId}/xray-outbounds/${id}/test`, {}, { timeout: 15000 }),

  // xray 节点配置：出站订阅（服务器池）
  listXrayOutboundSubscriptions: (nodeId) => client.get(`/admin-api/nodes/${nodeId}/xray-outbound-subscriptions`),
  createXrayOutboundSubscription: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-outbound-subscriptions`, data, { timeout: 40000 }),
  updateXrayOutboundSubscription: (nodeId, id, data) => client.put(`/admin-api/nodes/${nodeId}/xray-outbound-subscriptions/${id}`, data, { timeout: 40000 }),
  deleteXrayOutboundSubscription: (nodeId, id) => client.delete(`/admin-api/nodes/${nodeId}/xray-outbound-subscriptions/${id}`),
  refreshXrayOutboundSubscription: (nodeId, id) => client.post(`/admin-api/nodes/${nodeId}/xray-outbound-subscriptions/${id}/refresh`, {}, { timeout: 40000 }),

  // xray 节点配置：路由
  listXrayRouting: (nodeId) => client.get(`/admin-api/nodes/${nodeId}/xray-routing`),
  createXrayRoutingRule: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-routing`, data),
  updateXrayRoutingRule: (nodeId, id, data) => client.put(`/admin-api/nodes/${nodeId}/xray-routing/${id}`, data),
  deleteXrayRoutingRule: (nodeId, id) => client.delete(`/admin-api/nodes/${nodeId}/xray-routing/${id}`),
  testXrayRouting: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-routing/test`, data),
  // 路由规则表「延时」列：下发 outbound_test 指令给节点做真实 TCP 握手，面板同步等回执。
  // 节点侧最坏要等满后端 8s 才回「pending」，放宽到 20s 免得被全局 30s 之外的抖动误判。
  testXrayRoutingOutbounds: (nodeId, data) =>
    client.post(`/admin-api/nodes/${nodeId}/xray-routing/outbound-test`, data, { timeout: 20000 }),
  // 负载均衡器 + 基础设置
  createXrayBalancer: (nodeId, data) => client.post(`/admin-api/nodes/${nodeId}/xray-routing/balancers`, data),
  updateXrayBalancer: (nodeId, id, data) => client.put(`/admin-api/nodes/${nodeId}/xray-routing/balancers/${id}`, data),
  deleteXrayBalancer: (nodeId, id) => client.delete(`/admin-api/nodes/${nodeId}/xray-routing/balancers/${id}`),
  updateXrayRoutingSettings: (nodeId, data) => client.put(`/admin-api/nodes/${nodeId}/xray-routing/settings`, data),

  // xray 节点配置：WARP
  getXrayWarp: (nodeId) => client.get(`/admin-api/nodes/${nodeId}/warp`),
  registerXrayWarp: (nodeId) => client.post(`/admin-api/nodes/${nodeId}/warp/register`, {}, { timeout: 40000 }),
  applyXrayWarp: (nodeId) => client.post(`/admin-api/nodes/${nodeId}/warp/apply`),
  rotateXrayWarp: (nodeId) => client.post(`/admin-api/nodes/${nodeId}/warp/rotate`, {}, { timeout: 40000 }),
  updateXrayWarp: (nodeId, data) => client.put(`/admin-api/nodes/${nodeId}/warp`, data),
  deleteXrayWarp: (nodeId) => client.delete(`/admin-api/nodes/${nodeId}/warp`),

  // Geo 数据（geoip.dat / geosite.dat 更新与分发，issue #40）
  // 更新要同步从上游下载两个文件，弱网可能 1~2 分钟，单独放宽超时
  getGeodata: () => client.get('/admin-api/geodata'),
  updateGeodata: () => client.post('/admin-api/geodata/update', {}, { timeout: 600000 }),
  dispatchGeodata: () => client.post('/admin-api/geodata/dispatch'),
}
