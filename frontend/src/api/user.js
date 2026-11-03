import client from './client'

/**
 * 用户端 API（/api/*，Bearer 鉴权）。
 */
export const userApi = {
  login: (token) => client.post('/api/login', { token }).then((r) => r),
  loginEmail: (email, password) => client.post('/api/login-email', { email, password }).then((r) => r),
  register: (email, password, captcha, emailCode) => {
    const data = { email, password }
    if (emailCode) {
      data.email_code = emailCode
    } else {
      data.captcha = captcha
    }
    return client.post('/api/register', data).then((r) => r)
  },
  captchaUrl: () => `/api/captcha?t=${Date.now()}`,
  me: () => client.get('/api/me'),
  syncTraffic: () => client.post('/api/sync-traffic'),
  syncTaskStatus: () => client.get('/api/sync-traffic/status'),
  nodes: () => client.get('/api/nodes'),
  tutorials: () => client.get('/api/tutorials'),
  tutorial: (id) => client.get(`/api/tutorials/${id}`),
  plans: () => client.get('/api/plans'),

  // 邮箱验证
  sendEmailVerifyCode: (email) => client.post('/api/email-verify/send', { email }).then((r) => r),

  // 找回密码（邮箱验证码）
  sendResetCode: (email) => client.post('/api/password-reset/send', { email }).then((r) => r),
  resetPassword: (email, code, password) => client.post('/api/password-reset/reset', { email, code, password }).then((r) => r),

  // 邀请码 / 兑换码
  myInvite: () => client.get('/api/invite/me'),
  // 我的邀请返佣（返佣域，只读）：data = { rate(0~1), total_rebate(分), invited_count,
  // invitees:[{ user_id, email(已打码), rebate(分), invited_at }] }
  myAffiliate: () => client.get('/api/affiliate'),
  // 仅校验不占用次数，用于输码即时反馈
  checkDiscount: (code, planId) => client.post('/api/discount/check', { code, plan_id: planId }),

  // 账户余额（金额一律「分」，展示用 utils/money 换算）
  // 返回 { balance, logs:[{id, amount, type, note, balance_after, created_at}] }，
  // logs 按 id 倒序；params 支持 { page, per_page }（默认 20，上限 100）。
  balance: (params) => client.get('/api/balance', { params }),
  // 兑换余额码：成功 data={added, balance}；失败后端回 400 + 明确文案
  //（兑换码不存在 / 已使用 / 已过期 / 已作废），失败次数多了会 429。
  redeem: (code) => client.post('/api/redeem', { code }).then((r) => r),

  // 支付
  paymentMethods: () => client.get('/api/payment/methods'),
  createOrder: (planId, paymentConfigId, discountCode) => {
    const data = { plan_id: planId, payment_config_id: paymentConfigId }
    if (discountCode) data.discount_code = discountCode
    return client.post('/api/payment/create', data).then((r) => r)
  },
  // 余额支付套餐：不经过第三方网关，成功即扣款（status 直接是 paid，没有 pay_url），
  // 余额不足时后端回 code 400「余额不足」。
  createBalancePayOrder: (planId, discountCode) => {
    const data = { plan_id: planId, payment: 'balance' }
    if (discountCode) data.discount_code = discountCode
    return client.post('/api/payment/create', data).then((r) => r)
  },
  // 余额充值下单（走第三方支付，回调到账）：amount 单位**分**，建的是 order_type=balance 订单。
  createRechargeOrder: (amountCents, paymentConfigId) => {
    const data = { order_type: 'balance', amount: amountCents }
    if (paymentConfigId) data.payment_config_id = paymentConfigId
    return client.post('/api/payment/create', data).then((r) => r)
  },
  createResetOrder: (paymentConfigId) => client.post('/api/payment/create-reset', { payment_config_id: paymentConfigId }).then((r) => r),
  orderStatus: (orderNo) => client.get('/api/payment/status', { params: { order_no: orderNo } }),
  myOrders: () => client.get('/api/payment/orders'),
  retryPay: (orderNo) => client.post('/api/payment/retry', { order_no: orderNo }).then((r) => r),

  // 网站信息（公开）
  siteConfig: () => client.get('/api/site-config'),

  // 用户自助重置订阅：换 token + uuid（旧订阅链接失效），**不影响当前登录态**。
  // 返回值里有新 token 与 sub_path —— 前端必须用它拼地址，不能复用页面里的旧 token。
  resetSubscription: () => client.post('/api/reset-subscription').then((r) => r),

  // 代理商城（与节点套餐完全独立）
  proxyProducts: () => client.get('/api/proxy-store/products'),
  createProxyOrder: (skuId, paymentConfigId) =>
    client.post('/api/proxy-store/orders', { sku_id: skuId, payment_config_id: paymentConfigId }).then((r) => r),
  proxyEntitlements: () => client.get('/api/proxy-store/entitlements'),
  proxyIps: (id) => client.get(`/api/proxy-store/entitlements/${id}/proxies`),
}
