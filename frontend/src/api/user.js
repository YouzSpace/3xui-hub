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
  // 仅校验不占用次数，用于输码即时反馈
  checkDiscount: (code, planId) => client.post('/api/discount/check', { code, plan_id: planId }),

  // 支付
  paymentMethods: () => client.get('/api/payment/methods'),
  createOrder: (planId, paymentConfigId, discountCode) => {
    const data = { plan_id: planId, payment_config_id: paymentConfigId }
    if (discountCode) data.discount_code = discountCode
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
